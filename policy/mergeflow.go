package policy

import (
	"context"
	"fmt"

	"github.com/jwarykowski/drover/loop"
)

// MergeFlowPolicy runs the closed loop: an upstream merge becomes a held agentic
// task on the board; a human releases it by toggling its status; drover then
// claims it, fires the action the task names, and closes it.
//
//	github.merged                    -> AddTask{agentic, status:Hold, action:Action, link, note}
//	board.updated & agentic & Ready  -> [SetStatus Running, RunAction(item.Action), SetStatus done]
//
// The status lifecycle is meaningful ONLY on agentic tasks — shepherd's `agentic`
// flag (0.19.0+), not an overloaded category. A hand-added item that happens to
// sit at status Ready is ignored, so the board's own todos never trigger the
// runner. The action to fire is read from the task's own `action` field, so
// different tasks can drive different actions; Action is the default stamped onto
// tasks this policy raises and the fallback when a released task names none.
type MergeFlowPolicy struct {
	Action  string // default action stamped on raised tasks / fallback on release
	Hold    string // status a new task is parked at; defaults to "hold"
	Ready   string // status a human sets to release; defaults to "go"
	Running string // status drover claims with; defaults to "running"
}

func (p MergeFlowPolicy) hold() string    { return orDefault(p.Hold, "hold") }
func (p MergeFlowPolicy) ready() string   { return orDefault(p.Ready, "go") }
func (p MergeFlowPolicy) running() string { return orDefault(p.Running, "running") }

func (p MergeFlowPolicy) Decide(_ context.Context, c loop.Context) []loop.Action {
	switch c.Event.Kind {
	case "github.merged":
		repo, title := str(c.Event.Payload["repo"]), str(c.Event.Payload["title"])
		return []loop.Action{loop.AddTask{Spec: loop.Spec{
			Text:    fmt.Sprintf("%s merged: %s", repo, title),
			Agentic: true,
			Status:  p.hold(),
			Action:  p.Action,
			Link:    str(c.Event.Payload["url"]),
			Note:    title,
		}}}

	case "board.added", "board.updated":
		it, ok := c.Event.Payload["item"].(loop.Item)
		// Only agentic tasks the human has released fire the action.
		if !ok || !it.Agentic || it.Status != p.ready() {
			return nil
		}
		action := it.Action
		if action == "" {
			action = p.Action // task named none — fall back to the policy default
		}
		return []loop.Action{
			loop.SetStatus{ID: it.ID, Status: p.running()}, // claim: a restart won't re-fire
			loop.RunAction{
				Name:   action,
				Args:   map[string]string{"title": it.Note, "url": it.Link, "id": it.ID},
				Reason: "merge release " + it.ID,
			},
			loop.SetStatus{ID: it.ID, Status: "done"}, // close: loop back
		}

	default:
		return nil
	}
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
