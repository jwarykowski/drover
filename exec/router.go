package exec

import (
	"context"

	"github.com/jwarykowski/drover/loop"
)

// RouterExecutor dispatches each action to the executor that can apply it:
// RunAgent to the agent (the actuation boundary), board mutations (AddTask,
// SetStatus) to the store. Actions run in order, one at a time, so a policy can
// emit a sequence like [SetStatus running, RunAgent] and rely on it executing
// left to right — if the RunAgent fails, Apply returns before any later action,
// leaving the task claimed for inspection.
type RouterExecutor struct {
	Store loop.Executor // handles AddTask, SetStatus
	Agent loop.Executor // handles RunAgent
}

func (r RouterExecutor) Apply(ctx context.Context, actions []loop.Action) error {
	for _, a := range actions {
		ex := r.Store
		if _, ok := a.(loop.RunAgent); ok {
			ex = r.Agent
		}
		if err := ex.Apply(ctx, []loop.Action{a}); err != nil {
			return err
		}
	}
	return nil
}
