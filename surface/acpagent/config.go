package acpagent

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/invokr-agent/acp"

	"github.com/invokr-agent/invokr/capability/approval"
	"github.com/invokr-agent/invokr/clients"
)

var modes = []acp.SessionMode{
	{ID: acp.SessionModeID(approval.Manual), Name: "Manual", Description: "ask before tools that need approval"},
	{ID: acp.SessionModeID(approval.Auto), Name: "Auto", Description: "run workspace tools without asking"},
	{ID: acp.SessionModeID(approval.Plan), Name: "Plan", Description: "read and plan; no changes"},
}

var modeCommand = acp.AvailableCommand{Name: "mode", Description: "show or set approval mode",
	Input: &acp.AvailableCommandInput{Hint: "[manual|auto|plan]"}}

var reasoningLevels = []string{"default", "none", "minimal", "low", "medium", "high", "xhigh", "max"}

const (
	configModel     acp.SessionConfigID = "model"
	configReasoning acp.SessionConfigID = "reasoning"
	listModelsWait                      = 3 * time.Second
)

type modelSession interface {
	SetModel(string)
	Model() string
}

type modelLister interface {
	ListModels(context.Context) ([]string, error)
}

type reasoningSession interface {
	SetReasoningEffort(string) error
	ReasoningEffort() clients.ReasoningState
}

func modeState(p *approval.Policy) *acp.SessionModeState {
	return &acp.SessionModeState{CurrentModeID: acp.SessionModeID(p.Mode()), AvailableModes: modes}
}

func (a *Agent) SetSessionMode(ctx context.Context, req acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	sess, err := a.session(req.SessionID)
	if err != nil {
		return acp.SetSessionModeResponse{}, err
	}
	if err := a.setMode(ctx, req.SessionID, sess, string(req.ModeID)); err != nil {
		return acp.SetSessionModeResponse{}, acp.NewError(acp.ErrInvalidParams, err.Error())
	}
	return acp.SetSessionModeResponse{}, nil
}

func (a *Agent) setMode(ctx context.Context, id acp.SessionID, sess *chatSession, mode string) error {
	if err := sess.approvals.Policy.SetMode(approval.Mode(mode)); err != nil {
		return err
	}
	return a.update(ctx, id, acp.SessionUpdate{CurrentModeUpdate: &acp.CurrentModeUpdate{CurrentModeID: acp.SessionModeID(mode)}})
}

// modeText answers /mode (and its alias /approvals) in chat.
func (a *Agent) modeText(ctx context.Context, id acp.SessionID, sess *chatSession, input string) (string, bool) {
	fields := strings.Fields(input)
	if len(fields) == 0 || (fields[0] != "/mode" && fields[0] != "/approvals") {
		return "", false
	}
	switch len(fields) {
	case 1:
		return "mode: " + string(sess.approvals.Policy.Mode()), true
	case 2:
		if err := a.setMode(ctx, id, sess, fields[1]); err != nil {
			return err.Error(), true
		}
		return "mode: " + fields[1] + " (future calls; running work keeps its permissions)", true
	}
	return "usage: /mode [manual|auto|plan]", true
}

func (a *Agent) SetSessionConfigOption(ctx context.Context, req acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	sess, err := a.session(req.SessionID)
	if err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}
	var value string
	if err := json.Unmarshal(req.Value, &value); err != nil {
		return acp.SetSessionConfigOptionResponse{}, acp.NewError(acp.ErrInvalidParams, "acp: config value must be a string")
	}
	ms, canModel := sess.IdentifiedSession.(modelSession)
	rs, canReason := sess.IdentifiedSession.(reasoningSession)
	switch {
	case req.ConfigID == configModel && canModel:
		ms.SetModel(value)
	case req.ConfigID == configReasoning && canReason:
		err = rs.SetReasoningEffort(value)
	default:
		err = fmt.Errorf("acp: unknown config option %q", req.ConfigID)
	}
	if err != nil {
		return acp.SetSessionConfigOptionResponse{}, acp.NewError(acp.ErrInvalidParams, err.Error())
	}
	return acp.SetSessionConfigOptionResponse{ConfigOptions: configOptions(ctx, sess)}, nil
}

// configOptions offers the model and reasoning effort as selects; a session
// that supports neither offers none.
func configOptions(ctx context.Context, sess *chatSession) []acp.SessionConfigOption {
	var out []acp.SessionConfigOption
	if ms, ok := sess.IdentifiedSession.(modelSession); ok {
		models := []string{ms.Model()}
		if lister, ok := sess.IdentifiedSession.(modelLister); ok {
			listCtx, cancel := context.WithTimeout(ctx, listModelsWait)
			if listed, err := lister.ListModels(listCtx); err == nil && len(listed) > 0 {
				models = listed
			}
			cancel()
		}
		out = append(out, selectConfig(configModel, "Model", acp.SessionConfigOptionCategoryModel, ms.Model(), models))
	}
	if rs, ok := sess.IdentifiedSession.(reasoningSession); ok {
		level := cmp.Or(rs.ReasoningEffort().Level, "default")
		out = append(out, selectConfig(configReasoning, "Reasoning", acp.SessionConfigOptionCategoryThoughtLevel, level, reasoningLevels))
	}
	return out
}

func selectConfig(id acp.SessionConfigID, name string, category acp.SessionConfigOptionCategory, current string, values []string) acp.SessionConfigOption {
	options := make([]acp.SessionConfigSelectOption, len(values))
	for i, v := range values {
		options[i] = acp.SessionConfigSelectOption{Value: acp.SessionConfigValueID(v), Name: v}
	}
	currentJSON, _ := json.Marshal(current)
	optionsJSON, _ := json.Marshal(options)
	return acp.SessionConfigOption{ID: id, Name: name, Category: category, Type: "select", CurrentValue: currentJSON, Options: optionsJSON}
}

// configChanged reports a /model, /reasoning or /profile command, after which
// clients get the new option values.
func configChanged(input string) bool {
	name, _, _ := strings.Cut(strings.TrimPrefix(input, "/"), " ")
	return strings.HasPrefix(input, "/") && (name == "model" || name == "reasoning" || name == "profile")
}
