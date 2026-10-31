package acpagent

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/invokr-agent/acp"

	"github.com/invokr-agent/invokr/capability/approval"
	"github.com/invokr-agent/invokr/constants"
	"github.com/invokr-agent/invokr/platform/runtime"
	"github.com/invokr-agent/invokr/platform/turn"
	"github.com/invokr-agent/invokr/platform/version"
)

var permissionOptions = []acp.PermissionOption{
	{OptionID: "allow", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
	{OptionID: "deny", Name: "Deny", Kind: acp.PermissionOptionKindRejectOnce},
}

type chatSession struct {
	turn.IdentifiedSession
	approvals *approval.Broker
	turnMu    sync.Mutex
	noticed   bool     // the update notice was sent in this session; guarded by turnMu
	models    []string // listed at session/new; read-only after
}

// Commands are the slash commands a chat runs without the model.
type Commands interface {
	Available() []acp.AvailableCommand
	// Run answers a command with reply, or turns it into prompt for the
	// model; handled is false for text that is not a command.
	Run(ctx context.Context, sess turn.Session, text string) (reply, prompt string, handled bool)
}

// Agent serves invokr chat sessions over ACP; sessions live only as long as the process.
type Agent struct {
	conn       *acp.AgentConn
	newSession runtime.ChatSessionFunc
	commands   Commands
	sessions   sync.Map
}

// latestVersion is the newer invokr release tag, "" when none is known; tests replace it.
var latestVersion = version.LatestVersion

// Serve runs the agent until the client hangs up; commands may be nil.
func Serve(ctx context.Context, in io.Reader, out io.Writer, newSession runtime.ChatSessionFunc, commands Commands) error {
	a := &Agent{conn: acp.NewAgentConn(in, out), newSession: newSession, commands: commands}
	latestVersion() // starts the background lookup so the first reply can carry its answer
	a.conn.Serve(ctx, a)
	<-a.conn.Done()
	return a.conn.Err()
}

func (a *Agent) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	text := &acp.AgentCapabilities{PromptCapabilities: &acp.PromptCapabilities{}}
	return acp.InitializeResponse{ProtocolVersion: acp.V1, AgentCapabilities: text, AgentInfo: &acp.Implementation{Name: "invokr", Version: version.Version}}, nil
}

func (a *Agent) Authenticate(context.Context, acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}

// NewSession only serves the workspace invokr was started in: sessions cannot carry their own.
func (a *Agent) NewSession(ctx context.Context, req acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	wd, err := os.Getwd()
	if err != nil {
		return acp.NewSessionResponse{}, err
	}
	if !sameDir(req.Cwd, wd) {
		return acp.NewSessionResponse{}, fmt.Errorf("acp: cwd %q differs from the workspace invokr was started in (%q); start invokr acp in that directory", req.Cwd, wd)
	}
	sess, broker, err := a.newSession("")
	if err != nil {
		return acp.NewSessionResponse{}, err
	}
	id := acp.SessionID(sess.SessionID())
	chat := &chatSession{IdentifiedSession: sess, approvals: broker}
	listModels(ctx, chat)
	a.sessions.Store(id, chat)
	commands := []acp.AvailableCommand{modeCommand}
	if a.commands != nil {
		commands = append(commands, a.commands.Available()...)
	}
	// Not ordered against the response: the library offers no hook, and a
	// client may see the update just before session/new returns.
	go func() {
		_ = a.update(context.WithoutCancel(ctx), id, acp.SessionUpdate{AvailableCommandsUpdate: &acp.AvailableCommandsUpdate{AvailableCommands: commands}})
	}()
	return acp.NewSessionResponse{SessionID: id, Modes: modeState(broker.Policy), ConfigOptions: configOptions(chat)}, nil
}

func (a *Agent) session(id acp.SessionID) (*chatSession, error) {
	v, ok := a.sessions.Load(id)
	if !ok {
		return nil, acp.NewError(acp.ErrInvalidParams, fmt.Sprintf("acp: unknown session %q", id))
	}
	return v.(*chatSession), nil
}

func (a *Agent) update(ctx context.Context, id acp.SessionID, u acp.SessionUpdate) error {
	return a.conn.SessionUpdate(ctx, acp.SessionNotification{SessionID: id, Update: u})
}

func sameDir(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

func (a *Agent) Prompt(ctx context.Context, req acp.PromptRequest) (acp.PromptResponse, error) {
	sess, err := a.session(req.SessionID)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	var text []string
	for _, block := range req.Prompt {
		if block.Text == nil {
			return acp.PromptResponse{}, acp.NewError(acp.ErrInvalidParams, "acp: only text prompt blocks are supported")
		}
		text = append(text, block.Text.Text)
	}
	sess.turnMu.Lock()
	defer sess.turnMu.Unlock()
	input := strings.Join(text, "\n")
	if reply, ok := a.modeText(ctx, req.SessionID, sess, input); ok {
		return a.reply(ctx, req.SessionID, reply)
	}
	if a.commands != nil {
		if reply, prompt, ok := a.commands.Run(ctx, sess.IdentifiedSession, input); ok {
			if prompt == "" {
				return a.commandReply(ctx, req.SessionID, sess, input, reply)
			}
			input = prompt
		}
	}
	return a.send(ctx, req.SessionID, sess, input)
}

// commandReply answers a command, first telling the client about any option it changed.
func (a *Agent) commandReply(ctx context.Context, id acp.SessionID, sess *chatSession, input, reply string) (acp.PromptResponse, error) {
	if configChanged(input) {
		_ = a.update(ctx, id, acp.SessionUpdate{ConfigOptionUpdate: &acp.ConfigOptionUpdate{ConfigOptions: configOptions(sess)}})
	}
	return a.reply(ctx, id, reply)
}

// send runs one model turn; the caller holds sess.turnMu.
func (a *Agent) send(ctx context.Context, id acp.SessionID, sess *chatSession, input string) (acp.PromptResponse, error) {
	var denied []turn.ToolOutcome
	var approved []acp.ToolCallID
	stopDrain := approval.Drain(sess.approvals, func(r approval.Request) approval.Decision {
		d := a.ask(ctx, id, r)
		if d.Approved {
			approved = append(approved, acp.ToolCallID(r.ID))
		} else {
			denied = append(denied, turn.ToolOutcome{Tool: r.Tool, Detail: strings.TrimPrefix(cmp.Or(r.Summary, r.Tool), "Run ")})
		}
		return d
	})
	resp, err := sess.Send(ctx, input)
	stopDrain()
	final := acp.ToolCallStatusCompleted
	if err != nil {
		final = acp.ToolCallStatusFailed
	}
	for _, callID := range approved {
		update := acp.SessionUpdate{ToolCallUpdate: &acp.ToolCallUpdate{ToolCallID: callID, Status: final}}
		_ = a.conn.SessionUpdate(context.WithoutCancel(ctx), acp.SessionNotification{SessionID: id, Update: update})
	}
	if err != nil {
		if ctx.Err() != nil {
			return acp.PromptResponse{}, ctx.Err()
		}
		slog.Error("acp: prompt failed", "error", err)
		return acp.PromptResponse{}, acp.NewError(acp.ErrInternalError, "model request failed")
	}
	reply := strings.TrimSpace(resp.Message) + toolTrailer(resp.ExecutionEvidence, resp.ToolFailures, denied)
	if tag := latestVersion(); tag != "" && !sess.noticed {
		sess.noticed = true
		reply += fmt.Sprintf("\n\nℹ️ invokr %s is available (running %s): run `invokr update`", tag, version.Version)
	}
	return a.reply(ctx, id, reply)
}

func (a *Agent) reply(ctx context.Context, id acp.SessionID, text string) (acp.PromptResponse, error) {
	chunk := acp.SessionUpdate{AgentMessageChunk: &acp.ContentChunk{Content: acp.ContentBlock{Text: &acp.TextContent{Text: text}}}}
	if err := a.update(ctx, id, chunk); err != nil {
		return acp.PromptResponse{}, err
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func toolTrailer(evidence []turn.ExecutionEvidence, failures, denied []turn.ToolOutcome) string {
	pending := slices.Clone(failures)
	labels := slices.Clone(denied)
	var items []string
	for _, e := range evidence {
		label, mark := evidenceMark(e, &pending, &labels)
		items = append(items, label+" "+mark)
	}
	for _, f := range pending {
		items = append(items, deniedLabel(f, &labels)+" ✗ "+f.Kind)
	}
	if len(items) == 0 {
		return ""
	}
	return "\n\n— tools: " + strings.Join(items, "; ")
}

func takeFirst(list *[]turn.ToolOutcome, tool string) (turn.ToolOutcome, bool) {
	i := slices.IndexFunc(*list, func(o turn.ToolOutcome) bool { return o.Tool == tool })
	if i < 0 {
		return turn.ToolOutcome{}, false
	}
	o := (*list)[i]
	*list = slices.Delete(*list, i, i+1)
	return o, true
}

func deniedLabel(f turn.ToolOutcome, labels *[]turn.ToolOutcome) string {
	if f.Kind == turn.OutcomeKindDenied {
		if l, ok := takeFirst(labels, f.Tool); ok {
			return l.Detail
		}
	}
	return cmp.Or(f.Tool, "unknown")
}

func evidenceMark(e turn.ExecutionEvidence, pending, labels *[]turn.ToolOutcome) (string, string) {
	label := cmp.Or(e.Command, e.Tool)
	if e.Outcome == "completed" && (e.ExitCode == nil || *e.ExitCode == 0) {
		return label, "✓"
	}
	outcome := e.Outcome
	if f, ok := takeFirst(pending, e.Tool); ok {
		if outcome != "flagged" {
			outcome = f.Kind
		}
		if e.Command == "" {
			label = deniedLabel(f, labels)
		}
	}
	if e.ExitCode != nil {
		return label, fmt.Sprintf("✗ %s exit=%d", outcome, *e.ExitCode)
	}
	return label, "✗ " + outcome
}

func (a *Agent) ask(ctx context.Context, id acp.SessionID, req approval.Request) approval.Decision {
	callID := acp.ToolCallID(req.ID)
	title, kind := cmp.Or(req.Summary, req.Tool), toolKind(req)
	announce := acp.SessionUpdate{ToolCall: &acp.ToolCall{ToolCallID: callID, Name: req.Tool, Title: title, Kind: kind, Status: acp.ToolCallStatusPending}}
	if err := a.conn.SessionUpdate(ctx, acp.SessionNotification{SessionID: id, Update: announce}); err != nil {
		return approval.Decision{Reason: "permission request failed"}
	}
	resp, err := a.conn.RequestPermission(ctx, acp.RequestPermissionRequest{SessionID: id, Options: permissionOptions,
		ToolCall: acp.ToolCallUpdate{ToolCallID: callID, Name: req.Tool, Title: title, Kind: kind}})
	decision, status := approval.Decision{Reason: "denied by user"}, acp.ToolCallStatusFailed
	if err == nil && resp.Outcome.Selected != nil && resp.Outcome.Selected.OptionID == permissionOptions[0].OptionID {
		decision, status = approval.Decision{Approved: true}, acp.ToolCallStatusInProgress
	}
	update := acp.SessionUpdate{ToolCallUpdate: &acp.ToolCallUpdate{ToolCallID: callID, Status: status}}
	_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{SessionID: id, Update: update})
	return decision
}

func toolKind(req approval.Request) acp.ToolKind {
	tiers := strings.Split(req.Tier, ",")
	switch {
	case req.Tool == constants.ShellToolName || slices.Contains(tiers, constants.CodeInterpreterTierCodeExecution):
		return acp.ToolKindExecute
	case req.Writes || slices.Contains(tiers, "write_workspace") || slices.Contains(tiers, constants.CodeInterpreterTierArtifactWrite):
		return acp.ToolKindEdit
	case req.Network || slices.Contains(tiers, constants.CodeInterpreterTierNetworkCommand):
		return acp.ToolKindFetch
	case req.ReadOnly || slices.Contains(tiers, constants.CodeInterpreterTierReadWorkspace):
		return acp.ToolKindRead
	}
	return acp.ToolKindOther
}

func (a *Agent) Cancel(context.Context, acp.CancelNotification) error { return nil }
