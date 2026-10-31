package runtime

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/invokr-agent/invokr/capability/approval"
	"github.com/invokr-agent/invokr/capability/syntax"
	"github.com/invokr-agent/invokr/tools/core"
	lspsystem "github.com/invokr-agent/invokr/tools/system/lsp"
	mcpsystem "github.com/invokr-agent/invokr/tools/system/mcp"
	tmuxsystem "github.com/invokr-agent/invokr/tools/system/tmux"
	toolboxsystem "github.com/invokr-agent/invokr/tools/system/toolbox"
	"github.com/rivt-ai/go-inference-router"

	"github.com/invokr-agent/invokr/agent"
	"github.com/invokr-agent/invokr/capability/agentdef"
	"github.com/invokr-agent/invokr/capability/agentprompt"
	"github.com/invokr-agent/invokr/capability/memory"
	"github.com/invokr-agent/invokr/capability/memory/history"
	"github.com/invokr-agent/invokr/capability/memory/recallgate"
	memzerfoo "github.com/invokr-agent/invokr/capability/memory/zerfoo"
	"github.com/invokr-agent/invokr/capability/skills"
	"github.com/invokr-agent/invokr/clients"
	"github.com/invokr-agent/invokr/config"
	"github.com/invokr-agent/invokr/platform/events"
	"github.com/invokr-agent/invokr/platform/logging"
	"github.com/invokr-agent/invokr/platform/turn"
	"github.com/invokr-agent/invokr/tools"

	"go.uber.org/zap"
)

type Runtime struct {
	services Services
	cleanup  func() error
}

type LogSource interface {
	Lines() []string
}

type logSink interface {
	io.Writer
	LogSource
}

type Options struct {
	Policy    *approval.Policy
	LogBuffer logSink
	LogView   bool
	// ModelFlagSet reports that --model was given on the command line, which
	// makes the model explicit even when a router profile pins one.
	ModelFlagSet bool
	// ModelOptional lets commands that never send a turn (sessions, index)
	// start without a served model.
	ModelOptional bool
}

// ChatSessionFunc creates a fresh agent session with its own approval broker.
// sessionID may be "" to generate a new one.
type ChatSessionFunc func(sessionID string) (turn.IdentifiedSession, *approval.Broker, error)

type Services struct {
	App             config.AppConfig
	Logger          *zap.Logger
	Events          events.Bus
	Approvals       *approval.Broker
	Memory          memory.Manager
	Session         turn.IdentifiedSession
	PromptHistory   turn.PromptHistory
	Logs            LogSource
	LogView         bool
	NewChatSession  ChatSessionFunc
	NewAgentSession func() (Services, error)
	Commands        CommandBackend
	Grants          *approval.Store
	// ServerProblem describes why the start-up probe of the model server
	// failed, in user words; empty when it answered. The run still starts,
	// so a server that comes up a moment later just works.
	ServerProblem string
}

func Bootstrap(app config.AppConfig, opts Options) (*Runtime, error) {
	app = app.WithDefaults()

	l, err := newValidatedLogger(app, opts)
	if err != nil {
		return nil, err
	}
	provider, app := newProvider(app, opts, l)
	serverProblem, err := resolveStartModel(&app, opts, provider, l)
	if err != nil {
		return nil, err
	}
	providerContext := agent.NewProviderContext(int64(app.Provider.ContextWindowTokens), app.Provider.Model)
	providerContext.SetLogger(l)
	backgroundCtx, cancelBackground := context.WithCancel(context.Background())
	keepBackground := false
	defer func() {
		if !keepBackground {
			cancelBackground()
		}
	}()

	eventDispatcher, approvalBroker := events.NewOrderedDispatcher(l.Named("events")), approval.NewBroker()
	approvalBroker.Policy = opts.Policy
	opts.Policy.SetOutsideReads(app.Workspace.Reads == config.ReadsAsk)
	zerfooWorker, embeddingProvider := startZerfooWorker(app)
	memoryManager, err := newMemoryManager(app, eventDispatcher, zerfooWorker, provider, providerContext, l)
	if err != nil {
		_ = zerfooWorker.Close()
		return nil, err
	}
	promptHistory, err := history.NewStore(history.Config{Dir: app.Memory.Dir, Cwd: app.Workspace.Root})
	if err != nil {
		_ = memoryManager.Close()
		_ = zerfooWorker.Close()
		return nil, err
	}
	eventDispatcher.Register(memory.NewEventHandler(memoryManager))
	skillManager := skills.NewManagerFromConfig(app, l.Named("skills"))
	layer := toolLayerDeps{
		policy: opts.Policy,
		app:    app, logger: l, provider: provider, dispatcher: eventDispatcher,
		mem: memoryManager, embeddings: embeddingProvider, skills: skillManager,
		recallGate: newRecallGate(app, provider, memoryManager, eventDispatcher, l),
		agentReg:   loadAgentRegistry(app.Agents.Dir, l), providerContext: providerContext, backgroundContext: backgroundCtx,
	}
	toolCaller, promptSyntax := layer.buildToolLayer(approvalBroker)
	session, err := agent.NewSession(layer.sessionDeps("", toolCaller, promptSyntax), app)
	if err != nil {
		return nil, closeRuntimeDeps(err, promptHistory, memoryManager, zerfooWorker)
	}

	newChatSession := layer.chatSessionFactory()
	services := Services{
		App: app, Logger: l, Events: eventDispatcher, Approvals: approvalBroker,
		Memory: memoryManager, Session: session, PromptHistory: promptHistory,
		Logs: opts.LogBuffer, LogView: opts.LogView, NewChatSession: newChatSession,
		Commands:      layer.commandService(session.SessionID(), memoryManager, newChatSession),
		Grants:        newStandingGrants(backgroundCtx, app, session, eventDispatcher, l, opts.Policy),
		ServerProblem: serverProblem,
	}
	services.NewAgentSession = func() (Services, error) {
		freshLayer, next := layer, services
		var err error
		next.Session, next.Approvals, err = freshLayer.newSession("")
		if err != nil {
			return Services{}, err
		}
		next.Commands = freshLayer.commandService(next.Session.SessionID(), memoryManager, newChatSession)
		next.Grants = newStandingGrants(backgroundCtx, app, next.Session, eventDispatcher, l, opts.Policy)
		return next, nil
	}
	if err := validateServices(services); err != nil {
		return nil, closeRuntimeDeps(err, promptHistory, memoryManager, zerfooWorker)
	}

	keepBackground = true
	return &Runtime{services: services, cleanup: runtimeCleanup(cancelBackground, promptHistory, memoryManager, zerfooWorker)}, nil
}

func runtimeCleanup(cancel context.CancelFunc, promptHistory interface{ Close() error }, memoryManager memory.Manager, worker *memzerfoo.Worker) func() error {
	return func() error {
		cancel()
		mcpsystem.ShutdownClients()
		lspsystem.ShutdownServers()
		toolboxsystem.ShutdownDaemons()
		tmuxsystem.ShutdownServers()
		return closeRuntimeDeps(nil, promptHistory, memoryManager, worker)
	}
}

// Config validation needs the logger for its warn-only findings (missing dirs).
func newValidatedLogger(app config.AppConfig, opts Options) (*zap.Logger, error) {
	l, err := logging.NewWithSink(app.Log, opts.LogBuffer)
	if err != nil {
		return nil, err
	}
	if err := app.Validate(l.Named("config")); err != nil {
		return nil, err
	}
	// Keep dependency stdlib/slog output from corrupting the interactive TUI.
	zap.RedirectStdLog(l.Named("stdlog"))
	return l, nil
}

func newProvider(app config.AppConfig, opts Options, l *zap.Logger) (*clients.Client, config.AppConfig) {
	var provider *clients.Client
	spec := routerSpec(app.Provider.Router)
	if app.Provider.Router.Enabled() {
		provider = clients.NewRouterClient(spec, app.Provider.HTTPTimeout, app.Provider.MaxRetries)
	} else {
		provider = clients.NewClientWithSettings(app.Provider.BaseURL, app.Provider.APIKey, app.Provider.HTTPTimeout, app.Provider.MaxRetries)
	}
	provider.SetStallTimeout(app.Provider.StallTimeout)
	source := explicitModelSource(opts)
	if app.Provider.Router.Enabled() {
		// The active profile pins the model unless the run asked for one
		// outright: an explicit --model/INVOKR_MODEL must reach the provider
		// (the router registers a profile for it on the fly) rather than be
		// silently replaced by the profile pin.
		if id, ok := spec.ActiveProfile(); ok {
			profile := spec.Profiles[id]
			if source == "" {
				app.Provider.Model, source = profile.Model, "profile"
			}
			app.Agent = withProfileSampling(app.Agent, app.Provider.Router.Providers[profile.Provider])
			l.Info("routing through provider profile",
				zap.String("profile", id), zap.String("provider", profile.Provider), zap.String("model", profile.Model),
				zap.Bool("temperature_disabled", app.Agent.TemperatureDisabled))
		}
	}
	l.Info("model resolved",
		zap.String("model", app.Provider.Model), zap.String("source", cmp.Or(source, "config")))
	return provider, app
}

// explicitModelSource reports how the run named a model outright, or "" when
// it did not. An explicit model wins over a router profile pin and over the
// provider's model list, so a run cannot report one model and call another.
func explicitModelSource(opts Options) string {
	switch {
	case opts.ModelFlagSet:
		return "flag"
	case config.Env("INVOKR_MODEL") != "":
		return "env"
	}
	return ""
}

// samplingFreeProviderTypes are provider types known to reject a request
// carrying a sampling temperature at all. Anthropic removed the parameter on
// its newest models; the openai-compatible driver (llama.cpp and friends) still
// expects it, which is why this is decided per profile rather than globally.
//
// Any other provider can opt in with a "temperature_disabled" provider option
// rather than waiting for an entry here, since which models reject the
// parameter changes faster than this list can track.
var samplingFreeProviderTypes = map[string]bool{"anthropic": true}

// withProfileSampling turns off the sampling temperature for a provider that
// rejects it. A user who set temperature_disabled themselves keeps it off for
// every profile — the flag only ever turns off, so switching to a local profile
// still honors an explicit opt-out.
func withProfileSampling(agent config.AgentConfig, provider config.RouterProviderConfig) config.AgentConfig {
	disabled := samplingFreeProviderTypes[provider.Type]
	if opted, ok := provider.Options["temperature_disabled"].(bool); ok {
		// An explicit provider option wins over the built-in list, so a
		// provider that starts or stops rejecting the parameter is a config
		// edit rather than a release.
		disabled = opted
	}
	agent.TemperatureDisabled = agent.TemperatureDisabled || disabled
	return agent
}

func routerSpec(cfg config.RouterConfig) clients.RouterSpec {
	spec := clients.RouterSpec{
		Active: cfg.Active, Providers: make(map[string]clients.RouterProvider, len(cfg.Providers)),
		Profiles: make(map[string]clients.RouterProfile, len(cfg.Profiles)),
	}
	for id, provider := range cfg.Providers {
		spec.Providers[id] = clients.RouterProvider(provider)
	}
	for id, profile := range cfg.Profiles {
		spec.Profiles[id] = clients.RouterProfile(profile)
	}
	return spec
}

// resolveStartModel settles the model before the session exists so turn 1
// already uses a served one. An explicitly requested model is never swapped
// for whatever the provider lists: the run fails loudly instead. The probe's
// failure comes back too, in user words, for the start-up notice.
func resolveStartModel(app *config.AppConfig, opts Options, provider *clients.Client, l *zap.Logger) (string, error) {
	if explicitModelSource(opts) != "" {
		return "", nil
	}
	var probeErr error
	app.Provider.Model, probeErr = resolveModel(context.Background(), provider, app.Provider.Model, l.Named("runtime"))
	problem := ""
	if probeErr != nil {
		problem = clients.DescribeError(probeErr)
	}
	switch {
	case app.Provider.Model != "" || opts.ModelOptional:
		return problem, nil
	case probeErr != nil:
		return "", fmt.Errorf("no model configured, and the model server could not be asked for one. %s", problem)
	}
	return "", errors.New("no model configured; run invokr setup or pass --model")
}

// resolveModel also returns the probe's failure, so the caller can tell the
// user the server is not answering before the first prompt does.
func resolveModel(ctx context.Context, provider *clients.Client, configured string, log *zap.Logger) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	models, err := provider.ListModels(ctx)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return configured, errors.New("the model server did not answer within 5s at start-up; it may still be loading")
	case inference.IsKind(err, inference.KindTransport):
		return configured, err // nothing answered at all: worth saying up front
	default:
		return configured, nil // no list endpoint (404) or a server error: trust config
	}
	if len(models) == 0 || slices.Contains(models, configured) {
		return configured, nil
	}
	if configured != "" { // none configured is the default, not a fault
		log.Warn("configured model unavailable, falling back",
			zap.String("configured", configured), zap.String("fallback", models[0]))
	}
	return models[0], nil
}

// Models load lazily on first use, so this does not download or load anything.
func startZerfooWorker(app config.AppConfig) (*memzerfoo.Worker, core.EmbeddingProvider) {
	if !app.Memory.Zerfoo.Enabled {
		return nil, nil
	}
	worker := memzerfoo.NewWorker(app.Memory.Zerfoo)
	return worker, worker
}

// newRecallGate builds the optional nothink recall gate. Returns a nil
// interface (feature off) unless enabled in config; subagents skip the gate
// (see subagent.go). An empty gate model falls back to the provider model,
// mirroring chatSummarizer.
func newRecallGate(app config.AppConfig, provider *clients.Client, mem memory.Manager, dispatcher events.Dispatcher, logger *zap.Logger) agentprompt.RecallGate {
	if !app.Memory.RecallGate.Enabled {
		return nil
	}
	cfg := app.Memory.RecallGate
	if strings.TrimSpace(cfg.Model) == "" {
		cfg.Model = app.Provider.Model
	}
	return recallgate.New(cfg, provider, mem, dispatcher, logger.Named("recallgate"))
}

func newMemoryManager(app config.AppConfig, dispatcher events.Dispatcher, worker *memzerfoo.Worker, chat memory.ChatSummarizer, models memory.ModelSource, logger *zap.Logger) (memory.Manager, error) {
	// Pass an untyped nil worker when zerfoo is off so the manager sees a true
	// nil interface (a typed (*Worker)(nil) would read as non-nil).
	if worker == nil {
		return memory.NewManagerFromConfigWithProvider(app, dispatcher, nil, chat, models, logger.Named("memory"))
	}
	return memory.NewManagerFromConfigWithProvider(app, dispatcher, worker, chat, models, logger.Named("memory"))
}

func closeRuntimeDeps(cause error, promptHistory interface{ Close() error }, memoryManager memory.Manager, worker *memzerfoo.Worker) error {
	return errors.Join(cause, promptHistory.Close(), memoryManager.Close(), worker.Close())
}

// toolLayerDeps bundles the shared dependencies for building a tool caller,
// subagent runner, and prompt syntax for a session.
type toolLayerDeps struct {
	policy            *approval.Policy
	app               config.AppConfig
	logger            *zap.Logger
	provider          clients.LLMProvider
	dispatcher        events.Dispatcher
	mem               memory.Manager
	embeddings        core.EmbeddingProvider
	skills            skills.Manager
	agentReg          agentdef.Registry
	caller            *tools.Caller
	providerContext   *agent.ProviderContext
	recallGate        agentprompt.RecallGate
	backgroundContext context.Context
}

func (d *toolLayerDeps) commandService(sessionID string, mem memory.Manager, newChatSession ChatSessionFunc) *CommandService {
	return &CommandService{
		app: d.app, tools: d.caller, skills: d.skills, agents: d.agentReg, memory: mem,
		newSession: newChatSession, events: d.dispatcher, sessionID: sessionID, running: map[string]struct{}{}, logger: d.logger,
	}
}

func (d *toolLayerDeps) buildToolLayer(approver *approval.Broker) (*tools.Caller, syntax.PromptSyntax) {
	subagents := &subagentRunner{}
	caller := tools.NewCaller(
		core.CallerDeps{
			Policy: approver.Policy,
			App:    d.app, Memory: d.mem, Embeddings: d.embeddings, Subagents: subagents,
			SkillManager: d.skills, Dispatcher: d.dispatcher, Approver: approver, AgentDefs: d.agentReg,
		},
		d.logger.Named("tools"),
	)
	logUnknownAgentTools(d.logger, d.agentReg, caller)
	d.caller = caller
	promptSyntax := syntax.NewTaggedJSONSyntax()
	*subagents = subagentRunner{
		logger: d.logger, provider: d.provider, events: d.dispatcher, skills: d.skills, tools: caller,
		syntax: promptSyntax, app: d.app, agentDefs: d.agentReg,
	}
	return caller, promptSyntax
}

func loadAgentRegistry(dir string, l *zap.Logger) agentdef.Registry {
	reg, err := agentdef.NewRegistryFromDir(dir)
	if err != nil {
		l.Warn("agent definitions load error", zap.Error(err))
		return agentdef.EmptyRegistry()
	}
	return reg
}

// logUnknownAgentTools performs a best-effort startup audit of agent definition
// tool allowlists against the static tool set. Advisory only; never fatal.
func logUnknownAgentTools(l *zap.Logger, reg agentdef.Registry, caller *tools.Caller) {
	live := toolNames(caller.Specs())
	for _, name := range reg.Names() {
		d, _ := reg.Get(name)
		if !d.ToolsExplicit {
			continue
		}
		for _, tool := range d.Tools {
			if !slices.Contains(live, tool) {
				l.Warn("agent definition references unregistered tool",
					zap.String("agent", name), zap.String("tool", tool))
			}
		}
	}
}

func (r *Runtime) Services() Services {
	if r == nil {
		return Services{}
	}
	return r.services
}

func (r *Runtime) Cleanup() error {
	if r == nil || r.cleanup == nil {
		return nil
	}
	err := r.cleanup()
	r.cleanup = nil
	return err
}

// newStandingGrants builds the standing rule store (grants and denials), prunes
// expired rules, excludes the tools no grant may ever cover, and wires the audit
// callback. Any load failure (including a corrupt file) degrades to "no rules"
// rather than to a permissive policy.
func newStandingGrants(ctx context.Context, app config.AppConfig, session turn.IdentifiedSession, dispatcher events.Dispatcher, l *zap.Logger, policy *approval.Policy) *approval.Store {
	if app.Approvals.StandingGrantsDisabled {
		return nil
	}
	store := approval.NewStore(app.Approvals.GrantsFile)
	store.SetWorkspace(app.Workspace.Root)
	// The exclusion list is static (approval.alwaysExcluded): tools that author
	// or delegate new capability, plus the sandboxed code runner. It is not
	// derived from tool capabilities — that made `shell`, the one tool standing
	// grants exist for, silently ungrantable while `code_interpreter` slipped
	// through whenever its container network was off.
	if err := store.Load(); err != nil {
		if policy != nil {
			policy.SetStore(nil, err)
		}
		// Every rule in the file is dropped, denials included — so a standing
		// denial the user set stops denying. Approvals fall back to prompting
		// (ask mode) rather than to allowing, which is the safe direction, but
		// a dropped denial must never pass as a routine warning: name the file
		// and the remedy at Error level.
		l.Error("standing rules unavailable: ALL standing grants AND DENIALS dropped; every affected call falls back to an approval prompt until the file is repaired or removed",
			zap.String("grants_file", app.Approvals.GrantsFile), zap.Error(err))
		return nil
	}
	if policy != nil {
		policy.SetStore(store, nil)
	}
	store.SetAuditor(func(g approval.Grant, req approval.Request, event string) {
		action, outcome := "approval:standing_grant", "approved"
		if g.Deny {
			action, outcome = "approval:standing_denial", "denied"
		}
		if event == "created" {
			outcome = "created"
		}
		dispatcher.Dispatch(ctx, events.Event{Type: events.EventAudit, Audit: &events.AuditPayload{
			Action: action, Tool: req.Tool, Outcome: outcome,
			Details: map[string]any{"grant_id": g.ID, "pattern": g.ArgPattern, "scope": g.Scope, "subject": req.Subject, "expires_at": g.ExpiresAt},
		}})
	})
	return store
}

func validateServices(s Services) error {
	switch {
	case s.Logger == nil:
		return fmt.Errorf("runtime logger is required")
	case s.Events == nil:
		return fmt.Errorf("runtime event bus is required")
	case s.Session == nil:
		return fmt.Errorf("runtime agent session is required")
	case s.PromptHistory == nil:
		return fmt.Errorf("runtime prompt history is required")
	default:
		return nil
	}
}

func (d toolLayerDeps) sessionDeps(sessionID string, caller *tools.Caller, promptSyntax syntax.PromptSyntax) agent.SessionDeps {
	return agent.SessionDeps{
		// Named so --log-filter agent.session=<level> (the flag's own help
		// example) actually scopes something: every logger the agent session
		// and its turn loop derive from this one carries the name.
		Logger:            d.logger.Named("agent.session"),
		Provider:          d.provider,
		ProviderContext:   d.providerContext,
		Events:            d.dispatcher,
		Memory:            d.mem,
		RecallGate:        d.recallGate,
		SessionID:         sessionID,
		Skills:            d.skills,
		Tools:             caller,
		Syntax:            promptSyntax,
		BackgroundContext: d.backgroundContext,
	}
}

// chatSessionFactory returns a ChatSessionFunc that builds a fresh session
// with its own approval broker and policy, sharing the runtime tool-layer
// dependencies: one chat's /mode must not change another's.
func (d toolLayerDeps) chatSessionFactory() ChatSessionFunc {
	return func(sessionID string) (turn.IdentifiedSession, *approval.Broker, error) {
		fresh := d
		fresh.providerContext, fresh.recallGate = nil, nil // Keep chat sessions independent of CLI-only context.
		fresh.policy = d.policy.Clone()
		return fresh.newSession(sessionID)
	}
}

func (d *toolLayerDeps) newSession(sessionID string) (turn.IdentifiedSession, *approval.Broker, error) {
	broker := approval.NewBroker()
	broker.Policy = d.policy
	caller, promptSyntax := d.buildToolLayer(broker)
	sess, err := agent.NewSession(d.sessionDeps(sessionID, caller, promptSyntax), d.app)
	if err != nil {
		caller.Close()
		return nil, nil, fmt.Errorf("runtime: new chat session: %w", err)
	}
	return sess, broker, nil
}
