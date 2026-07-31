package policy

import (
	"context"
	"testing"

	"github.com/jwarykowski/drover/config"
	"github.com/jwarykowski/drover/loop"
)

func cfg(actions ...config.Action) *config.Config {
	c := &config.Config{}
	c.Action = actions
	return c
}

func decide(p Policy, c loop.Context) []loop.Action {
	return p.Decide(context.Background(), c)
}

func TestParksOneHeldRunPerMatchingAction(t *testing.T) {
	p := Policy{Config: cfg(
		config.Action{ID: "a1", On: "github.pull_request.merged"},
		config.Action{ID: "a2", On: "github.pull_request.merged"},
		config.Action{ID: "a3", On: "github.issues.opened"},
	)}
	acts := decide(p, loop.Context{Event: loop.Event{
		ID: "e1", Type: "github.pull_request.merged", Source: "acme-api",
		Data: map[string]string{"repo": "acme/api", "title": "fix login", "url": "u", "subject": "u"},
	}})
	if len(acts) != 2 {
		t.Fatalf("want one run per matching action, got %d", len(acts))
	}
	add, ok := acts[0].(loop.AddTask)
	if !ok {
		t.Fatalf("want AddTask, got %T", acts[0])
	}
	s := add.Spec
	if s.Status != "hold" {
		t.Errorf("a run must park held for a human, got %q", s.Status)
	}
	if s.Action != "a1" {
		t.Errorf("action reference not carried: %q", s.Action)
	}
	if s.Source != "acme-api" {
		t.Errorf("provenance not recorded: %+v", s)
	}
	if s.Subject != "u" {
		t.Errorf("subject not carried: %q", s.Subject)
	}
	if s.Type != "github.pull_request.merged" {
		t.Errorf("event type not carried: %q", s.Type)
	}
	if s.Data["repo"] != "acme/api" {
		t.Errorf("event data must ride along for the prompt: %v", s.Data)
	}
	if s.Text != "acme/api: fix login" {
		t.Errorf("lane label = %q", s.Text)
	}
}

// The where filter is what makes a non-GitHub source first class: it matches on
// any event key, not a hard-coded repo field.
func TestWhereFilterNarrowsOnAnyKey(t *testing.T) {
	p := Policy{Config: cfg(
		config.Action{ID: "eng", On: "linear.issue.labeled", Where: map[string]string{"team": "ENG", "label": "p0"}},
	)}
	ev := func(data map[string]string) loop.Context {
		return loop.Context{Event: loop.Event{ID: "e", Type: "linear.issue.labeled", Data: data}}
	}
	if got := decide(p, ev(map[string]string{"team": "ENG", "label": "p0"})); len(got) != 1 {
		t.Fatalf("all filter keys matching should park a run, got %d", len(got))
	}
	if got := decide(p, ev(map[string]string{"team": "ENG", "label": "p2"})); len(got) != 0 {
		t.Fatalf("a mismatched key must not match, got %d", len(got))
	}
	// An absent key can never satisfy a non-empty filter — a filter only narrows.
	if got := decide(p, ev(map[string]string{"team": "ENG"})); len(got) != 0 {
		t.Fatalf("a missing key must not match, got %d", len(got))
	}
}

// auto parks at the release status instead of hold, so the existing re-drive
// dispatches it — there is no separate auto-fire path to keep correct.
func TestAutoParksReleasedNotHeld(t *testing.T) {
	p := Policy{Config: cfg(config.Action{ID: "a1", On: "demo.ping", Auto: true})}
	acts := decide(p, loop.Context{Event: loop.Event{ID: "e1", Type: "demo.ping"}})
	if len(acts) != 1 {
		t.Fatalf("want 1 action, got %d", len(acts))
	}
	if got := acts[0].(loop.AddTask).Spec.Status; got != "go" {
		t.Fatalf("auto action should park released, got %q", got)
	}
}

func TestSubjectFallsBackUrlThenEventID(t *testing.T) {
	p := Policy{Config: cfg(config.Action{ID: "a1", On: "demo.ping"})}
	spec := func(data map[string]string) loop.Spec {
		acts := decide(p, loop.Context{Event: loop.Event{ID: "e1", Type: "demo.ping", Data: data}})
		return acts[0].(loop.AddTask).Spec
	}
	if got := spec(map[string]string{"subject": "S-1", "url": "u"}).Subject; got != "S-1" {
		t.Errorf("an explicit subject wins, got %q", got)
	}
	if got := spec(map[string]string{"url": "u"}).Subject; got != "u" {
		t.Errorf("url is the next best handle, got %q", got)
	}
	// No stable handle: the event id is unique per event, so this simply never
	// dedups — the right answer for a source with no recurring subject.
	if got := spec(nil).Subject; got != "e1" {
		t.Errorf("event id is the last resort, got %q", got)
	}
}

func TestDispatchClaimsAndRunsAReleasedTask(t *testing.T) {
	p := Policy{Config: cfg()}
	c := loop.Context{
		Event: loop.Event{ID: "r1", Type: loop.TaskUpdated, Data: map[string]string{"task_id": "t1"}},
		Tasks: []loop.Task{{ID: "t1", Status: "go", Action: "a1", Data: map[string]string{"title": "x"}}},
	}
	acts := decide(p, c)
	if len(acts) != 2 {
		t.Fatalf("want claim + run, got %d", len(acts))
	}
	set, ok := acts[0].(loop.SetStatus)
	if !ok || set.ID != "t1" || set.Status != "running" {
		t.Fatalf("first action must claim the task: %#v", acts[0])
	}
	run, ok := acts[1].(loop.RunAgent)
	if !ok || run.ActionID != "a1" || run.TaskID != "t1" {
		t.Fatalf("second action must run the agent: %#v", acts[1])
	}
	if run.Data["title"] != "x" {
		t.Fatalf("the raising event's data must reach the run: %v", run.Data)
	}
}

// The claim reads the task's LIVE status, so a replayed re-drive for a task
// already claimed is a no-op — this is what stops a double-fire.
func TestDispatchIgnoresTasksNotAtTheReleaseStatus(t *testing.T) {
	p := Policy{Config: cfg()}
	ev := loop.Event{ID: "r1", Type: loop.TaskUpdated, Data: map[string]string{"task_id": "t1"}}
	for _, tc := range []struct {
		name string
		task loop.Task
	}{
		{"still held", loop.Task{ID: "t1", Status: "hold", Action: "a1"}},
		{"already claimed", loop.Task{ID: "t1", Status: "running", Action: "a1"}},
		{"no action reference", loop.Task{ID: "t1", Status: "go"}},
		{"absent from the live slice", loop.Task{ID: "other", Status: "go", Action: "a1"}},
	} {
		if got := decide(p, loop.Context{Event: ev, Tasks: []loop.Task{tc.task}}); len(got) != 0 {
			t.Errorf("%s: want no actions, got %#v", tc.name, got)
		}
	}
	// A re-drive with no task id at all is inert.
	if got := decide(p, loop.Context{Event: loop.Event{Type: loop.TaskUpdated}}); len(got) != 0 {
		t.Errorf("a re-drive with no task id must do nothing, got %#v", got)
	}
}

// A task.updated event must never be treated as an ingestible event, even if
// somebody writes an action listening for that type.
func TestTaskUpdatedNeverParksANewRun(t *testing.T) {
	p := Policy{Config: cfg(config.Action{ID: "a1", On: loop.TaskUpdated})}
	got := decide(p, loop.Context{Event: loop.Event{ID: "r1", Type: loop.TaskUpdated, Data: map[string]string{"task_id": "t1"}}})
	for _, a := range got {
		if _, isAdd := a.(loop.AddTask); isAdd {
			t.Fatalf("a re-drive must not park a run: %#v", got)
		}
	}
}
