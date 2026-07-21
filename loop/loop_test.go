package loop

import (
	"context"
	"testing"
)

// stubPolicy returns whatever it's told, so the loop test controls the actions
// without depending on RulesPolicy.
type stubPolicy struct{ actions []Action }

func (s stubPolicy) Decide(context.Context, Context) []Action { return s.actions }

// recorder captures what the executor was handed.
type recorder struct{ got []Action }

func (r *recorder) Apply(_ context.Context, a []Action) error {
	r.got = append(r.got, a...)
	return nil
}

// passAssembler wraps an event into a bare Context.
type passAssembler struct{ board []Item }

func (p passAssembler) Assemble(_ context.Context, e Event) (Context, error) {
	return Context{Event: e, Board: p.board}, nil
}

func TestLoopRun(t *testing.T) {
	rec := &recorder{}
	l := Loop{
		Assembler: passAssembler{},
		Policy:    stubPolicy{actions: []Action{AddTask{Spec{Text: "x"}}}},
		Executor:  rec,
	}
	if err := l.Run(context.Background(), Event{Type: "ci.failed"}); err != nil {
		t.Fatal(err)
	}
	if len(rec.got) != 1 {
		t.Fatalf("want 1 applied action, got %d", len(rec.got))
	}
}

func TestLoopRunNoActionsSkipsExecutor(t *testing.T) {
	rec := &recorder{}
	l := Loop{Assembler: passAssembler{}, Policy: stubPolicy{}, Executor: rec}
	if err := l.Run(context.Background(), Event{Type: "unknown"}); err != nil {
		t.Fatal(err)
	}
	if len(rec.got) != 0 {
		t.Errorf("no actions: executor should not run, got %d", len(rec.got))
	}
}
