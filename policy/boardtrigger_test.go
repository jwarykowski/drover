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

func TestBoardTriggerFiresOnHumanOpenItem(t *testing.T) {
	p := BoardTrigger{Registry: boardReg()}
	it := loop.Item{ID: "h1", Agentic: false, Status: "", Text: "triage me", Link: "u"}
	acts := p.Decide(context.Background(), fire(it))
	if len(acts) != 2 {
		t.Fatalf("want claim + run, got %d: %#v", len(acts), acts)
	}
	ss, ok := acts[0].(loop.SetStatus)
	if !ok || ss.Status != "running" || ss.ID != "h1" {
		t.Fatalf("claim wrong: %#v", acts[0])
	}
	ra, ok := acts[1].(loop.RunAgent)
	if !ok || ra.ActionID != "b1" || ra.TaskID != "h1" {
		t.Fatalf("run wrong: %#v", acts[1])
	}
	if ra.Args["title"] != "triage me" {
		t.Fatalf("title should be the item text: %#v", ra.Args)
	}
}

func TestBoardTriggerIgnores(t *testing.T) {
	p := BoardTrigger{Registry: boardReg()}
	// Agentic items belong to Dispatcher — the trust boundary.
	if acts := p.Decide(context.Background(), fire(loop.Item{ID: "1", Agentic: true, Status: ""})); acts != nil {
		t.Fatalf("agentic item must not fire BoardTrigger: %#v", acts)
	}
	// Claimed/terminal states are not open → no re-fire on reconcile writes.
	for _, s := range []string{"running", "done", "hold", "go"} {
		it := loop.Item{ID: "x", Agentic: false, Status: s, Text: "t"}
		if acts := p.Decide(context.Background(), fire(it)); acts != nil {
			t.Fatalf("status %q must not fire: %#v", s, acts)
		}
	}
	// No registry action for this type.
	noMatch := loop.Context{
		Event: loop.Event{Type: "board.added", Data: loop.BoardChange{Item: loop.Item{ID: "a"}}},
		Board: []loop.Item{{ID: "a", Text: "t"}},
	}
	if acts := p.Decide(context.Background(), noMatch); acts != nil {
		t.Fatalf("unmatched type must not fire: %#v", acts)
	}
	// Absent from the live board (board.removed) → nothing to run against.
	absent := loop.Context{Event: loop.Event{Type: "board.updated", Data: loop.BoardChange{Item: loop.Item{ID: "gone"}}}}
	if acts := p.Decide(context.Background(), absent); acts != nil {
		t.Fatalf("absent item must not fire: %#v", acts)
	}
}

func termReg(on string) *registry.Registry {
	return &registry.Registry{Actions: []registry.Action{
		{ID: "t1", On: on, Target: "/tmp", Do: "x"},
	}}
}

func TestBoardTriggerTerminalFiresAndForgets(t *testing.T) {
	for _, ev := range []string{"board.removed", "board.archived"} {
		p := BoardTrigger{Registry: termReg(ev)}
		// Terminal item is NOT on the live board — only in the event payload.
		it := loop.Item{ID: "g1", Agentic: false, Status: "done", Text: "wrap up", Link: "u"}
		acts := p.Decide(context.Background(), loop.Context{
			Event: loop.Event{Type: ev, Data: loop.BoardChange{Item: it}},
			Board: nil, // gone from the live board
		})
		if len(acts) != 1 {
			t.Fatalf("%s: want a single detached run, got %d: %#v", ev, len(acts), acts)
		}
		ra, ok := acts[0].(loop.RunAgent)
		if !ok || ra.ActionID != "t1" {
			t.Fatalf("%s: run wrong: %#v", ev, acts[0])
		}
		if ra.TaskID != "" {
			t.Fatalf("%s: terminal run must have empty TaskID (no reconcile), got %q", ev, ra.TaskID)
		}
		if ra.Args["title"] != "wrap up" {
			t.Fatalf("%s: args should come from the event payload: %#v", ev, ra.Args)
		}
	}
}

func TestBoardTriggerTerminalRespectsTrustBoundary(t *testing.T) {
	p := BoardTrigger{Registry: termReg("board.archived")}
	it := loop.Item{ID: "a1", Agentic: true, Status: "done", Text: "parked"} // untrusted lane
	acts := p.Decide(context.Background(), loop.Context{
		Event: loop.Event{Type: "board.archived", Data: loop.BoardChange{Item: it}},
	})
	if acts != nil {
		t.Fatalf("agentic (Ingress-parked) item must not fire even on archive: %#v", acts)
	}
}

// Dispatcher and BoardTrigger share the board. prefix via Chain; on any one item
// exactly one of them acts, so results never double-fire.
func TestChainBoardMutuallyExclusive(t *testing.T) {
	ch := Chain{Dispatcher{}, BoardTrigger{Registry: boardReg()}}

	agentic := loop.Item{ID: "t1", Agentic: true, Status: "go", Action: "a1"}
	acts := ch.Decide(context.Background(), fire(agentic))
	if len(acts) != 2 {
		t.Fatalf("agentic go item: only Dispatcher should act, got %d: %#v", len(acts), acts)
	}
	if ra := acts[1].(loop.RunAgent); ra.ActionID != "a1" {
		t.Fatalf("Dispatcher should fire the task's action id: %#v", ra)
	}

	human := loop.Item{ID: "h1", Agentic: false, Status: "", Text: "t"}
	acts = ch.Decide(context.Background(), fire(human))
	if len(acts) != 2 {
		t.Fatalf("human open item: only BoardTrigger should act, got %d: %#v", len(acts), acts)
	}
	if ra := acts[1].(loop.RunAgent); ra.ActionID != "b1" {
		t.Fatalf("BoardTrigger should fire the matched registry action: %#v", ra)
	}
}
