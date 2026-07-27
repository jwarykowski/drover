package policy

import (
	"context"
	"testing"

	"github.com/jwarykowski/drover/loop"
	"github.com/jwarykowski/drover/registry"
)

func boardReg() *registry.Registry {
	return &registry.Registry{Actions: []registry.Action{
		{ID: "b1", On: "board.updated", Target: "/tmp", Do: "x"},
	}}
}

// BoardTrigger parks a drover-owned run; it never mutates shepherd, so the only
// action it emits is an AddTask into the FileStore.
func TestBoardTriggerParksDroverRun(t *testing.T) {
	p := BoardTrigger{Registry: boardReg()}
	it := loop.Item{ID: "h1", Status: "", Text: "triage me", Link: "u"}
	acts := p.Decide(context.Background(), fire(it))
	if len(acts) != 1 {
		t.Fatalf("want a single AddTask, got %d: %#v", len(acts), acts)
	}
	add, ok := acts[0].(loop.AddTask)
	if !ok {
		t.Fatalf("want AddTask, got %#v", acts[0])
	}
	if !add.Spec.Agentic {
		t.Error("parked run must be agentic (drover-owned, invisible to shepherd)")
	}
	if add.Spec.Status != "hold" {
		t.Errorf("board run is parked for manual release → hold, got %q", add.Spec.Status)
	}
	if add.Spec.Action != "b1" {
		t.Errorf("must carry the matched action id, got %q", add.Spec.Action)
	}
	if add.Spec.Link != "h1" {
		t.Errorf("Link must be the board item id (dedup key), got %q", add.Spec.Link)
	}
	if add.Spec.Note != "triage me" {
		t.Errorf("Note (agent title) should be the item text, got %q", add.Spec.Note)
	}
}

// No SetStatus, ever: the shepherd board is a pure trigger and must not be
// written back.
func TestBoardTriggerNeverWritesShepherd(t *testing.T) {
	p := BoardTrigger{Registry: boardReg()}
	acts := p.Decide(context.Background(), fire(loop.Item{ID: "h1", Text: "t"}))
	for _, a := range acts {
		if _, ok := a.(loop.SetStatus); ok {
			t.Fatalf("BoardTrigger must not emit SetStatus (would mutate shepherd): %#v", a)
		}
		if _, ok := a.(loop.RunAgent); ok {
			t.Fatalf("BoardTrigger parks a task; the Dispatcher fires it, not BoardTrigger: %#v", a)
		}
	}
}

func TestBoardTriggerIgnores(t *testing.T) {
	p := BoardTrigger{Registry: boardReg()}
	// No registry action for this type.
	noMatch := loop.Context{
		Event: loop.Event{Type: "board.added", Data: loop.BoardChange{Item: loop.Item{ID: "a", Text: "t"}}},
	}
	if acts := p.Decide(context.Background(), noMatch); acts != nil {
		t.Fatalf("unmatched type must not fire: %#v", acts)
	}
	// Wrong event payload shape (a Signal, not a BoardChange).
	bad := loop.Context{Event: loop.Event{Type: "board.updated", Data: loop.Signal{}}}
	if acts := p.Decide(context.Background(), bad); acts != nil {
		t.Fatalf("non-BoardChange payload must not fire: %#v", acts)
	}
}

// A terminal change (item archived/removed) still parks a run if an action
// listens on that type — the item id is in the event payload, no live board
// needed.
func TestBoardTriggerTerminalParks(t *testing.T) {
	for _, ev := range []string{"board.removed", "board.archived"} {
		reg := &registry.Registry{Actions: []registry.Action{{ID: "t1", On: ev, Target: "/tmp", Do: "x"}}}
		p := BoardTrigger{Registry: reg}
		it := loop.Item{ID: "g1", Status: "done", Text: "wrap up"}
		acts := p.Decide(context.Background(), loop.Context{
			Event: loop.Event{Type: ev, Data: loop.BoardChange{Item: it}},
		})
		if len(acts) != 1 {
			t.Fatalf("%s: want a single AddTask, got %d: %#v", ev, len(acts), acts)
		}
		if add := acts[0].(loop.AddTask); add.Spec.Action != "t1" || add.Spec.Link != "g1" {
			t.Fatalf("%s: parked spec wrong: %#v", ev, add.Spec)
		}
	}
}
