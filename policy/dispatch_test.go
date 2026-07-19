package policy

import (
	"context"
	"testing"

	"github.com/jwarykowski/drover/loop"
)

func TestDispatcherFiresReleasedAgentic(t *testing.T) {
	p := Dispatcher{}
	it := loop.Item{ID: "t1", Agentic: true, Status: "go", Action: "a1", Note: "bump", Link: "u"}
	acts := p.Decide(context.Background(), loop.Context{Event: loop.Event{
		Type: "board.updated", Data: loop.BoardChange{Item: it},
	}})
	if len(acts) != 2 {
		t.Fatalf("want claim + run, got %d", len(acts))
	}
	ss, ok := acts[0].(loop.SetStatus)
	if !ok || ss.Status != "running" || ss.ID != "t1" {
		t.Fatalf("claim wrong: %#v", acts[0])
	}
	ra, ok := acts[1].(loop.RunAgent)
	if !ok || ra.ActionID != "a1" || ra.TaskID != "t1" {
		t.Fatalf("run wrong: %#v", acts[1])
	}
	if ra.Args["title"] != "bump" || ra.Args["url"] != "u" {
		t.Fatalf("args not from item: %#v", ra.Args)
	}
}

func TestDispatcherIgnores(t *testing.T) {
	p := Dispatcher{}
	cases := []loop.Item{
		{ID: "1", Agentic: false, Status: "go", Action: "a"},  // not agentic
		{ID: "2", Agentic: true, Status: "hold", Action: "a"}, // not released
		{ID: "3", Agentic: true, Status: "go", Action: ""},    // no action
	}
	for _, it := range cases {
		if acts := p.Decide(context.Background(), loop.Context{Event: loop.Event{
			Type: "board.updated", Data: loop.BoardChange{Item: it},
		}}); acts != nil {
			t.Fatalf("item %s should not fire: %#v", it.ID, acts)
		}
	}
	if acts := p.Decide(context.Background(), loop.Context{Event: loop.Event{
		Type: "github.pull_request.merged", Data: loop.Signal{},
	}}); acts != nil {
		t.Fatalf("a signal should not dispatch: %#v", acts)
	}
}
