package store

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/jwarykowski/drover/loop"
)

// FakeStore is an in-memory loop.Store for tests — no binary, no network. It
// mirrors ShepherdStore's observable behaviour: Add assigns an id and echoes the
// item, List applies the same filter semantics, SetStatus addresses by id.
type FakeStore struct {
	mu       sync.Mutex
	items    []loop.Item
	archived []loop.Item // items moved off the live board by Archive
	seq      int
}

// Seed installs starting items (ids filled in if empty).
func (f *FakeStore) Seed(items ...loop.Item) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, it := range items {
		if it.ID == "" {
			f.seq++
			it.ID = fmt.Sprintf("fake-%d", f.seq)
		}
		it.Index = len(f.items) + 1
		f.items = append(f.items, it)
	}
}

func (f *FakeStore) List(_ context.Context, filter loop.Filter) ([]loop.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []loop.Item
	for _, it := range f.items {
		if it.Done && !filter.IncludeDone {
			continue
		}
		if filter.Category != "" && it.Category != filter.Category {
			continue
		}
		if filter.Text != "" && !strings.Contains(strings.ToLower(it.Text), strings.ToLower(filter.Text)) {
			continue
		}
		out = append(out, it)
	}
	return out, nil
}

func (f *FakeStore) Add(_ context.Context, s loop.Spec) (loop.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	it := loop.Item{
		ID: fmt.Sprintf("fake-%d", f.seq), Index: len(f.items) + 1,
		Text: s.Text, Category: s.Category, Priority: normPrio(s.Priority),
		Status: s.Status, Agentic: s.Agentic, Action: s.Action,
		Due: s.Due, Link: s.Link, Note: s.Note,
	}
	f.items = append(f.items, it)
	return it, nil
}

func (f *FakeStore) SetStatus(_ context.Context, id, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.items {
		if f.items[i].ID == id {
			f.items[i].Done = status == "done"
			if status != "done" && status != "undone" {
				f.items[i].Status = status
			}
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrNotFound, id)
}

func (f *FakeStore) Note(_ context.Context, id, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.items {
		if f.items[i].ID == id {
			f.items[i].Note = text
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrNotFound, id)
}

// Archive moves an item off the live board into the archive set — the live List
// no longer returns it, mirroring shepherd's per-item archive.
func (f *FakeStore) Archive(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.items {
		if f.items[i].ID == id {
			f.archived = append(f.archived, f.items[i])
			f.items = append(f.items[:i], f.items[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrNotFound, id)
}

// Archived returns the items moved off the board by Archive (tests assert on it).
func (f *FakeStore) Archived() []loop.Item {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]loop.Item(nil), f.archived...)
}

func normPrio(p string) string {
	switch strings.ToUpper(p) {
	case "H":
		return "H"
	case "M":
		return "M"
	case "L":
		return "L"
	}
	return ""
}
