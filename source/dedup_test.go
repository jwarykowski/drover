package source

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jwarykowski/drover/loop"
)

// sliceSource replays a fixed set of events then closes.
type sliceSource []loop.Event

func (s sliceSource) Events(context.Context) <-chan loop.Event {
	ch := make(chan loop.Event, len(s))
	for _, e := range s {
		ch <- e
	}
	close(ch)
	return ch
}

func TestDedupDropsSeenAndEmpty(t *testing.T) {
	src := sliceSource{{ID: "a"}, {ID: "a"}, {ID: ""}, {ID: "b"}}
	d := Dedup{Src: src, Seen: NewMemSeen()}
	var got []string
	for e := range d.Events(context.Background()) {
		got = append(got, e.ID)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("want [a b] (dup + empty dropped), got %v", got)
	}
}

func TestFileSeenRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seen")
	s, err := OpenFileSeen(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Empty() {
		t.Fatal("fresh FileSeen should be empty")
	}
	if err := s.Add("x"); err != nil {
		t.Fatal(err)
	}
	_ = s.Add("y")
	if !s.Has("x") || s.Empty() {
		t.Fatal("Add not reflected")
	}

	// Reopen: ids persisted across restarts.
	s2, err := OpenFileSeen(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Has("x") || !s2.Has("y") {
		t.Fatal("ids not persisted")
	}
}
