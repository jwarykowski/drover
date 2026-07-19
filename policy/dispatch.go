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
	it := b.Item
	// Only an agentic task the human has released, with an action to fire.
	if !it.Agentic || it.Action == "" || it.Status != p.ready() {
		return nil
	}
	return []loop.Action{
		loop.SetStatus{ID: it.ID, Status: p.running()},
		loop.RunAgent{
			ActionID: it.Action,
			TaskID:   it.ID,
			Args:     map[string]string{"title": it.Note, "url": it.Link, "id": it.ID},
		},
	}
}
