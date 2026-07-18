// Package exec applies actions. StoreExecutor handles task mutations; it never
// runs strings sourced from the board.
package exec

import (
	"context"
	"fmt"

	"github.com/jwarykowski/drover/loop"
)

// StoreExecutor applies board mutations (AddTask, SetStatus) through a Store. It
// never runs strings sourced from the board.
type StoreExecutor struct {
	Store loop.Store
}

// Apply handles each board action: AddTask creates a task (skipping any whose
// link already exists — dedup by link until shepherd offers an idempotency key);
// SetStatus transitions an existing item by id.
func (x StoreExecutor) Apply(ctx context.Context, actions []loop.Action) error {
	for _, a := range actions {
		switch v := a.(type) {
		case loop.AddTask:
			exists, err := x.linkExists(ctx, v.Spec.Link)
			if err != nil {
				return err
			}
			if exists {
				continue
			}
			if _, err := x.Store.Add(ctx, v.Spec); err != nil {
				return err
			}
		case loop.SetStatus:
			if err := x.Store.SetStatus(ctx, v.ID, v.Status); err != nil {
				return err
			}
		default:
			return fmt.Errorf("exec: unsupported action %T", a)
		}
	}
	return nil
}

func (x StoreExecutor) linkExists(ctx context.Context, link string) (bool, error) {
	if link == "" {
		return false, nil
	}
	board, err := x.Store.List(ctx, loop.Filter{IncludeDone: true})
	if err != nil {
		return false, err
	}
	for _, it := range board {
		if it.Link == link {
			return true, nil
		}
	}
	return false, nil
}
