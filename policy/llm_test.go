package policy

import (
	"context"
	"errors"
	"testing"

	"github.com/jwarykowski/drover/loop"
)

// fakeReasoner returns canned proposals (or an error) — no SDK, no network.
type fakeReasoner struct {
	specs []loop.Spec
	err   error
}

func (f fakeReasoner) Propose(context.Context, loop.Event, []loop.Item, []byte) ([]loop.Spec, error) {
	return f.specs, f.err
}

func TestLLMReasonerValidatesProposals(t *testing.T) {
	r := LLMReasoner{Reasoner: fakeReasoner{specs: []loop.Spec{
		{Text: "good task", Category: "ci", Priority: "H"},
		{Text: "", Priority: "H"},        // no text — dropped
		{Text: "bad prio", Priority: "X"}, // bad priority — dropped
		{Text: "bad date", Due: "soon"},   // bad date — dropped
	}}}

	got := r.Decide(context.Background(), loop.Context{Event: loop.Event{Kind: "ci.failed"}})
	if len(got) != 1 {
		t.Fatalf("want 1 valid action, got %d", len(got))
	}
	if add := got[0].(loop.AddTask); add.Spec.Text != "good task" {
		t.Errorf("wrong surviving spec: %+v", add.Spec)
	}
}

func TestLLMReasonerFallsBackOnError(t *testing.T) {
	r := LLMReasoner{
		Reasoner: fakeReasoner{err: errors.New("model down")},
		Fallback: RulesPolicy{},
	}
	c := loop.Context{Event: loop.Event{
		Kind:    "ci.failed",
		Payload: map[string]any{"title": "boom", "link": "https://ci/1"},
	}}
	got := r.Decide(context.Background(), c)
	if len(got) != 1 {
		t.Fatalf("fallback: want 1 action from RulesPolicy, got %d", len(got))
	}
}

func TestShadowPolicyReturnsTrustedOnly(t *testing.T) {
	var logged bool
	p := ShadowPolicy{
		Trusted: RulesPolicy{},
		Shadow:  LLMReasoner{Reasoner: fakeReasoner{specs: []loop.Spec{{Text: "shadow only"}}}},
		Logf:    func(string, ...any) { logged = true },
	}
	c := loop.Context{Event: loop.Event{Kind: "ci.failed", Payload: map[string]any{"title": "x"}}}
	got := p.Decide(context.Background(), c)
	// Trusted (rules) produces the ci task; shadow's proposal never acts.
	if len(got) != 1 {
		t.Fatalf("want trusted's 1 action, got %d", len(got))
	}
	if add := got[0].(loop.AddTask); add.Spec.Category != "ci" {
		t.Errorf("returned shadow action, not trusted: %+v", add.Spec)
	}
	if !logged {
		t.Error("shadow diff was not logged")
	}
}

func TestValidateSpec(t *testing.T) {
	ok := []loop.Spec{{Text: "a"}, {Text: "b", Priority: "m"}, {Text: "c", Due: "2026-08-01"}}
	for _, s := range ok {
		if err := ValidateSpec(s); err != nil {
			t.Errorf("ValidateSpec(%+v): unexpected error %v", s, err)
		}
	}
	bad := []loop.Spec{{}, {Text: "x", Priority: "z"}, {Text: "x", Category: "a b"}, {Text: "x", Due: "2026/08/01"}}
	for _, s := range bad {
		if err := ValidateSpec(s); err == nil {
			t.Errorf("ValidateSpec(%+v): want error, got nil", s)
		}
	}
}
