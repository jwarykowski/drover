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
// link AND action already exist — dedup by link+action so one event can still
// raise several distinct actions, while a re-delivery of the same event doesn't
// double-park); SetStatus transitions an existing item by id.
func (x StoreExecutor) Apply(ctx context.Context, actions []loop.Action) error {
	for _, a := range actions {
		switch v := a.(type) {
		case loop.AddTask:
			exists, err := x.taskExists(ctx, v.Spec.Link, v.Spec.Action)
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

// taskExists reports whether an item already carries this link and action —
// so a re-delivered event doesn't double-park, while a different action on the
// same link still gets its own task. An empty link never dedups (nothing to key
// on).
func (x StoreExecutor) taskExists(ctx context.Context, link, action string) (bool, error) {
	if link == "" {
		return false, nil
	}
	board, err := x.Store.List(ctx, loop.Filter{IncludeDone: true})
	if err != nil {
		return false, err
	}
	for _, it := range board {
		if it.Link == link && it.Action == action {
			return true, nil
		}
	}
	return false, nil
}
