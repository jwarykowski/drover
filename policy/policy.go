// Package policy is drover's think step: a pure function from an event and the
// current tasks to the actions to apply. No I/O, so it stays table-testable.
package policy

import (
	"context"
	"fmt"

	"github.com/jwarykowski/drover/config"
	"github.com/jwarykowski/drover/loop"
)

// Policy handles every event with one rule set, because after the payload sum
// type went away there is only one event shape to reason about. It does two
// things:
//
//   - a task.updated re-drive dispatches a task a human has released;
//   - anything else is an ingested event: match it against the config and park
//     one run per matching action.
//
// It is data-driven — teaching drover to react to a new source is a drover.toml
// row, not a new policy.
type Policy struct {
	Config  *config.Config
	Hold    string // park status; defaults to "hold"
	Ready   string // release status a human sets; defaults to "go"
	Running string // claim status; defaults to "running"
}

func (p Policy) hold() string    { return or(p.Hold, "hold") }
func (p Policy) ready() string   { return or(p.Ready, "go") }
func (p Policy) running() string { return or(p.Running, "running") }

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (p Policy) Decide(_ context.Context, c loop.Context) []loop.Action {
	if p.Config == nil {
		return nil
	}
	if c.Event.Type == loop.TaskUpdated {
		return p.dispatch(c)
	}
	return p.park(c)
}

// park turns an ingested event into held runs, one per matching action, each
// carrying that action's id and the event's data.
//
// The task waits at hold until a human releases it — the review gate that makes
// running an agent on event text drover did not author safe. An action marked
// auto skips that wait by parking straight at the release status, so the
// re-drive dispatches it on the next tick: same path, no separate auto-fire
// branch to keep correct.
func (p Policy) park(c loop.Context) []loop.Action {
	status := p.hold()
	var acts []loop.Action
	for _, a := range p.Config.Match(c.Event.Type, c.Event.Data) {
		s := status
		if a.Auto {
			s = p.ready()
		}
		acts = append(acts, loop.AddTask{Spec: loop.Spec{
			Text:    taskText(c.Event),
			Status:  s,
			Type:    c.Event.Type,     // the raising event's type, carried for diagnostics
			Action:  a.ID,             // reference only; the prompt lives in the config
			Subject: subject(c.Event), // one run per (subject, action) at a time
			Source:  c.Event.Source,
			Link:    c.Event.Data["url"],
			Note:    c.Event.Data["title"],
			Data:    c.Event.Data, // replayed into the prompt and the target template
		}})
	}
	return acts
}

// dispatch fires a released task: it claims the task (so a restart or a replayed
// re-drive will not fire it twice) and emits the RunAgent the executor resolves.
// The closing status is not emitted here — the outcome depends on the agent's
// verdict, which only the executor sees.
func (p Policy) dispatch(c loop.Context) []loop.Action {
	id := c.Event.Data["task_id"]
	if id == "" {
		return nil
	}
	// Claim on the task's LIVE status from the assembled slice, not the status
	// carried in the event: a re-drive can report `go` for a task already
	// claimed `running`, and trusting it would double-fire. Resolving live makes
	// a replay idempotent while a genuine re-release still fires. Absent from
	// the live slice (done or deleted) means don't fire.
	t, ok := liveTask(c.Tasks, id)
	if !ok || t.Action == "" || t.Status != p.ready() {
		return nil
	}
	return []loop.Action{
		loop.SetStatus{ID: t.ID, Status: p.running()},
		loop.RunAgent{ActionID: t.Action, TaskID: t.ID, Data: t.Data},
	}
}

// taskText labels the run for the lanes: the event's own title where it has
// one, prefixed by whichever identifier the source considers primary.
func taskText(e loop.Event) string {
	title := e.Data["title"]
	if title == "" {
		return e.Type
	}
	for _, k := range []string{"repo", "project", "board"} {
		if v := e.Data[k]; v != "" {
			return fmt.Sprintf("%s: %s", v, title)
		}
	}
	return title
}

// subject is the stable identity of the thing an event concerns, used to keep
// one run per (subject, action) in flight. Sources that name it explicitly win;
// a url is the next best stable handle; the event id is the last resort, and
// being unique per event it simply never dedups — which is the right answer for
// a source that has no notion of a recurring subject.
func subject(e loop.Event) string {
	if s := e.Data["subject"]; s != "" {
		return s
	}
	if u := e.Data["url"]; u != "" {
		return u
	}
	return e.ID
}

func liveTask(tasks []loop.Task, id string) (loop.Task, bool) {
	for _, t := range tasks {
		if t.ID == id {
			return t, true
		}
	}
	return loop.Task{}, false
}
