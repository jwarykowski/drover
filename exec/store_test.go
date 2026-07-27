package exec

import (
	"context"
	"testing"

	"github.com/jwarykowski/drover/loop"
)

// stubStore is a minimal in-memory Store — enough to exercise dedup without the
// full store/fake.go, which lands with the rest of the Phase 2 test infra.
type stubStore struct {
	items []loop.Item
	adds  int
}

func (s *stubStore) List(_ context.Context, _ loop.Filter) ([]loop.Item, error) {
	return s.items, nil
}
func (s *stubStore) Add(_ context.Context, spec loop.Spec) (loop.Item, error) {
	s.adds++
	it := loop.Item{ID: "new", Text: spec.Text, Link: spec.Link}
	s.items = append(s.items, it)
	return it, nil
}
func (s *stubStore) SetStatus(_ context.Context, _, _ string) error { return nil }
func (s *stubStore) Note(_ context.Context, _, _ string) error      { return nil }
func (s *stubStore) Archive(_ context.Context, _ string) error      { return nil }

func TestApplyDedupByLink(t *testing.T) {
	st := &stubStore{items: []loop.Item{{ID: "1", Link: "https://ci/42"}}}
	x := StoreExecutor{Store: st}
	act := []loop.Action{loop.AddTask{Spec: loop.Spec{Text: "dup", Link: "https://ci/42"}}}

	// Existing link: no add.
	if err := x.Apply(context.Background(), act); err != nil {
		t.Fatal(err)
	}
	if st.adds != 0 {
		t.Errorf("existing link: want 0 adds, got %d", st.adds)
	}

	// New link: one add, and a repeat is idempotent.
	fresh := []loop.Action{loop.AddTask{Spec: loop.Spec{Text: "new", Link: "https://ci/99"}}}
	if err := x.Apply(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	if err := x.Apply(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	if st.adds != 1 {
		t.Errorf("new link twice: want 1 add, got %d", st.adds)
	}
}

// A completed run no longer blocks a fresh trigger for the same link+action —
// this is what lets a board item re-fire once its previous run is done.
func TestApplyRefiresAfterDone(t *testing.T) {
	st := &stubStore{items: []loop.Item{{ID: "1", Link: "board:h1", Action: "b1", Done: true}}}
	x := StoreExecutor{Store: st}
	act := []loop.Action{loop.AddTask{Spec: loop.Spec{Text: "again", Link: "board:h1", Action: "b1"}}}
	if err := x.Apply(context.Background(), act); err != nil {
		t.Fatal(err)
	}
	if st.adds != 1 {
		t.Errorf("done item must not block re-fire: want 1 add, got %d", st.adds)
	}

	// An ACTIVE run with the same link+action still blocks.
	st2 := &stubStore{items: []loop.Item{{ID: "2", Link: "board:h2", Action: "b1"}}}
	x2 := StoreExecutor{Store: st2}
	act2 := []loop.Action{loop.AddTask{Spec: loop.Spec{Text: "dup", Link: "board:h2", Action: "b1"}}}
	if err := x2.Apply(context.Background(), act2); err != nil {
		t.Fatal(err)
	}
	if st2.adds != 0 {
		t.Errorf("active run must block a duplicate: want 0 adds, got %d", st2.adds)
	}
}
