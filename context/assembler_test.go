package context

import (
	"context"
	"testing"

	"github.com/jwarykowski/drover/loop"
	"github.com/jwarykowski/drover/store"
)

func TestWorkingContextCarriesEventAndTasks(t *testing.T) {
	fs := &store.FakeStore{}
	fs.Seed(
		loop.Task{ID: "t1", Text: "old ci break"},
		loop.Task{ID: "t2", Text: "buy milk"},
	)
	w := WorkingContext{Store: fs}

	c, err := w.Assemble(context.Background(), loop.Event{Type: "github.pull_request.merged"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Event.Type != "github.pull_request.merged" {
		t.Errorf("event not carried through: %+v", c.Event)
	}
	// Nothing narrows yet: dispatch resolves a task by id, so it needs every open
	// task in the slice. A filter that hid the dispatched task would stop it firing.
	if len(c.Tasks) != 2 {
		t.Errorf("want every open task, got %d: %+v", len(c.Tasks), c.Tasks)
	}
}

// A done task is not in the assembled slice, so a re-drive for one cannot
// dispatch — the store's own filter is what makes replay safe.
func TestWorkingContextExcludesDoneTasks(t *testing.T) {
	fs := &store.FakeStore{}
	fs.Seed(loop.Task{ID: "t1", Text: "a"}, loop.Task{ID: "t2", Text: "b", Done: true})
	w := WorkingContext{Store: fs}

	c, err := w.Assemble(context.Background(), loop.Event{Type: loop.TaskUpdated})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Tasks) != 1 || c.Tasks[0].ID != "t1" {
		t.Errorf("done task should not be attended to: %+v", c.Tasks)
	}
}
