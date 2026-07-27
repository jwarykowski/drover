package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jwarykowski/drover/loop"
)

// FileStore is drover's own task datastore: agentic tasks live here as JSON, not
// on a shepherd board. It is BOTH a loop.Store (the task CRUD seam the loop
// mutates) and a loop.Source (a ticker re-drives live tasks as board.updated
// events so a released hold→go task gets dispatched — the same nudge shepherd's
// poll-mode watch gives, but in-process).
//
// A single instance is shared by the daemon and the dashboard, so its mutex is
// the one serialisation point; two instances over one file would clobber each
// other. Emitting events from the dedicated Events goroutine — never from a
// mutation call — keeps the loop's own writes from recursing into the channel.
type FileStore struct {
	path string
	tick time.Duration

	mu    sync.Mutex
	items []loop.Item // live board
	arch  []loop.Item // archived, off the live board
	seq   int         // monotonic Index source
}

// fileData is the on-disk shape: the whole store rewritten atomically per change
// (tiny at task scale — mirrors registry.go).
type fileData struct {
	Items    []loop.Item `json:"items"`
	Archived []loop.Item `json:"archived"`
	Seq      int         `json:"seq"`
}

// OpenFileStore loads the task store from path. A missing file is an empty store
// (first run), not an error. An empty path is an in-memory store (tests).
func OpenFileStore(path string) (*FileStore, error) {
	s := &FileStore{path: path, tick: 750 * time.Millisecond}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("tasks: read %s: %w", path, err)
	}
	var d fileData
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("tasks: parse %s: %w", path, err)
	}
	s.items, s.arch, s.seq = d.Items, d.Archived, d.Seq
	return s, nil
}

// flush rewrites the whole file atomically. Caller holds s.mu.
func (s *FileStore) flush() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(fileData{Items: s.items, Archived: s.arch, Seq: s.seq}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path) // atomic: a crash never leaves a half-written file
}

// Archived returns a copy of the archived (completed, off-board) tasks, newest
// last. The dashboard surfaces these in the done lane so a finished run stays
// reviewable (its per-job log is keyed by id and persists regardless).
func (s *FileStore) Archived() []loop.Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]loop.Item, len(s.arch))
	copy(out, s.arch)
	return out
}

// List returns the live board narrowed by f (Done/Category/Text), a copy.
func (s *FileStore) List(_ context.Context, f loop.Filter) ([]loop.Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []loop.Item
	for _, it := range s.items {
		if it.Done && !f.IncludeDone {
			continue
		}
		if f.Category != "" && it.Category != f.Category {
			continue
		}
		if f.Text != "" && !strings.Contains(it.Text, f.Text) {
			continue
		}
		out = append(out, it)
	}
	return out, nil
}

// Add appends a new task from spec and returns it.
func (s *FileStore) Add(_ context.Context, spec loop.Spec) (loop.Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	it := loop.Item{
		ID:       newTaskID(),
		Index:    s.seq,
		Text:     spec.Text,
		Category: spec.Category,
		Priority: strings.ToUpper(spec.Priority),
		Status:   spec.Status,
		Agentic:  spec.Agentic,
		Action:   spec.Action,
		Due:      spec.Due,
		Link:     spec.Link,
		Note:     spec.Note,
		Board:    spec.Board,
	}
	s.items = append(s.items, it)
	if err := s.flush(); err != nil {
		return loop.Item{}, err
	}
	return it, nil
}

// SetStatus marks an item done/undone or sets a named status, by id. Mirrors
// ShepherdStore: "done" closes it; "undone"/"" reopens and clears status; any
// other value is a named status (e.g. hold/go/running) on an open item.
func (s *FileStore) SetStatus(_ context.Context, id, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := s.find(id)
	if it == nil {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	switch status {
	case "done":
		it.Done = true
	case "undone", "":
		it.Done, it.Status = false, ""
	default:
		it.Done, it.Status = false, status
	}
	return s.flush()
}

// Note attaches a note to an item by id.
func (s *FileStore) Note(_ context.Context, id, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := s.find(id)
	if it == nil {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	it.Note = text
	return s.flush()
}

// Archive moves an item off the live board into the archive.
func (s *FileStore) Archive(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, it := range s.items {
		if it.ID == id {
			it.Done = true
			s.items = append(s.items[:i], s.items[i+1:]...)
			s.arch = append(s.arch, it)
			return s.flush()
		}
	}
	return fmt.Errorf("%w: %s", ErrNotFound, id)
}

// Delete removes a task entirely, from the live board or the archive — used by
// the dashboard to drop a run the operator no longer wants. The per-job log is
// the caller's to remove (the store doesn't know the log dir).
func (s *FileStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, it := range s.items {
		if it.ID == id {
			s.items = append(s.items[:i], s.items[i+1:]...)
			return s.flush()
		}
	}
	for i, it := range s.arch {
		if it.ID == id {
			s.arch = append(s.arch[:i], s.arch[i+1:]...)
			return s.flush()
		}
	}
	return fmt.Errorf("%w: %s", ErrNotFound, id)
}

// Restart re-queues a run: pulled back onto the live board if it was archived,
// then reset to a fresh parked task (hold, not done, verdict cleared) so it
// waits in the held lane for the operator to release — same gate as any run.
// The stale per-job log is the caller's to clear.
func (s *FileStore) Restart(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, it := range s.arch {
		if it.ID == id {
			s.arch = append(s.arch[:i], s.arch[i+1:]...)
			it.Done, it.Status, it.Note = false, "hold", ""
			s.items = append(s.items, it)
			return s.flush()
		}
	}
	if it := s.find(id); it != nil {
		it.Done, it.Status, it.Note = false, "hold", ""
		return s.flush()
	}
	return fmt.Errorf("%w: %s", ErrNotFound, id)
}

// find returns a pointer to the live item with id, or nil. Caller holds s.mu.
func (s *FileStore) find(id string) *loop.Item {
	for i := range s.items {
		if s.items[i].ID == id {
			return &s.items[i]
		}
	}
	return nil
}

// Events makes FileStore a loop.Source: every tick it re-drives each open task
// as a board.updated event. The Dispatcher reads the task's LIVE status (not the
// event's), so replaying an already-claimed task is a no-op — only a task at `go`
// fires. Latency to dispatch is at most one tick.
//
// ponytail: fixed-interval re-drive of the whole open board; add a mutation
// notify channel only if that latency ever bites.
func (s *FileStore) Events(ctx context.Context) <-chan loop.Event {
	out := make(chan loop.Event) // unbuffered: the loop paces the re-drive
	go func() {
		defer close(out)
		t := time.NewTicker(s.tick)
		defer t.Stop()
		for {
			for _, it := range s.liveSnapshot() {
				select {
				case <-ctx.Done():
					return
				case out <- taskEvent(it):
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return out
}

// liveSnapshot copies the open (not-done) tasks under the lock.
func (s *FileStore) liveSnapshot() []loop.Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]loop.Item, 0, len(s.items))
	for _, it := range s.items {
		if !it.Done {
			out = append(out, it)
		}
	}
	return out
}

func taskEvent(it loop.Item) loop.Event {
	return loop.Event{
		ID:     "task:updated:" + it.ID,
		Type:   "board.updated",
		Source: "drover.tasks",
		Data:   loop.BoardChange{Item: it},
		At:     time.Now(),
	}
}

func newTaskID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// DefaultTasksPath is where the agentic task store lives, beside actions.toml.
func DefaultTasksPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "drover", "tasks.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "drover", "tasks.json")
}

// DefaultLogDir is where per-job claude stream logs live, beside tasks.json.
func DefaultLogDir() string {
	return filepath.Join(filepath.Dir(DefaultTasksPath()), "logs")
}
