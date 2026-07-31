package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jwarykowski/drover/loop"
)

// FileStore is drover's task datastore: runs live here as JSON, owned by
// drover alone. It is BOTH a loop.Store (the CRUD seam the loop mutates) and a
// loop.Source (a ticker re-drives live tasks as task.updated events, so a task
// a human has released gets dispatched without any source having to notice).
//
// A single instance is shared by the daemon and the dashboard, so its mutex is
// the one serialisation point; two instances over one file would clobber each
// other. Emitting events from the dedicated Events goroutine — never from a
// mutation call — keeps the loop's own writes from recursing into the channel.
type FileStore struct {
	path string
	tick time.Duration

	mu    sync.Mutex
	items []loop.Task // live tasks
	arch  []loop.Task // archived, off the live lanes
	seq   int         // monotonic Index source
}

// fileData is the on-disk shape: the whole store rewritten atomically per
// change (tiny at task scale).
type fileData struct {
	Items    []loop.Task `json:"items"`
	Archived []loop.Task `json:"archived"`
	Seq      int         `json:"seq"`
}

// Now is the clock used to stamp task timestamps, swappable in tests.
var Now = time.Now

// ErrNotFound is returned when a task id addresses nothing.
var ErrNotFound = errors.New("task not found")

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

// Archived returns a copy of the archived (completed) tasks, newest last. The
// dashboard surfaces these in the done lane so a finished run stays
// reviewable (its per-job log is keyed by id and persists regardless).
func (s *FileStore) Archived() []loop.Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]loop.Task, len(s.arch))
	copy(out, s.arch)
	return out
}

// List returns the live tasks, a copy; f only decides whether done ones ride along.
func (s *FileStore) List(_ context.Context, f loop.Filter) ([]loop.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []loop.Task
	for _, it := range s.items {
		if it.Done && !f.IncludeDone {
			continue
		}
		out = append(out, it)
	}
	return out, nil
}

// Add appends a new task from spec and returns it.
func (s *FileStore) Add(_ context.Context, spec loop.Spec) (loop.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	it := loop.Task{
		ID:       newTaskID(),
		Index:    s.seq,
		Text:     spec.Text,
		Priority: strings.ToUpper(spec.Priority),
		Status:   spec.Status,
		Type:     spec.Type,
		Action:   spec.Action,
		Subject:  spec.Subject,
		Due:      spec.Due,
		Link:     spec.Link,
		Note:     spec.Note,
		Source:   spec.Source,
		Data:     spec.Data,
		Created:  Now().UTC().Format(time.RFC3339),
	}
	s.items = append(s.items, it)
	if err := s.flush(); err != nil {
		return loop.Task{}, err
	}
	return it, nil
}

// SetStatus marks a task done/undone or sets a named status, by id. "done"
// closes it; "undone"/"" reopens and clears status; any other value is a named
// status (e.g. hold/go/running) on an open task.
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
		it.Completed = Now().UTC().Format(time.RFC3339)
	case "undone", "":
		it.Done, it.Status, it.Completed = false, "", ""
	default:
		it.Done, it.Status = false, status
	}
	return s.flush()
}

// Note attaches a note to a task by id.
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

// Archive moves a task off the live lanes into the archive.
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

// Delete removes a task entirely, live or archived — used by
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

// Restart re-queues a run: pulled back onto the live lanes if it was archived,
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

// find returns a pointer to the live task with id, or nil. Caller holds s.mu.
func (s *FileStore) find(id string) *loop.Task {
	for i := range s.items {
		if s.items[i].ID == id {
			return &s.items[i]
		}
	}
	return nil
}

// Events makes FileStore a loop.Source: every tick it re-drives each open task
// as a task.updated event. The policy reads the task's LIVE status (not the
// event's), so replaying an already-claimed task is a no-op — only a task at
// `go` fires. Latency to dispatch is at most one tick.
//
// Fixed-interval re-drive of every open task; add a mutation notify channel
// only if that latency ever bites.
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
func (s *FileStore) liveSnapshot() []loop.Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]loop.Task, 0, len(s.items))
	for _, it := range s.items {
		if !it.Done {
			out = append(out, it)
		}
	}
	return out
}

// taskEvent is the one event drover raises rather than ingests. It carries only
// the task id: the policy resolves the task's live state from the store, so a
// stale replay can never fire on a status the task has already left.
func taskEvent(t loop.Task) loop.Event {
	return loop.Event{
		ID:     "task:updated:" + t.ID,
		Type:   loop.TaskUpdated,
		Source: "drover.tasks",
		Data:   map[string]string{"task_id": t.ID},
		At:     time.Now(),
	}
}

func newTaskID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// DefaultTasksPath is where the task store lives, beside drover.toml.
func DefaultTasksPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "drover", "tasks.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "drover", "tasks.json")
}

// DefaultLogDir is where per-job agent stream logs live, beside tasks.json.
func DefaultLogDir() string {
	return filepath.Join(filepath.Dir(DefaultTasksPath()), "logs")
}
