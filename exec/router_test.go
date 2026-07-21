package exec

import (
	"context"
	"testing"

	"github.com/jwarykowski/drover/loop"
	"github.com/jwarykowski/drover/store"
)

// recordExec captures which actions reached it, to prove routing and order.
type recordExec struct{ got *[]loop.Action }

func (r recordExec) Apply(_ context.Context, a []loop.Action) error {
	*r.got = append(*r.got, a...)
	return nil
}

func TestRouterDispatchesByTypeInOrder(t *testing.T) {
	var storeGot, runGot []loop.Action
	r := RouterExecutor{
		Store:  recordExec{got: &storeGot},
		Runner: recordExec{got: &runGot},
	}
	seq := []loop.Action{
		loop.SetStatus{ID: "x", Status: "running"},
		loop.RunAction{Name: "run-skill"},
		loop.SetStatus{ID: "x", Status: "done"},
	}
	if err := r.Apply(context.Background(), seq); err != nil {
		t.Fatal(err)
	}
	if len(runGot) != 1 {
		t.Fatalf("runner should get 1 RunAction, got %d", len(runGot))
	}
	if len(storeGot) != 2 {
		t.Fatalf("store should get 2 SetStatus, got %d", len(storeGot))
	}
	if s := storeGot[0].(loop.SetStatus); s.Status != "running" {
		t.Fatalf("store order wrong: first was %q", s.Status)
	}
}

func TestStoreExecutorAppliesSetStatus(t *testing.T) {
	st := &store.FakeStore{}
	st.Seed(loop.Item{ID: "t1", Status: "go"})
	x := StoreExecutor{Store: st}
	if err := x.Apply(context.Background(), []loop.Action{loop.SetStatus{ID: "t1", Status: "done"}}); err != nil {
		t.Fatal(err)
	}
	items, _ := st.List(context.Background(), loop.Filter{IncludeDone: true})
	if !items[0].Done {
		t.Fatalf("SetStatus done did not mark item done: %#v", items[0])
	}
}
