package policy

import (
	"context"

	"github.com/jwarykowski/drover/loop"
	"github.com/jwarykowski/drover/registry"
)

// BoardTrigger turns a shepherd board change into a drover-side run WITHOUT
// mutating the board. shepherd is a pure trigger source: on a change matching a
// registry action, BoardTrigger parks an agentic task in drover's OWN store (the
// FileStore), which the Dispatcher then fires and the agent reconciles back to —
// never the shepherd item. The human's board item is left exactly as they set it
// (open/done); drover tracks the run entirely on its side.
//
// The parked task carries the board item id as Link, so StoreExecutor dedups on
// it: one active run per (board item, action). A repeat board.updated while the
// run is in flight is a no-op; a fresh run fires only once the previous one is
// done (StoreExecutor.taskExists ignores completed tasks).
//
// Parked at "hold": the run waits in the dashboard's held lane until a human
// releases it (hold → go), same gate as Ingress. Board triggers are a queue of
// suggested runs the operator kicks off manually, not auto-fired.
type BoardTrigger struct {
	Registry *registry.Registry
	Hold     string // park status; defaults to "hold" (a human releases to "go")
}

func (p BoardTrigger) hold() string {
	if p.Hold == "" {
		return "hold"
	}
	return p.Hold
}

func (p BoardTrigger) Decide(_ context.Context, c loop.Context) []loop.Action {
	b, ok := c.Event.Data.(loop.BoardChange)
	if !ok || p.Registry == nil {
		return nil
	}
	it := b.Item // the changed item, straight from the watch payload
	matches := p.Registry.Match(c.Event.Type, "")
	if len(matches) == 0 {
		return nil
	}
	// First matching action only; multiple board actions of one type on a single
	// item is a niche we don't need yet.
	a := matches[0]
	return []loop.Action{loop.AddTask{Spec: loop.Spec{
		Text:    it.Text,
		Agentic: true,     // a drover-owned run, invisible to shepherd
		Status:  p.hold(), // parked for a human to release (hold → go)
		Action:  a.ID,     // reference only; the prompt lives in the registry
		Link:    it.ID,    // dedup key: one active run per (board item, action)
		Note:    it.Text,  // the agent's "title" context
		Board:   b.Board,  // origin board, so the dashboard can filter by selection
	}}}
}
