// Package context assembles the bundle a policy reasons over: the triggering
// event plus the slice of tasks worth attending to. Profile, retrieval and
// history tiers slot in behind Assembler later.
package context

import (
	"context"

	"github.com/jwarykowski/drover/loop"
)

// WorkingContext is the Tier 1 assembler: it reads the task slice relevant to
// the event and returns it alongside the event. Attention as a WHERE clause.
type WorkingContext struct {
	Store loop.Store
}

// Assemble derives a filter from the event and reads the matching tasks.
func (w WorkingContext) Assemble(ctx context.Context, e loop.Event) (loop.Context, error) {
	tasks, err := w.Store.List(ctx, filterFor(e))
	if err != nil {
		return loop.Context{}, err
	}
	return loop.Context{Event: e, Tasks: tasks}, nil
}

// filterFor maps an event type to the tasks worth attending to. A dispatch
// needs the live task by id, so it reads everything open; nothing else narrows
// yet.
func filterFor(loop.Event) loop.Filter {
	return loop.Filter{}
}
