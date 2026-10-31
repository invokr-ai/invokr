package approval

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/invokr-agent/invokr/platform/events"
	"github.com/invokr-agent/invokr/platform/turn"
)

type Mode string

const (
	Manual      Mode = "manual"
	Auto        Mode = "auto"
	Plan        Mode = "plan"
	YOLO        Mode = "yolo"
	YOLOWarning      = "YOLO: approvals and Invokr sandbox/workspace restrictions disabled. Explicit denials still apply. Restart to leave YOLO."
)

// Policy is session-local. The execution owners provide facts about prepared
// operations; neither model arguments nor tool-advertised capabilities prove confinement.
type Policy struct {
	mu      sync.Mutex
	mode    Mode
	publish bool
	outside bool
	store   *Store
	loadErr error
	active  map[Mode]int
}

func NewPolicy(mode Mode, publish bool) *Policy {
	mode = cmp.Or(mode, Manual)
	return &Policy{mode: mode, publish: publish, active: make(map[Mode]int)}
}

func (p *Policy) Mode() Mode {
	if p == nil {
		return Manual
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mode
}

func (p *Policy) SetMode(mode Mode) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mode == YOLO {
		return fmt.Errorf("YOLO is fixed for this session; restart to change mode")
	}
	if !slices.Contains([]Mode{Manual, Auto, Plan}, mode) {
		return fmt.Errorf("mode must be manual, auto, or plan; YOLO requires --yolo at startup")
	}
	if p.loadErr != nil && mode != Manual {
		return fmt.Errorf("standing rules unavailable: %w", p.loadErr)
	}
	p.mode = mode
	return nil
}

// Clone starts an independent policy for another session: same settings and
// standing-rules store, its own mode from here on.
func (p *Policy) Clone() *Policy {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return &Policy{mode: p.mode, publish: p.publish, outside: p.outside, store: p.store, loadErr: p.loadErr, active: make(map[Mode]int)}
}

func (p *Policy) SetPublishing(allow bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.publish = allow
}
func (p *Policy) Publishing() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.publish }

// SetOutsideReads lets read and file_search ask to reach paths beyond the
// workspace roots (workspace.reads "ask"), for this session.
func (p *Policy) SetOutsideReads(on bool) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.outside = on
}
func (p *Policy) OutsideReads() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.outside
}

func OutsideReadsAllowed(ctx context.Context) bool {
	s, ok := ctx.Value(callKey{}).(*callState)
	return ok && s.policy.OutsideReads()
}
func (p *Policy) SetStore(store *Store, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.store, p.loadErr = store, err
}

func (p *Policy) PriorWork() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for mode, count := range p.active {
		if mode != p.mode {
			n += count
		}
	}
	return n
}

type callKey struct{}
type callState struct {
	approver Approver
	policy   *Policy
	mode     Mode
	request  Request
}

func (p *Policy) Begin(ctx context.Context, req Request, approvers ...Approver) (context.Context, func()) {
	p.mu.Lock()
	s := &callState{policy: p, mode: p.mode, request: req}
	if len(approvers) > 0 {
		s.approver = approvers[0]
	}
	p.active[s.mode]++
	p.mu.Unlock()
	return context.WithValue(ctx, callKey{}, s), s.release
}

func (s *callState) release() {
	s.policy.mu.Lock()
	defer s.policy.mu.Unlock()
	s.policy.active[s.mode]--
}

// Retain keeps background work visible after its initiating tool has returned.
func Retain(ctx context.Context) func() {
	events.RecordExecution(ctx, turn.ExecutionEvidence{Persistent: true})
	s, _ := ctx.Value(callKey{}).(*callState)
	if s == nil {
		return func() {}
	}
	s.policy.mu.Lock()
	s.policy.active[s.mode]++
	s.policy.mu.Unlock()
	return sync.OnceFunc(s.release)
}

func ModeFromContext(ctx context.Context) Mode {
	if s, ok := ctx.Value(callKey{}).(*callState); ok {
		return s.mode
	}
	return Manual
}

func CallRequest(ctx context.Context) Request {
	if s, ok := ctx.Value(callKey{}).(*callState); ok {
		return s.request
	}
	return Request{}
}

// RulesPath lets execution owners seal the actual store, including custom locations.
func RulesPath(ctx context.Context) string {
	s, ok := ctx.Value(callKey{}).(*callState)
	if !ok {
		return ""
	}
	s.policy.mu.Lock()
	defer s.policy.mu.Unlock()
	if s.policy.store == nil {
		return ""
	}
	return s.policy.store.path
}

// Authorize returns handled=false for legacy callers without session policy.
// Approval is bound to this invocation, never installed as a standing grant.
func Authorize(ctx context.Context, approver Approver, req Request, ask bool) (bool, error) {
	s, ok := ctx.Value(callKey{}).(*callState)
	if !ok {
		return false, nil
	}
	approver = cmp.Or(approver, s.approver)
	req.Subject = cmp.Or(req.Subject, s.request.Subject)
	decision, resolved := s.decide(req, ask)
	if resolved {
		return true, decisionError(req, decision)
	}
	if approver == nil {
		return true, &DeniedError{Tool: req.Tool, Reason: "approval required but no approver is configured"}
	}
	req.PolicyChecked = true
	decision, err := approver.RequestApproval(ctx, req)
	if err != nil {
		return true, err
	}
	recheck := *s
	recheck.mode = s.policy.Mode()
	if current, resolved := recheck.decide(req, ask); resolved && !current.Approved {
		return true, decisionError(req, current)
	}
	return true, decisionError(req, decision)
}

func decisionError(req Request, d Decision) error {
	if !d.Approved {
		return &DeniedError{Tool: req.Tool, Reason: d.Reason}
	}
	return nil
}

func (s *callState) denial(req Request) (Decision, bool) {
	s.policy.mu.Lock()
	store, err := s.policy.store, s.policy.loadErr
	s.policy.mu.Unlock()
	if err != nil && s.mode != Manual {
		return Decision{Reason: "standing rules unavailable: " + err.Error()}, true
	}
	return store.DenyOnly(req)
}

func (s *callState) decide(req Request, ask bool) (Decision, bool) {
	if d, ok := s.denial(req); ok {
		return d, true
	}
	if s.mode == Plan && blockedInPlan(req) {
		return Decision{Reason: "Plan permits only reads and confined verification with read-only sources"}, true
	}
	s.policy.mu.Lock()
	store, publish := s.policy.store, s.policy.publish
	s.policy.mu.Unlock()
	if s.mode == YOLO {
		return Decision{Approved: true, Reason: "YOLO session policy"}, true
	}
	if s.mode == Manual && !ask {
		return Decision{Approved: true}, true
	}
	if needsScopedApproval(s.mode, req, publish) {
		return Decision{}, false
	}
	if d, ok := store.Decide(req); ok {
		return d, true
	}
	if autoApproved(s.mode, req) {
		return Decision{Approved: true, Reason: string(s.mode) + " session policy"}, true
	}
	return Decision{}, false
}

// autoApproved is the session policy's own yes for reads and confined work;
// a read outside the workspace never gets it.
func autoApproved(mode Mode, req Request) bool {
	return mode != Manual && !req.Outside && (req.ReadOnly || req.Confined)
}

func blockedInPlan(req Request) bool {
	return !req.ReadOnly && !req.Confined || req.Writes || req.Publishes
}

func needsScopedApproval(mode Mode, req Request, publish bool) bool {
	return mode == Auto && (req.Protected || req.RequiresScopedApproval || req.Publishes && !publish) || mode == Plan && req.Network
}
