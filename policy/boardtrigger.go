package policy

import (
	"context"

	"github.com/jwarykowski/drover/loop"
	"github.com/jwarykowski/drover/registry"
)

// BoardTrigger fires a registry action on a board change, matching by event type
// the way Ingress matches github types. It has NO hold→go human gate, so it is
// scoped to human-authored items (Agentic==false): an Ingress-parked task carries
// untrusted upstream text and is fired only by the gated Dispatcher.
//
// It handles two lifecycle phases differently:
//
//   - Live (board.added / board.updated): the item is still on the board, so
//     BoardTrigger claims it Running and RunAgent reconciles the verdict back.
//     Idempotency: the agent's own reconcile writes arrive as more board.updated
//     events, so it fires only while the item is still OPEN (unclaimed); once
//     claimed the item no longer qualifies. Fires at most once per open→run.
//   - Terminal (board.removed / board.archived): the item is off the live board,
//     so BoardTrigger reads it from the event payload and fires-and-forget — no
//     claim, no reconcile (empty TaskID). The transition emits once, so no loop.
//
// a board.added action ALSO fires on any follow-up todo a fired agent
// creates (they land as human-authored open items) — a cascade. Prefer
// board.updated, or don't emit followups from a board.added action.
type BoardTrigger struct {
	Registry *registry.Registry
	Running  string // claim status; defaults to "running"
}

func (p BoardTrigger) running() string {
	if p.Running == "" {
		return "running"
	}
	return p.Running
}

func (p BoardTrigger) Decide(_ context.Context, c loop.Context) []loop.Action {
	b, ok := c.Event.Data.(loop.BoardChange)
	if !ok || p.Registry == nil {
		return nil
	}

	terminal := isTerminal(c.Event.Type)
	it := b.Item // terminal items are off the live board; use the event payload
	if !terminal {
		var ok bool
		if it, ok = liveItem(c.Board, b.Item.ID); !ok {
			return nil
		}
		// Unclaimed only, so drover's own reconcile writes don't re-fire it.
		if !openStatus(it.Status) {
			return nil
		}
	}
	if it.Agentic { // trust boundary: never auto-fire on Ingress-parked text
		return nil
	}
	matches := p.Registry.Match(c.Event.Type, "")
	if len(matches) == 0 {
		return nil
	}
	// first matching action only; multiple board actions of one type
	// on a single item is a niche we don't need yet.
	a := matches[0]
	args := map[string]string{"title": it.Text, "url": it.Link, "id": it.ID}
	if terminal {
		// Fire-and-forget: empty TaskID tells the executor to skip reconcile.
		return []loop.Action{loop.RunAgent{ActionID: a.ID, TaskID: "", Args: args}}
	}
	return []loop.Action{
		loop.SetStatus{ID: it.ID, Status: p.running()},
		loop.RunAgent{ActionID: a.ID, TaskID: it.ID, Args: args},
	}
}

// isTerminal reports whether the event is a terminal board transition — the item
// has left the active board (removed or archived), so there is nothing to claim
// or reconcile.
func isTerminal(evType string) bool {
	return evType == "board.removed" || evType == "board.archived"
}

// openStatus reports whether an item is unclaimed — not in a drover claim/gate
// or terminal state. Anything else (empty default, a human label like "todo")
// is eligible to trigger once.
func openStatus(s string) bool {
	switch s {
	case "hold", "go", "running", "done":
		return false
	}
	return true
}
