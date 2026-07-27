package source

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"time"

	"github.com/jwarykowski/drover/loop"
)

// WatchSource streams board changes as events by running `shepherd watch` and
// parsing its NDJSON. It reconnects when the stream drops (e.g. shepherd
// restarts) and applies backpressure through an unbuffered channel — a slow
// consumer pauses the reader, which pauses shepherd's writes.
type WatchSource struct {
	Bin      string        // shepherd binary; defaults to "shepherd"
	Board    string        // board to watch; empty means the default
	Interval time.Duration // poll interval passed to shepherd watch; 0 uses its default
	Backoff  time.Duration // wait before reconnecting; 0 defaults to 1s
	Logf     func(string, ...any)
}

// watchLine is one NDJSON record from `shepherd watch`: a change (added/updated/
// removed/archived) carries item; the initial snapshot carries items.
type watchLine struct {
	Type  string      `json:"type"`
	Item  loop.Item   `json:"item"`
	Items []loop.Item `json:"items"`
}

func (s WatchSource) bin() string {
	if s.Bin == "" {
		return "shepherd"
	}
	return s.Bin
}

func (s WatchSource) backoff() time.Duration {
	if s.Backoff <= 0 {
		return time.Second
	}
	return s.Backoff
}

func (s WatchSource) logf(format string, a ...any) {
	if s.Logf != nil {
		s.Logf(format, a...)
	}
}

// boardID is the concrete name of the watched board, resolving the empty
// default to "default" — so a triggered run on the default board is tagged with
// a real id, distinct from a board-less (github/sentry) run.
func (s WatchSource) boardID() string {
	if s.Board == "" {
		return "default"
	}
	return s.Board
}

// Events runs the watch loop until ctx is cancelled, reconnecting on drop.
func (s WatchSource) Events(ctx context.Context) <-chan loop.Event {
	out := make(chan loop.Event) // unbuffered: the consumer paces the producer
	go func() {
		defer close(out)
		for ctx.Err() == nil {
			err := s.stream(ctx, out)
			if ctx.Err() != nil {
				return
			}
			s.logf("watch stream ended (%v); reconnecting in %s", err, s.backoff())
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.backoff()):
			}
		}
	}()
	return out
}

// stream runs one `shepherd watch` process and pumps its output until the
// process exits or ctx is cancelled.
func (s WatchSource) stream(ctx context.Context, out chan<- loop.Event) error {
	args := []string{"watch"}
	if s.Interval > 0 {
		args = append(args, "--interval", s.Interval.String())
	}
	if s.Board != "" {
		args = append(args, "--board", s.Board)
	}
	cmd := exec.CommandContext(ctx, s.bin(), args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	scanErr := scan(ctx, stdout, out, s.boardID(), s.logf)
	// CommandContext kills the process on ctx cancel; reap it either way.
	_ = cmd.Wait()
	return scanErr
}

// scan reads NDJSON lines from r and emits an event per change. Split out from
// the subprocess so it's unit-tested with a plain reader. The snapshot line is
// the baseline, not a change, so it's logged and skipped.
func scan(ctx context.Context, r io.Reader, out chan<- loop.Event, board string, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // items with notes can be long
	for sc.Scan() {
		var ln watchLine
		if err := json.Unmarshal(sc.Bytes(), &ln); err != nil {
			logf("skipping unparsable watch line: %v", err)
			continue
		}
		if ln.Type == "snapshot" {
			logf("watch snapshot: %d item(s)", len(ln.Items))
			// Re-drive agentic items from the snapshot so a release that landed
			// while the stream was down (or before startup) isn't lost — the
			// snapshot is the only carrier of current state after a reconnect.
			// Dispatcher's live-status check makes replaying an already-claimed
			// task a no-op, so this is safe to fire every reconnect.
			for _, it := range ln.Items {
				if !it.Agentic {
					continue
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case out <- boardEvent("updated", it, board):
				}
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- boardEvent(ln.Type, ln.Item, board):
		}
	}
	return sc.Err()
}

// boardEvent wraps a shepherd item change as a loop event, tagged with the board
// it came from.
func boardEvent(kind string, it loop.Item, board string) loop.Event {
	return loop.Event{
		ID:     "board:" + kind + ":" + it.ID,
		Type:   "board." + kind,
		Source: "shepherd.watch",
		Data:   loop.BoardChange{Item: it, Board: board},
		At:     time.Now(),
	}
}
