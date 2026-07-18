package policy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jwarykowski/drover/loop"
)

// Reasoner is the LLM call, behind an interface so the loop and tests never
// depend on a live model. The real Anthropic client is one implementation
// (policy/anthropic.go); tests use a fake. It returns proposed specs drawn from
// a fixed vocabulary — never free-form shell.
type Reasoner interface {
	// Propose returns candidate task specs for the event. schema is shepherd's
	// item JSON Schema (from `shepherd schema`), handed to the model so it targets
	// a valid item shape.
	Propose(ctx context.Context, event loop.Event, board []loop.Item, schema []byte) ([]loop.Spec, error)
}

// LLMReasoner is a Policy backed by a Reasoner. Every proposal is validated
// against the allowed vocabulary before it becomes an Action; on timeout or
// error it falls back to a deterministic policy. This keeps a nondeterministic
// model from ever producing an action the executor wouldn't accept.
type LLMReasoner struct {
	Reasoner Reasoner
	Schema   []byte        // shepherd item schema; constrains + informs the model
	Timeout  time.Duration // 0 means no deadline beyond the caller's ctx
	Fallback loop.Policy   // used when the reasoner errors or times out
	Logf     func(string, ...any)
}

func (r LLMReasoner) logf(format string, a ...any) {
	if r.Logf != nil {
		r.Logf(format, a...)
	}
}

// Decide asks the reasoner for proposals, validates each, and returns the valid
// ones as AddTask actions. Any failure routes to the fallback policy.
func (r LLMReasoner) Decide(ctx context.Context, c loop.Context) []loop.Action {
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	specs, err := r.Reasoner.Propose(ctx, c.Event, c.Board, r.Schema)
	if err != nil {
		r.logf("llm reasoner failed (%v); using fallback", err)
		return r.fallback(ctx, c)
	}

	var actions []loop.Action
	for _, s := range specs {
		if verr := ValidateSpec(s); verr != nil {
			// Untrusted model output that misses the vocabulary is dropped, not acted on.
			r.logf("dropping invalid proposal %q: %v", s.Text, verr)
			continue
		}
		actions = append(actions, loop.AddTask{Spec: s})
	}
	return actions
}

func (r LLMReasoner) fallback(ctx context.Context, c loop.Context) []loop.Action {
	if r.Fallback == nil {
		return nil
	}
	return r.Fallback.Decide(ctx, c)
}

// ValidateSpec is the vocabulary gate: a proposed spec must have text and only
// values drawn from shepherd's grammar. This runs before the executor sees any
// model output, so an out-of-vocabulary or malformed proposal can never act.
func ValidateSpec(s loop.Spec) error {
	if strings.TrimSpace(s.Text) == "" {
		return fmt.Errorf("empty text")
	}
	switch strings.ToUpper(s.Priority) {
	case "", "H", "M", "L":
	default:
		return fmt.Errorf("priority %q not in H|M|L", s.Priority)
	}
	if s.Category != "" && strings.ContainsAny(s.Category, " \t") {
		return fmt.Errorf("category %q must be a single token", s.Category)
	}
	if s.Due != "" && !looksLikeDate(s.Due) {
		return fmt.Errorf("due %q is not YYYY-MM-DD", s.Due)
	}
	return nil
}

// looksLikeDate accepts YYYY-MM-DD, the shape shepherd stores dates in.
func looksLikeDate(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i, ch := range s {
		if i == 4 || i == 7 {
			continue
		}
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// FallbackPolicy tries Primary first and uses Fallback only when Primary
// produces no actions — the composition the plan calls for (LLM first, rules as
// the safety net).
type FallbackPolicy struct {
	Primary  loop.Policy
	Fallback loop.Policy
}

func (p FallbackPolicy) Decide(ctx context.Context, c loop.Context) []loop.Action {
	if a := p.Primary.Decide(ctx, c); len(a) > 0 {
		return a
	}
	return p.Fallback.Decide(ctx, c)
}

// ShadowPolicy runs Shadow alongside Trusted on every event, logs how they
// differ, and returns only Trusted's actions. This is how the LLM earns trust:
// it proposes against real events without acting until the diffs look right.
type ShadowPolicy struct {
	Trusted loop.Policy
	Shadow  loop.Policy
	Logf    func(string, ...any)
}

func (p ShadowPolicy) Decide(ctx context.Context, c loop.Context) []loop.Action {
	trusted := p.Trusted.Decide(ctx, c)
	shadow := p.Shadow.Decide(ctx, c)
	if p.Logf != nil {
		p.Logf("shadow: event=%s trusted=%d shadow=%d", c.Event.Kind, len(trusted), len(shadow))
	}
	return trusted
}
