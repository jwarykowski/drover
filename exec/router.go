package exec

import (
	"context"

	"github.com/jwarykowski/drover/loop"
)

// RouterExecutor dispatches each action to the executor that can apply it:
// RunAction to the runner and RunAgent to the agent (both actuation
// boundaries), board mutations (AddTask, SetStatus) to the store. Actions run in
// order, one at a time, so a policy can emit a sequence like [SetStatus running,
// RunAgent] and rely on it executing left to right — if the RunAgent fails, Apply
// returns before any later action, leaving the task claimed for inspection.
type RouterExecutor struct {
	Store  loop.Executor // handles AddTask, SetStatus
	Runner loop.Executor // handles RunAction
	Agent  loop.Executor // handles RunAgent
}

func (r RouterExecutor) Apply(ctx context.Context, actions []loop.Action) error {
	for _, a := range actions {
		ex := r.Store
		switch a.(type) {
		case loop.RunAction:
			ex = r.Runner
		case loop.RunAgent:
			ex = r.Agent
		}
		if err := ex.Apply(ctx, []loop.Action{a}); err != nil {
			return err
		}
	}
	return nil
}
