package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/jwarykowski/drover/loop"
)

// The money path: state must survive a process restart (a parked hold must still
// be there, and a release must persist), since durability is the whole reason the
// store exists on disk rather than in memory.
func TestFileStorePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	ctx := context.Background()

	s, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.Add(ctx, loop.Spec{Text: "fix ci", Agentic: true, Action: "a1", Status: "hold"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus(ctx, it.ID, "go"); err != nil {
		t.Fatal(err)
	}

	// reopen from disk — a fresh instance, as after a restart
	s2, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.List(ctx, loop.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != it.ID {
		t.Fatalf("task not persisted: %+v", got)
	}
	if got[0].Status != "go" || !got[0].Agentic || got[0].Action != "a1" {
		t.Fatalf("fields not persisted: %+v", got[0])
	}
}

func TestFileStoreDoneAndArchive(t *testing.T) {
	ctx := context.Background()
	s, _ := OpenFileStore("") // in-memory
	it, _ := s.Add(ctx, loop.Spec{Text: "t", Agentic: true})

	if err := s.SetStatus(ctx, it.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.List(ctx, loop.Filter{}); len(got) != 0 {
		t.Fatalf("done task must drop off the default list: %+v", got)
	}
	if got, _ := s.List(ctx, loop.Filter{IncludeDone: true}); len(got) != 1 || !got[0].Done {
		t.Fatalf("done task must appear with IncludeDone: %+v", got)
	}
	if err := s.Archive(ctx, it.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.List(ctx, loop.Filter{IncludeDone: true}); len(got) != 0 {
		t.Fatalf("archived task must leave the live board: %+v", got)
	}
}

func TestFileStoreDelete(t *testing.T) {
	ctx := context.Background()
	s, _ := OpenFileStore("")
	live, _ := s.Add(ctx, loop.Spec{Text: "live"})
	gone, _ := s.Add(ctx, loop.Spec{Text: "gone"})
	_ = s.Archive(ctx, gone.ID) // archived tasks are deletable too

	if err := s.Delete(ctx, live.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.List(ctx, loop.Filter{IncludeDone: true}); len(got) != 0 {
		t.Fatalf("live board not empty: %+v", got)
	}
	if got := s.Archived(); len(got) != 0 {
		t.Fatalf("archive not empty: %+v", got)
	}
	if err := s.Delete(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestFileStoreRestart(t *testing.T) {
	ctx := context.Background()
	s, _ := OpenFileStore("")
	it, _ := s.Add(ctx, loop.Spec{Text: "run", Agentic: true, Action: "a1", Status: "go"})
	_ = s.Note(ctx, it.ID, "done: shipped")
	_ = s.SetStatus(ctx, it.ID, "done")
	_ = s.Archive(ctx, it.ID)

	if err := s.Restart(ctx, it.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.List(ctx, loop.Filter{IncludeDone: true})
	if len(got) != 1 {
		t.Fatalf("restart must pull the task back onto the live board: %+v", got)
	}
	r := got[0]
	if r.Done || r.Status != "hold" || r.Note != "" {
		t.Fatalf("restart must re-park at hold with cleared verdict, got %+v", r)
	}
	if len(s.Archived()) != 0 {
		t.Fatal("restart must remove the task from the archive")
	}
}

func TestFileStoreMissingIDIsNotFound(t *testing.T) {
	s, _ := OpenFileStore("")
	err := s.SetStatus(context.Background(), "ghost", "go")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
