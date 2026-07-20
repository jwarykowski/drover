package policy

import (
	"context"

	"github.com/jwarykowski/drover/loop"
)

// Dispatcher fires a released agentic task, for ALL sources. When a human flips
// an agentic task to Ready, it claims the task (Running, so a restart won't
// re-fire) and emits a RunAgent carrying the task's action id and event context.
// It does NOT emit the closing status: the outcome (done vs left running)
// depends on the agent's verdict, which only the executor sees — so the close
// lives there.
type Dispatcher struct {
	Ready   string // release status a human sets; defaults to "go"
	Running string // claim status; defaults to "running"
}

func (p Dispatcher) ready() string {
	if p.Ready == "" {
		return "go"
	}
	return p.Ready
}

func (p Dispatcher) running() string {
	if p.Running == "" {
		return "running"
	}
	return p.Running
}

func (p Dispatcher) Decide(_ context.Context, c loop.Context) []loop.Action {
	b, ok := c.Event.Data.(loop.BoardChange)
	if !ok {
		return nil
	}
	// Claim on the item's LIVE status from the assembled board, not the status
	// embedded in the event: a snapshot replayed on reconnect can report `go` for
	// a task drover already claimed `running`, and trusting it would double-fire.
	// Resolving live makes a replay idempotent while a genuine re-release (a
	// non-running task back at `go`) still fires. Absent from the live board (done
	// or removed) means don't fire.
	it, ok := liveItem(c.Board, b.Item.ID)
	if !ok {
		return nil
	}
	if !it.Agentic || it.Action == "" || it.Status != p.ready() {
		return nil
	}
	// ponytail: tiny TOCTOU between this read and the running write — shepherd has
	// no atomic CAS. Window is two CLI calls vs. a 20-min agent run, so acceptable.
	return []loop.Action{
		loop.SetStatus{ID: it.ID, Status: p.running()},
		loop.RunAgent{
			ActionID: it.Action,
			TaskID:   it.ID,
			Args:     map[string]string{"title": it.Note, "url": it.Link, "id": it.ID},
		},
	}
}

func liveItem(board []loop.Item, id string) (loop.Item, bool) {
	for _, it := range board {
		if it.ID == id {
			return it, true
		}
	}
	return loop.Item{}, false
}
