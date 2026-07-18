// Package exec applies actions. StoreExecutor handles task mutations; it never
// runs strings sourced from the board.
package exec

import (
	"context"
	"fmt"

	"github.com/jwarykowski/drover/loop"
)

// StoreExecutor applies AddTask actions through a Store, idempotently.
type StoreExecutor struct {
	Store loop.Store
}

// Apply creates a task per AddTask, skipping any whose link already exists on the
// board. Dedup is by link until shepherd offers a real idempotency key.
func (x StoreExecutor) Apply(ctx context.Context, actions []loop.Action) error {
	for _, a := range actions {
		add, ok := a.(loop.AddTask)
		if !ok {
			return fmt.Errorf("exec: unsupported action %T", a)
		}
		exists, err := x.linkExists(ctx, add.Spec.Link)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := x.Store.Add(ctx, add.Spec); err != nil {
			return err
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
