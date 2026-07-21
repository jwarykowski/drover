package policy

import (
	"context"
	"testing"

	"github.com/jwarykowski/drover/loop"
	"github.com/jwarykowski/drover/registry"
)

func TestIngressParksMatchedActions(t *testing.T) {
	reg := &registry.Registry{Actions: []registry.Action{
		{ID: "a1", On: "github.pull_request.merged", Repo: "acme/api"},
		{ID: "a2", On: "github.pull_request.merged"}, // no repo filter — also matches
		{ID: "a3", On: "sentry.issue.opened"},        // wrong type
	}}
	p := Ingress{Registry: reg}
	acts := p.Decide(context.Background(), loop.Context{Event: loop.Event{
		Type: "github.pull_request.merged",
		Data: loop.Signal{Repo: "acme/api", Title: "bump", URL: "u"},
	}})
	if len(acts) != 2 {
		t.Fatalf("want 2 parked tasks (a1 + a2), got %d", len(acts))
	}
	add, ok := acts[0].(loop.AddTask)
	if !ok {
		t.Fatalf("want AddTask, got %T", acts[0])
	}
	if !add.Spec.Agentic || add.Spec.Status != "hold" {
		t.Fatalf("not held agentic: %+v", add.Spec)
	}
	if add.Spec.Action != "a1" {
		t.Fatalf("want action id a1, got %q", add.Spec.Action)
	}
	if add.Spec.Link != "u" {
		t.Fatalf("link not carried: %q", add.Spec.Link)
	}
}

func TestIngressIgnoresNonSignalAndNoMatch(t *testing.T) {
	reg := &registry.Registry{Actions: []registry.Action{{ID: "a1", On: "github.issues.opened"}}}
	p := Ingress{Registry: reg}
	if acts := p.Decide(context.Background(), loop.Context{Event: loop.Event{
		Type: "board.updated", Data: loop.BoardChange{},
	}}); acts != nil {
		t.Fatalf("board change should not park: %#v", acts)
	}
	if acts := p.Decide(context.Background(), loop.Context{Event: loop.Event{
		Type: "github.pull_request.merged", Data: loop.Signal{Repo: "x"},
	}}); acts != nil {
		t.Fatalf("no matching row should be nil: %#v", acts)
	}
}
