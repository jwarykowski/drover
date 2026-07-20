package store

import (
	"context"
	"sync"

	"github.com/jwarykowski/drover/loop"
)

// Locking serialises access to an inner Store with a mutex, so concurrent agent
// workers and the sensing goroutine never invoke a file-locked shepherd at the
// same time. Share one instance (a pointer) across every consumer for the mutex
// to actually serialise.
type Locking struct {
	Store loop.Store
	mu    sync.Mutex
}

func (l *Locking) List(ctx context.Context, f loop.Filter) ([]loop.Item, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Store.List(ctx, f)
}

func (l *Locking) Add(ctx context.Context, s loop.Spec) (loop.Item, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Store.Add(ctx, s)
}

func (l *Locking) SetStatus(ctx context.Context, id, status string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Store.SetStatus(ctx, id, status)
}

func (l *Locking) Note(ctx context.Context, id, text string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Store.Note(ctx, id, text)
}
