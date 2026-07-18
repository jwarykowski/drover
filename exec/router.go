package exec

import (
	"context"

	"github.com/jwarykowski/drover/loop"
)

// RouterExecutor dispatches each action to the executor that can apply it:
// RunAction to the runner (the actuation boundary), board mutations (AddTask,
// SetStatus) to the store. Actions run in order, one at a time, so a policy can
// emit a sequence like [SetStatus running, RunAction, SetStatus done] and rely on
// it executing left to right — if the RunAction fails, Apply returns before the
// closing SetStatus, leaving the task claimed for inspection rather than closed.
type RouterExecutor struct {
	Store  loop.Executor // handles AddTask, SetStatus
	Runner loop.Executor // handles RunAction
}

func (r RouterExecutor) Apply(ctx context.Context, actions []loop.Action) error {
	for _, a := range actions {
		ex := r.Store
		if _, ok := a.(loop.RunAction); ok {
			ex = r.Runner
		}
		if err := ex.Apply(ctx, []loop.Action{a}); err != nil {
			return err
		}
	}
	return nil
}
