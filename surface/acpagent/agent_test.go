package acpagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/invokr-agent/acp"
	"go.lsp.dev/jsonrpc2"

	"github.com/invokr-agent/invokr/capability/approval"
	"github.com/invokr-agent/invokr/platform/turn"
)

type stubSession struct {
	id     string
	broker *approval.Broker
}

func (s *stubSession) SessionID() string { return s.id }

func (s *stubSession) Send(ctx context.Context, text string) (turn.AgentResponse, error) {
	switch text {
	case "block":
		<-ctx.Done()
		return turn.AgentResponse{}, errors.New("model call aborted")
	case "fail":
		return turn.AgentResponse{}, errors.New("provider down")
	}
	if strings.HasPrefix(text, "echo ") {
		return turn.AgentResponse{Message: text}, nil
	}
	if _, err := s.broker.RequestApproval(ctx, approval.Request{Tool: "shell", Summary: "run ls"}); err != nil {
		return turn.AgentResponse{Message: "denied"}, nil
	}
	return ranLs, nil
}

var ranLs = turn.AgentResponse{Message: "ran ls", ToolFailures: []turn.ToolOutcome{{Tool: "shell", Kind: "denied"}},
	ExecutionEvidence: []turn.ExecutionEvidence{{Tool: "shell", Command: "ls", Outcome: "completed", ExitCode: new(0)}, {Tool: "shell", Outcome: "flagged"}}}

type stubClient struct {
	decide func(acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error)

	mu       sync.Mutex
	asked    []acp.RequestPermissionRequest
	updates  []acp.SessionUpdate
	commands []acp.AvailableCommand // kept apart: it arrives asynchronously after session/new
}

func (c *stubClient) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if u := n.Update.AvailableCommandsUpdate; u != nil {
		c.commands = u.AvailableCommands
		return nil
	}
	c.updates = append(c.updates, n.Update)
	return nil
}

func (c *stubClient) RequestPermission(_ context.Context, req acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	c.mu.Lock()
	c.asked = append(c.asked, req)
	c.mu.Unlock()
	return c.decide(req)
}

func selectOption(i int) func(acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	return func(req acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
		return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Selected: &acp.SelectedPermissionOutcome{OptionID: req.Options[i].OptionID}}}, nil
	}
}

func connect(t *testing.T, client *stubClient) *acp.ClientConn {
	t.Helper()
	return connectWith(t, client, nil)
}

func connectWith(t *testing.T, client *stubClient, commands Commands) *acp.ClientConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	toAgentR, toAgentW := io.Pipe()
	toClientR, toClientW := io.Pipe()
	var n atomic.Int32
	newSession := func(string) (turn.IdentifiedSession, *approval.Broker, error) {
		broker := approval.NewBroker()
		broker.Policy = approval.NewPolicy(approval.Manual, false)
		return &stubSession{fmt.Sprintf("sess-%d", n.Add(1)), broker}, broker, nil
	}
	go func() { _ = Serve(ctx, toAgentR, toClientW, newSession, commands) }()
	cc := acp.Connect(ctx, client, toClientR, toAgentW)
	t.Cleanup(func() { _ = cc.Close() })
	init, err := cc.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.V1})
	if err != nil || init.ProtocolVersion != acp.V1 {
		t.Fatalf("initialize = %+v, %v", init, err)
	}
	if pc := init.AgentCapabilities.PromptCapabilities; pc.Image || pc.Audio || pc.EmbeddedContext {
		t.Fatalf("prompt capabilities = %+v, want text only", pc)
	}
	return cc
}

func newSession(t *testing.T, cc *acp.ClientConn) acp.SessionID {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	sess, err := cc.NewSession(context.Background(), acp.NewSessionRequest{Cwd: wd})
	if err != nil {
		t.Fatal(err)
	}
	return sess.SessionID
}

func prompt(cc *acp.ClientConn, id acp.SessionID, text string) (acp.PromptResponse, error) {
	return cc.Prompt(context.Background(), acp.PromptRequest{SessionID: id, Prompt: []acp.ContentBlock{{Text: &acp.TextContent{Text: text}}}})
}

func lastReply(t *testing.T, c *stubClient) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	u := c.updates[len(c.updates)-1]
	if u.AgentMessageChunk == nil {
		t.Fatalf("last update = %+v, want message chunk", u)
	}
	return u.AgentMessageChunk.Content.Text.Text
}

func TestPromptPermissionOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		decide func(acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error)
		want   string
		status acp.ToolCallStatus
	}{
		{"allow", selectOption(0), "ran ls\n\n— tools: ls ✓; shell ✗ flagged", acp.ToolCallStatusInProgress},
		{"reject", selectOption(1), "denied", acp.ToolCallStatusFailed},
		{"cancelled", func(acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
			return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}}}, nil
		}, "denied", acp.ToolCallStatusFailed},
		{"error", func(acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
			return acp.RequestPermissionResponse{}, errors.New("client gone")
		}, "denied", acp.ToolCallStatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &stubClient{decide: tc.decide}
			cc := connect(t, client)
			id := newSession(t, cc)
			resp, err := prompt(cc, id, "list files")
			if err != nil || resp.StopReason != acp.StopReasonEndTurn {
				t.Fatalf("prompt = %+v, %v", resp, err)
			}
			if got := lastReply(t, client); got != tc.want {
				t.Fatalf("reply = %q, want %q", got, tc.want)
			}
			asked := client.asked[0]
			if asked.ToolCall.Name != "shell" || asked.ToolCall.Title != "run ls" || asked.ToolCall.Kind != acp.ToolKindExecute {
				t.Fatalf("permission request = %+v", asked)
			}
			announce, done := client.updates[0].ToolCall, client.updates[1].ToolCallUpdate
			if announce == nil || announce.ToolCallID != asked.ToolCall.ToolCallID || announce.Status != acp.ToolCallStatusPending {
				t.Fatalf("announce = %+v", client.updates[0])
			}
			if done == nil || done.ToolCallID != announce.ToolCallID || done.Status != tc.status {
				t.Fatalf("completion = %+v, want status %q", client.updates[1], tc.status)
			}
			if tc.status == acp.ToolCallStatusInProgress {
				if fin := client.updates[2].ToolCallUpdate; fin == nil || fin.ToolCallID != announce.ToolCallID || fin.Status != acp.ToolCallStatusCompleted {
					t.Fatalf("final = %+v, want completed", client.updates[2])
				}
			}
		})
	}
}

func TestToolTrailerMergesDeniedCall(t *testing.T) {
	evidence := []turn.ExecutionEvidence{
		{Tool: "shell", Command: "rm note.txt", Outcome: "completed", ExitCode: new(0)},
		{Tool: "shell", Command: "rm keep.txt", Outcome: "failed"},
	}
	failures := []turn.ToolOutcome{{Tool: "shell", Kind: "denied"}}
	want := "\n\n— tools: rm note.txt ✓; rm keep.txt ✗ denied"
	if got := toolTrailer(evidence, failures, nil); got != want {
		t.Fatalf("trailer = %q, want %q", got, want)
	}
}

func TestToolTrailerLabelsDeniedFromApproval(t *testing.T) {
	denied := []turn.ToolOutcome{{Tool: "shell", Detail: "rm keep.txt"}}
	failures := []turn.ToolOutcome{{Tool: "shell", Kind: "denied"}}
	want := "\n\n— tools: rm keep.txt ✗ denied"
	for name, evidence := range map[string][]turn.ExecutionEvidence{
		"no evidence":   nil,
		"with evidence": {{Tool: "shell", Outcome: "failed"}},
	} {
		if got := toolTrailer(evidence, failures, denied); got != want {
			t.Fatalf("%s: trailer = %q, want %q", name, got, want)
		}
	}
}

func TestToolKind(t *testing.T) {
	for _, tc := range []struct {
		req  approval.Request
		want acp.ToolKind
	}{
		{approval.Request{Tool: "shell"}, acp.ToolKindExecute},
		{approval.Request{Tier: "write_workspace"}, acp.ToolKindEdit},
		{approval.Request{Tool: "web_fetch", Network: true}, acp.ToolKindFetch},
		{approval.Request{Tier: "read_workspace"}, acp.ToolKindRead},
		{approval.Request{Tier: "delegate_task"}, acp.ToolKindOther},
		{approval.Request{Tool: "code_interpreter", Tier: "read_workspace,code_execution,artifact_write"}, acp.ToolKindExecute},
		{approval.Request{Tier: "artifact_write"}, acp.ToolKindEdit},
		{approval.Request{Tier: "execute_network_command"}, acp.ToolKindFetch},
		{approval.Request{Tool: "edit", Writes: true}, acp.ToolKindEdit},
		{approval.Request{ReadOnly: true}, acp.ToolKindRead},
		{approval.Request{Tier: "unread_workspace"}, acp.ToolKindOther},
	} {
		if got := toolKind(tc.req); got != tc.want {
			t.Errorf("toolKind(%+v) = %q, want %q", tc.req, got, tc.want)
		}
	}
}

func TestConcurrentSessionsHaveIndependentApprovals(t *testing.T) {
	var allowed acp.SessionID
	client := &stubClient{decide: func(req acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
		return selectOption(map[bool]int{true: 0, false: 1}[req.SessionID == allowed])(req)
	}}
	cc := connect(t, client)
	allowed = newSession(t, cc)
	denied := newSession(t, cc)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, id := range []acp.SessionID{allowed, denied} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := prompt(cc, id, "list files")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got := map[acp.ToolCallID]acp.ToolCallStatus{}
	sessionOf := map[acp.ToolCallID]acp.SessionID{}
	for _, a := range client.asked {
		sessionOf[a.ToolCall.ToolCallID] = a.SessionID
	}
	for _, u := range client.updates {
		if u.ToolCallUpdate != nil {
			got[u.ToolCallUpdate.ToolCallID] = u.ToolCallUpdate.Status
		}
	}
	for call, status := range got {
		want := acp.ToolCallStatusFailed
		if sessionOf[call] == allowed {
			want = acp.ToolCallStatusCompleted
		}
		if status != want {
			t.Fatalf("session %s call %s status %q, want %q", sessionOf[call], call, status, want)
		}
	}
	if len(got) != 2 {
		t.Fatalf("tool call updates = %v, want 2", got)
	}
}

func TestPromptErrors(t *testing.T) {
	cc := connect(t, &stubClient{decide: selectOption(0)})
	id := newSession(t, cc)
	for _, tc := range []struct {
		name string
		req  acp.PromptRequest
		want string
		code acp.ErrorCode
	}{
		{"unknown session", acp.PromptRequest{SessionID: "nope", Prompt: []acp.ContentBlock{{Text: &acp.TextContent{Text: "hi"}}}}, "unknown session", acp.ErrInvalidParams},
		{"send error", acp.PromptRequest{SessionID: id, Prompt: []acp.ContentBlock{{Text: &acp.TextContent{Text: "fail"}}}}, "model request failed", acp.ErrInternalError},
		{"non-text block", acp.PromptRequest{SessionID: id, Prompt: []acp.ContentBlock{{Image: &acp.ImageContent{Data: "x", MIMEType: "image/png"}}}}, "only text", acp.ErrInvalidParams},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := cc.Prompt(context.Background(), tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if rpcErr := (*jsonrpc2.Error)(nil); !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc2.Code(tc.code) {
				t.Fatalf("err = %#v, want code %d", err, tc.code)
			}
		})
	}
}

func TestUpdateNoticeOncePerSession(t *testing.T) {
	orig := latestVersion
	latestVersion = func() string { return "v9.9.9" }
	t.Cleanup(func() { latestVersion = orig })
	client := &stubClient{decide: selectOption(0)}
	cc := connect(t, client)
	id := newSession(t, cc)
	for i, want := range []bool{true, false} {
		if _, err := prompt(cc, id, "list files"); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(lastReply(t, client), "invokr v9.9.9 is available"); got != want {
			t.Fatalf("reply %d: notice = %v, want %v", i, got, want)
		}
	}
}

func TestNewSessionRefusesOtherCwd(t *testing.T) {
	cc := connect(t, &stubClient{})
	if _, err := cc.NewSession(context.Background(), acp.NewSessionRequest{Cwd: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "differs from the workspace") {
		t.Fatalf("err = %v, want cwd refusal", err)
	}
}

func TestNewSessionAcceptsSymlinkedCwd(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "ws")
	if err := os.Symlink(wd, link); err != nil {
		t.Fatal(err)
	}
	cc := connect(t, &stubClient{})
	if _, err := cc.NewSession(context.Background(), acp.NewSessionRequest{Cwd: link}); err != nil {
		t.Fatalf("NewSession via symlink: %v", err)
	}
}

func TestCancelStopsPrompt(t *testing.T) {
	cc := connect(t, &stubClient{})
	id := newSession(t, cc)
	done := make(chan acp.PromptResponse, 1)
	go func() {
		resp, _ := prompt(cc, id, "block")
		done <- resp
	}()
	time.Sleep(50 * time.Millisecond)
	if err := cc.Cancel(context.Background(), acp.CancelNotification{SessionID: id}); err != nil {
		t.Fatal(err)
	}
	if resp := <-done; resp.StopReason != acp.StopReasonCancelled {
		t.Fatalf("stop reason = %q, want cancelled", resp.StopReason)
	}
}

type stubCommands struct{}

func (stubCommands) Available() []acp.AvailableCommand {
	return []acp.AvailableCommand{{Name: "usage", Description: "show token usage"}}
}

func (stubCommands) Run(_ context.Context, sess turn.Session, text string) (string, string, bool) {
	if _, ok := sess.(*stubSession); !ok {
		return "got a wrapper, not the runtime session", "", true // commands type-assert the session's optional interfaces
	}
	switch {
	case text == "/usage":
		return "12 tokens", "", true
	case strings.HasPrefix(text, "/skill:x"):
		return "", "echo follow skill x", true
	}
	return "", "", false
}

func TestSlashCommands(t *testing.T) {
	client := &stubClient{decide: selectOption(0)}
	cc := connectWith(t, client, stubCommands{})
	id := newSession(t, cc)
	deadline := time.Now().Add(2 * time.Second)
	for !hasCommands(client) {
		if time.Now().After(deadline) {
			t.Fatal("no available_commands_update after session/new")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for text, want := range map[string]string{
		"/usage":        "12 tokens",
		"/skill:x task": "echo follow skill x",
		"echo /unknown": "echo /unknown",
	} {
		if _, err := prompt(cc, id, text); err != nil {
			t.Fatal(err)
		}
		if got := lastReply(t, client); !strings.HasPrefix(got, want) {
			t.Errorf("%q replied %q, want %q", text, got, want)
		}
	}
}

func hasCommands(c *stubClient) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.ContainsFunc(c.commands, func(cmd acp.AvailableCommand) bool { return cmd.Name == "usage" })
}

func TestModesArePerSession(t *testing.T) {
	client := &stubClient{decide: selectOption(0)}
	cc := connect(t, client)
	wd, _ := os.Getwd()
	first, err := cc.NewSession(context.Background(), acp.NewSessionRequest{Cwd: wd})
	if err != nil || first.Modes == nil || first.Modes.CurrentModeID != "manual" || len(first.Modes.AvailableModes) != 3 {
		t.Fatalf("session/new modes = %+v, %v", first.Modes, err)
	}
	other := newSession(t, cc)
	if _, err := cc.SetSessionMode(context.Background(), acp.SetSessionModeRequest{SessionID: first.SessionID, ModeID: "plan"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cc.SetSessionMode(context.Background(), acp.SetSessionModeRequest{SessionID: first.SessionID, ModeID: "yolo"}); err == nil {
		t.Fatal("set_mode accepted yolo")
	}
	for id, want := range map[acp.SessionID]string{first.SessionID: "mode: plan", other: "mode: manual"} {
		if _, err := prompt(cc, id, "/mode"); err != nil {
			t.Fatal(err)
		}
		if got := lastReply(t, client); got != want {
			t.Errorf("/mode in %s = %q, want %q", id, got, want)
		}
	}
	if _, err := prompt(cc, other, "/mode auto"); err != nil {
		t.Fatal(err)
	}
	if got := lastReply(t, client); !strings.HasPrefix(got, "mode: auto") {
		t.Errorf("/mode auto = %q", got)
	}
}
