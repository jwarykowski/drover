package source

import (
	"context"
	"sync"

	"github.com/jwarykowski/drover/loop"
)

// Merge fans several sources into one event stream so a single loop can react to
// events from all of them — e.g. GitHubSource (merges) and WatchSource (board
// releases) driving the same policy. The merged channel closes once every input
// has closed.
func Merge(srcs ...loop.Source) loop.Source {
	return mergedSource(srcs)
}

type mergedSource []loop.Source

func (m mergedSource) Events(ctx context.Context) <-chan loop.Event {
	out := make(chan loop.Event)
	var wg sync.WaitGroup
	for _, s := range m {
		wg.Add(1)
		go func(src loop.Source) {
			defer wg.Done()
			for e := range src.Events(ctx) {
				select {
				case <-ctx.Done():
					return
				case out <- e:
				}
			}
		}(s)
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}
