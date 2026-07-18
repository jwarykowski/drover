// Package source senses events. GitSource is the Phase 1 batch implementation:
// one event, emitted then done — driven by `drover ingest` from a git hook or a
// CI step. The streaming WatchSource arrives in Phase 3.
package source

import (
	"context"

	"github.com/jwarykowski/drover/loop"
)

// GitSource emits a single pre-built event and closes.
type GitSource struct {
	Event loop.Event
}

// Events returns a channel carrying the one event, then closed.
func (g GitSource) Events(ctx context.Context) <-chan loop.Event {
	ch := make(chan loop.Event, 1)
	ch <- g.Event
	close(ch)
	return ch
}
