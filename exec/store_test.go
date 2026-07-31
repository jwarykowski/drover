package exec

import (
	"context"
	"testing"

	"github.com/jwarykowski/drover/loop"
)

// stubStore is a minimal in-memory Store — enough to exercise dedup while
// counting adds, which store/fake.go does not expose.
type stubStore struct {
	items []loop.Task
	adds  int
}

func (s *stubStore) List(_ context.Context, _ loop.Filter) ([]loop.Task, error) {
	return s.items, nil
}
func (s *stubStore) Add(_ context.Context, spec loop.Spec) (loop.Task, error) {
	s.adds++
	it := loop.Task{ID: "new", Text: spec.Text, Subject: spec.Subject, Action: spec.Action}
	s.items = append(s.items, it)
	return it, nil
}
func (s *stubStore) SetStatus(_ context.Context, _, _ string) error { return nil }
func (s *stubStore) Note(_ context.Context, _, _ string) error      { return nil }
func (s *stubStore) Archive(_ context.Context, _ string) error      { return nil }

func TestApplyDedupBySubject(t *testing.T) {
	st := &stubStore{items: []loop.Task{{ID: "1", Subject: "https://ci/42"}}}
	x := StoreExecutor{Store: st}
	act := []loop.Action{loop.AddTask{Spec: loop.Spec{Text: "dup", Subject: "https://ci/42"}}}

	// Existing subject: no add.
	if err := x.Apply(context.Background(), act); err != nil {
		t.Fatal(err)
	}
	if st.adds != 0 {
		t.Errorf("existing subject: want 0 adds, got %d", st.adds)
	}

	// New subject: one add, and a repeat is idempotent.
	fresh := []loop.Action{loop.AddTask{Spec: loop.Spec{Text: "new", Subject: "https://ci/99"}}}
	if err := x.Apply(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	if err := x.Apply(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	if st.adds != 1 {
		t.Errorf("new subject twice: want 1 add, got %d", st.adds)
	}
}

// Two different actions on ONE subject are two runs — dedup is per
// (subject, action), so a single event can still drive several actions.
func TestApplyAllowsDistinctActionsOnOneSubject(t *testing.T) {
	st := &stubStore{}
	x := StoreExecutor{Store: st}
	if err := x.Apply(context.Background(), []loop.Action{
		loop.AddTask{Spec: loop.Spec{Text: "review", Subject: "pr/1", Action: "a1"}},
		loop.AddTask{Spec: loop.Spec{Text: "changelog", Subject: "pr/1", Action: "a2"}},
	}); err != nil {
		t.Fatal(err)
	}
	if st.adds != 2 {
		t.Errorf("distinct actions on one subject: want 2 adds, got %d", st.adds)
	}
}

// A source that names no subject cannot be deduped, so every event parks — the
// alternative (collapsing them all into one run) would silently drop work.
func TestApplyNeverDedupsWithoutASubject(t *testing.T) {
	st := &stubStore{}
	x := StoreExecutor{Store: st}
	act := []loop.Action{loop.AddTask{Spec: loop.Spec{Text: "anon", Action: "a1"}}}
	for i := 0; i < 2; i++ {
		if err := x.Apply(context.Background(), act); err != nil {
			t.Fatal(err)
		}
	}
	if st.adds != 2 {
		t.Errorf("subjectless events must all park: want 2 adds, got %d", st.adds)
	}
}

// A completed run no longer blocks a fresh trigger for the same subject+action —
// this is what lets a recurring subject re-fire once its previous run is done.
func TestApplyRefiresAfterDone(t *testing.T) {
	st := &stubStore{items: []loop.Task{{ID: "1", Subject: "h1", Action: "b1", Done: true}}}
	x := StoreExecutor{Store: st}
	act := []loop.Action{loop.AddTask{Spec: loop.Spec{Text: "again", Subject: "h1", Action: "b1"}}}
	if err := x.Apply(context.Background(), act); err != nil {
		t.Fatal(err)
	}
	if st.adds != 1 {
		t.Errorf("done run must not block a re-fire: want 1 add, got %d", st.adds)
	}

	// An ACTIVE run with the same subject+action still blocks.
	st2 := &stubStore{items: []loop.Task{{ID: "2", Subject: "h2", Action: "b1"}}}
	x2 := StoreExecutor{Store: st2}
	act2 := []loop.Action{loop.AddTask{Spec: loop.Spec{Text: "dup", Subject: "h2", Action: "b1"}}}
	if err := x2.Apply(context.Background(), act2); err != nil {
		t.Fatal(err)
	}
	if st2.adds != 0 {
		t.Errorf("active run must block a duplicate: want 0 adds, got %d", st2.adds)
	}
}
