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
