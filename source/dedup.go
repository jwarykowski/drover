package source

import (
	"bufio"
	"context"
	"os"
	"strings"
	"sync"

	"github.com/jwarykowski/drover/loop"
)

// Seen records which event ids have already been handled, so a source can be
// made at-most-once regardless of how its transport dedups (or doesn't).
type Seen interface {
	Has(id string) bool
	Add(id string) error
}

// Dedup wraps a source and drops events whose id has been seen. It records
// BEFORE emitting: if the process dies mid-handoff the event is dropped rather
// than re-fired — the same bias as claiming a task `running` before acting. An
// empty id is always dropped (an undeduplicatable event must not fan out).
type Dedup struct {
	Src  loop.Source
	Seen Seen
	Logf func(string, ...any)
}

func (d Dedup) Events(ctx context.Context) <-chan loop.Event {
	out := make(chan loop.Event)
	go func() {
		defer close(out)
		for e := range d.Src.Events(ctx) {
			if e.ID == "" || d.Seen.Has(e.ID) {
				continue
			}
			if err := d.Seen.Add(e.ID); err != nil {
				if d.Logf != nil {
					d.Logf("dedup: record %s: %v", e.ID, err)
				}
				continue // prefer dropping over double-firing
			}
			select {
			case <-ctx.Done():
				return
			case out <- e:
			}
		}
	}()
	return out
}

// FileSeen is a Seen backed by a newline-delimited file: ids are loaded into a
// set on open and appended on Add.
//
// append-only flat file, compact if it ever grows large.
type FileSeen struct {
	path string
	mu   sync.Mutex
	set  map[string]bool
}

// OpenFileSeen loads the id set from path (a missing file starts empty).
func OpenFileSeen(path string) (*FileSeen, error) {
	f := &FileSeen{path: path, set: map[string]bool{}}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		if id := strings.TrimSpace(sc.Text()); id != "" {
			f.set[id] = true
		}
	}
	return f, sc.Err()
}

// Empty reports whether nothing has been recorded yet — a cold start, used to
// decide whether to seed the current head without firing history.
func (f *FileSeen) Empty() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.set) == 0
}

func (f *FileSeen) Has(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.set[id]
}

func (f *FileSeen) Add(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.set[id] {
		return nil
	}
	if f.path != "" {
		file, err := os.OpenFile(f.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if _, err := file.WriteString(id + "\n"); err != nil {
			file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	f.set[id] = true
	return nil
}

// MemSeen is an in-memory Seen for tests and webhook runs that don't persist.
type MemSeen struct {
	mu  sync.Mutex
	set map[string]bool
}

func NewMemSeen() *MemSeen { return &MemSeen{set: map[string]bool{}} }

func (m *MemSeen) Has(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.set[id]
}

func (m *MemSeen) Add(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.set[id] = true
	return nil
}
