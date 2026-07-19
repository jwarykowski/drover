// Package context assembles the bundle a policy reasons over. Phase 0+1 ships
// only the working-context tier: the triggering event plus the relevant board
// slice. Profile, retrieval and history tiers slot in behind Assembler later.
package context

import (
	"context"

	"github.com/jwarykowski/drover/loop"
)

// WorkingContext is the Tier 1 assembler: it reads the board slice relevant to
// the event and returns it alongside the event. Attention as a WHERE clause.
type WorkingContext struct {
	Store loop.Store
}

// Assemble derives a filter from the event and reads the matching board slice.
func (w WorkingContext) Assemble(ctx context.Context, e loop.Event) (loop.Context, error) {
	board, err := w.Store.List(ctx, filterFor(e))
	if err != nil {
		return loop.Context{}, err
	}
	return loop.Context{Event: e, Board: board}, nil
}

// filterFor maps an event type to the slice of the board worth attending to.
func filterFor(e loop.Event) loop.Filter {
	switch e.Type {
	case "ci.failed":
		return loop.Filter{Category: "ci"}
	default:
		return loop.Filter{}
	}
}
