package policy

import (
	"fmt"

	"context"

	"github.com/jwarykowski/drover/loop"
)

// MergeFlowPolicy runs the closed loop: an upstream merge becomes a held agentic
// task on the board; a human releases it by toggling its status; drover then
// claims it, fires the configured skill, and closes it.
//
//	github.merged                 -> AddTask{@marker, status:Hold, link:url, note:title}
//	board.updated & agentic & Ready -> [SetStatus Running, RunAction, SetStatus done]
//
// The status lifecycle is meaningful ONLY on agentic tasks — those drover raised,
// tagged with Marker. A hand-added item that happens to sit at status Ready is
// ignored, so the board's own todos never trigger the runner.
type MergeFlowPolicy struct {
	Action  string // allowlist action fired on release, e.g. "run-skill"
	Marker  string // category tagging agentic tasks; defaults to "agentic"
	Hold    string // status a new task is parked at; defaults to "hold"
	Ready   string // status a human sets to release; defaults to "go"
	Running string // status drover claims with; defaults to "running"
}

func (p MergeFlowPolicy) marker() string  { return orDefault(p.Marker, "agentic") }
func (p MergeFlowPolicy) hold() string    { return orDefault(p.Hold, "hold") }
func (p MergeFlowPolicy) ready() string   { return orDefault(p.Ready, "go") }
func (p MergeFlowPolicy) running() string { return orDefault(p.Running, "running") }

func (p MergeFlowPolicy) Decide(_ context.Context, c loop.Context) []loop.Action {
	switch c.Event.Kind {
	case "github.merged":
		repo, title := str(c.Event.Payload["repo"]), str(c.Event.Payload["title"])
		return []loop.Action{loop.AddTask{Spec: loop.Spec{
			Text:     fmt.Sprintf("%s merged: %s", repo, title),
			Category: p.marker(),
			Status:   p.hold(),
			Link:     str(c.Event.Payload["url"]),
			Note:     title,
		}}}

	case "board.added", "board.updated":
		it, ok := c.Event.Payload["item"].(loop.Item)
		// Only agentic tasks the human has released fire the skill.
		if !ok || it.Category != p.marker() || it.Status != p.ready() {
			return nil
		}
		return []loop.Action{
			loop.SetStatus{ID: it.ID, Status: p.running()}, // claim: a restart won't re-fire
			loop.RunAction{
				Name:   p.Action,
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
