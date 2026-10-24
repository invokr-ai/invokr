package approval

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

type Request struct {
	ID, Tool, Reason, Tier, Summary, Details string
	// Execution facts are supplied by trusted tool preparation, never tool arguments.
	ReadOnly, Confined, Writes, Network, Publishes, Protected bool
	// RequiresScopedApproval keeps opaque execution behind a per-call decision in Auto.
	RequiresScopedApproval bool
	Paths                  []string
	PolicyChecked          bool
	// Subject is the canonical, matchable argument string for this call (for
	// shell, the command line). It is the ONLY thing standing grants match
	// against: a request that leaves it empty can never be auto-approved by a
	// grant. Never put anything model-controlled but unvalidated here that the
	// user would not recognise as "what this call does".
	Subject string
	// SubjectIsPathList marks Subject as a space-separated list of paths
	// (apply_patch). Standing grants then match each path independently, so a
	// pattern like `docs/*` must cover every touched path — a wildcard can
	// never swallow the separator and hide an extra path.
	SubjectIsPathList bool
	// Outside marks a read beyond the workspace roots. Only the user or a
	// standing grant may approve it: no mode's read-only auto-approval does.
	Outside bool
	// GrantPattern, when set, is the pattern an "always" decision stores
	// instead of one derived from Subject. Empty on an Outside request means
	// it can only be approved once.
	GrantPattern string
	// Diff, when set, is the file change a write would make, for the prompt
	// to show as a diff instead of the raw arguments.
	Diff *FileDiff
}

// FileDiff is one file's whole content before and after a proposed write.
type FileDiff struct{ Path, Before, After string }

type Decision struct {
	Approved bool
	Reason   string
}

type DeniedError struct{ Tool, Reason string }

func (e *DeniedError) Error() string {
	if strings.TrimSpace(e.Tool) == "" {
		return e.Reason
	}
	return fmt.Sprintf("approval denied for %s: %s", e.Tool, e.Reason)
}

type Approver interface {
	RequestApproval(context.Context, Request) (Decision, error)
}

type Broker struct {
	requests chan Pending
	Policy   *Policy
}

func NewBroker() *Broker { return &Broker{requests: make(chan Pending)} }

func (b *Broker) RequestApproval(ctx context.Context, req Request) (Decision, error) {
	if b == nil {
		return Decision{}, fmt.Errorf("approval required for %s but no approver is configured", req.Tool)
	}
	req.Tool = strings.TrimSpace(req.Tool)
	req.Tool = cmp.Or(req.Tool, "tool")
	if strings.TrimSpace(req.ID) == "" {
		req.ID = uuid.NewString()
	}
	pending := Pending{Request: req, decision: make(chan Decision, 1)}
	select {
	case <-ctx.Done():
		return Decision{}, ctx.Err()
	case b.requests <- pending:
	}
	select {
	case <-ctx.Done():
		return Decision{}, ctx.Err()
	case decision := <-pending.decision:
		if decision.Approved {
			return decision, nil
		}
		return decision, &DeniedError{Tool: req.Tool, Reason: cmp.Or(strings.TrimSpace(decision.Reason), "denied by user")}
	}
}

func (b *Broker) Requests() <-chan Pending {
	if b == nil {
		return nil
	}
	return b.requests
}

// Drain consumes the broker's pending-approval channel in a goroutine,
// resolving each request with the decision returned by decide. It returns a
// stop function that cancels the goroutine; a nil broker yields a no-op stop.
func Drain(broker *Broker, decide func(Request) Decision) func() {
	if broker == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case req, ok := <-broker.Requests():
				if !ok {
					return
				}
				req.Decide(decide(req.Request))
			}
		}
	}()
	return cancel
}

type Pending struct {
	Request
	decision chan Decision
}

func (p Pending) Decide(decision Decision) {
	select {
	case p.decision <- decision:
	default:
	}
}
