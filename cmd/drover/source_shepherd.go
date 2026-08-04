package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jwarykowski/drover/loop"
)

// `drover source shepherd` is a source plugin: it runs `shepherd watch`,
// translates each change into a drover event, and writes it to stdout.
//
//	[[source]]
//	name  = "shepherd"
//	cmd   = ["drover", "source", "shepherd"]
//	types = ["shepherd.added", "shepherd.updated", "shepherd.removed", "shepherd.archived"]
//
// This file is the ONLY place in drover that knows shepherd exists. Everything
// downstream sees an ordinary event, so shepherd is swappable for any other
// board — or dropped entirely — by editing one config row.
//
// shepherd is trigger-only: drover reads the board and never writes to it. A
// matched change parks a run in drover's own store, leaving the person's todo
// exactly as they set it.
func sourceShepherd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("source shepherd", flag.ContinueOnError)
	board := fs.String("board", "", "board to watch; empty watches the default board")
	all := fs.Bool("all", false, "watch every board instead of one")
	bin := fs.String("bin", "shepherd", "shepherd binary")
	interval := fs.Duration("interval", 0, "poll interval passed to shepherd watch; 0 uses its default")
	backoff := fs.Duration("backoff", time.Second, "wait before reconnecting a dropped stream")
	if err := fs.Parse(args); err != nil {
		return err
	}

	out := &lineWriter{w: os.Stdout}
	if !*all {
		// One `shepherd boards` read serves the board's working directory, which
		// rides along in every event as {{dir}}.
		dirs, err := shepherdBoards(ctx, *bin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "list boards: %v; continuing without board dirs\n", err)
			dirs = map[string]string{}
		}
		watchBoard(ctx, out, *bin, *board, dirs[*board], *interval, *backoff)
		return nil
	}
	watchAll(ctx, out, *bin, *interval, *backoff, boardRescan)
	return nil
}

// boardRescan is how often --all re-reads `shepherd boards`. The list is not a
// startup constant: a board created after the shim started would otherwise
// never be watched, and its todos would silently raise no events.
const boardRescan = 30 * time.Second

// watchAll watches every board, re-reading the board list on each tick so boards
// created later get picked up. A watcher, once started, runs for the life of the
// shim.
// ponytail: a deleted board leaves its watcher polling a missing file (harmless,
// it just never fires); stop them per board if that ever costs anything.
func watchAll(ctx context.Context, out *lineWriter, bin string, interval, backoff, rescan time.Duration) {
	var wg sync.WaitGroup
	watching := map[string]bool{}
	for ctx.Err() == nil {
		dirs, err := shepherdBoards(ctx, bin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "list boards: %v\n", err)
			// Nothing watched yet means a cold start with shepherd unreachable —
			// fall back to the default board so the source is not silently dead.
			if len(watching) == 0 {
				dirs = map[string]string{"": ""}
			}
		}
		names := make([]string, 0, len(dirs))
		for name := range dirs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, b := range names {
			if watching[b] {
				continue
			}
			watching[b] = true
			wg.Add(1)
			go func(b, dir string) {
				defer wg.Done()
				watchBoard(ctx, out, bin, b, dir, interval, backoff)
			}(b, dirs[b])
		}
		select {
		case <-ctx.Done():
		case <-time.After(rescan):
		}
	}
	wg.Wait()
}

// watchBoard runs `shepherd watch` for one board until ctx is cancelled,
// reconnecting when the stream drops (shepherd restarted, board rotated).
func watchBoard(ctx context.Context, out *lineWriter, bin, board, dir string, interval, backoff time.Duration) {
	for ctx.Err() == nil {
		err := streamBoard(ctx, out, bin, board, dir, interval)
		if ctx.Err() != nil {
			return
		}
		fmt.Fprintf(os.Stderr, "watch %s ended (%v); reconnecting in %s\n", boardID(board), err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

func streamBoard(ctx context.Context, out *lineWriter, bin, board, dir string, interval time.Duration) error {
	args := []string{"watch"}
	if interval > 0 {
		args = append(args, "--interval", interval.String())
	}
	if board != "" {
		args = append(args, "--board", board)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	scanErr := scanShepherd(ctx, stdout, out, boardID(board), dir)
	_ = cmd.Wait() // CommandContext kills it on cancel; reap it either way
	return scanErr
}

// shepherdItem is the slice of a shepherd item this shim carries through. Only
// the fields an action might match on or a prompt might render are forwarded —
// the rest of shepherd's schema stays shepherd's business.
type shepherdItem struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Link string `json:"link"`
	Note string `json:"note"`
}

// watchLine is one NDJSON record from `shepherd watch`: a change carries item,
// the opening snapshot carries items.
type watchLine struct {
	Type  string         `json:"type"`
	Item  shepherdItem   `json:"item"`
	Items []shepherdItem `json:"items"`
}

// scanShepherd translates shepherd's watch stream into drover events. The
// snapshot line is a baseline rather than a change, so it is skipped: re-firing
// every item on each reconnect would park a duplicate run for the whole board.
func scanShepherd(ctx context.Context, r io.Reader, out *lineWriter, board, dir string) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // items with notes can be long
	for sc.Scan() {
		var ln watchLine
		if err := json.Unmarshal(sc.Bytes(), &ln); err != nil {
			fmt.Fprintf(os.Stderr, "skipping unparsable watch line: %v\n", err)
			continue
		}
		if ln.Type == "snapshot" {
			fmt.Fprintf(os.Stderr, "watch snapshot: %d item(s) on %s\n", len(ln.Items), board)
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		out.emit(boardEvent(ln.Type, ln.Item, board, dir, time.Now()))
	}
	return sc.Err()
}

// boardEvent wraps a shepherd change as a drover event. dir travels in the data
// so an action can set target = "{{dir}}" and have the agent run in the board's
// own directory — drover itself never resolves a board path.
//
// The id carries the change time, not just the item id: editing one item twice
// is two events, and reusing an id would have drover's dedup swallow every edit
// after the first. Recurrence is expressed by subject instead, which holds the
// item id — that keeps one run per item in flight without hiding later changes.
func boardEvent(kind string, it shepherdItem, board, dir string, at time.Time) loop.Event {
	data := map[string]string{
		"title":   it.Text,
		"subject": it.ID,
		"item_id": it.ID,
		"board":   board,
	}
	if it.Link != "" {
		data["url"] = it.Link
	}
	if it.Note != "" {
		data["note"] = it.Note
	}
	if dir != "" {
		data["dir"] = dir
	}
	return loop.Event{
		ID:     fmt.Sprintf("shepherd:%s:%s:%d", kind, it.ID, at.UnixNano()),
		Type:   "shepherd." + kind,
		Source: "shepherd/" + board,
		Data:   data,
		At:     at,
	}
}

// shepherdBoards reads `shepherd boards --json` once, returning name → working
// directory. The default board is keyed by the empty string, which is the
// --board argument shepherd expects for it.
func shepherdBoards(ctx context.Context, bin string) (map[string]string, error) {
	out, err := exec.CommandContext(ctx, bin, "boards", "--json").Output()
	if err != nil {
		return nil, err
	}
	var boards []struct {
		Name string `json:"name"`
		Dir  string `json:"dir"`
	}
	if err := json.Unmarshal(out, &boards); err != nil {
		return nil, err
	}
	dirs := make(map[string]string, len(boards))
	for _, b := range boards {
		name := b.Name
		if name == "default" {
			name = ""
		}
		dirs[name] = strings.TrimSpace(b.Dir)
	}
	return dirs, nil
}

func boardID(board string) string {
	if board == "" {
		return "default"
	}
	return board
}
