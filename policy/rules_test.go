package policy

import (
	"context"
	"testing"

	"github.com/jwarykowski/drover/loop"
)

func TestDecide(t *testing.T) {
	p := RulesPolicy{}

	ciFail := loop.Context{Event: loop.Event{
		Type: "ci.failed",
		Data: loop.Generic{"title": "build broke", "link": "https://ci/42"},
	}}
	got := p.Decide(context.Background(), ciFail)
	if len(got) != 1 {
		t.Fatalf("ci.failed: want 1 action, got %d", len(got))
	}
	add, ok := got[0].(loop.AddTask)
	if !ok {
		t.Fatalf("want AddTask, got %T", got[0])
	}
	if add.Spec.Category != "ci" || add.Spec.Priority != "H" || add.Spec.Link != "https://ci/42" {
		t.Errorf("wrong spec: %+v", add.Spec)
	}
	if add.Spec.Text != "CI failed: build broke" {
		t.Errorf("wrong text: %q", add.Spec.Text)
	}

	if got := p.Decide(context.Background(), loop.Context{Event: loop.Event{Type: "unknown"}}); got != nil {
		t.Errorf("unknown type: want nil, got %v", got)
	}
}
