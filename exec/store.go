// Package exec applies actions. StoreExecutor handles task mutations; it never
// runs a string sourced from an event.
package exec

import (
	"context"
	"fmt"

	"github.com/jwarykowski/drover/loop"
)

// StoreExecutor applies task mutations (AddTask, SetStatus) through a Store. It
// never runs a string sourced from an event.
type StoreExecutor struct {
	Store loop.Store
}

// Apply handles each action: AddTask creates a task (skipping any whose subject
// AND action already have a live run — so one event can still raise several
// distinct actions, while a repeat event about the same subject doesn't
// double-park); SetStatus transitions an existing task by id.
func (x StoreExecutor) Apply(ctx context.Context, actions []loop.Action) error {
	for _, a := range actions {
		switch v := a.(type) {
		case loop.AddTask:
			exists, err := x.taskExists(ctx, v.Spec.Subject, v.Spec.Action)
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

// taskExists reports whether an ACTIVE (not-done) task already carries this
// subject and action — so a repeat event doesn't double-park while a run is in
// flight, a different action on the same subject still gets its own task, and a
// completed run no longer blocks a fresh one (re-fire after done). An empty
// subject never dedups (nothing to key on).
func (x StoreExecutor) taskExists(ctx context.Context, subject, action string) (bool, error) {
	if subject == "" {
		return false, nil
	}
	tasks, err := x.Store.List(ctx, loop.Filter{IncludeDone: true})
	if err != nil {
		return false, err
	}
	for _, t := range tasks {
		if t.Done {
			continue // a finished run no longer blocks a fresh trigger
		}
		if t.Subject == subject && t.Action == action {
			return true, nil
		}
	}
	return false, nil
}
