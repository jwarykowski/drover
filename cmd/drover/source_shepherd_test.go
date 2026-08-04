package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// emitted decodes what the shim wrote to stdout.
type emitted struct {
	ID     string            `json:"id"`
	Type   string            `json:"type"`
	Source string            `json:"source"`
	Data   map[string]string `json:"data"`
}

func drain(t *testing.T, body, board, dir string) []emitted {
	t.Helper()
	var buf bytes.Buffer
	out := &lineWriter{w: &buf}
	if err := scanShepherd(context.Background(), strings.NewReader(body), out, board, dir); err != nil {
		t.Fatalf("scan: %v", err)
	}
	var evs []emitted
	for _, ln := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if ln == "" {
			continue
		}
		var e emitted
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			t.Fatalf("shim wrote an undecodable line %q: %v", ln, err)
		}
		evs = append(evs, e)
	}
	return evs
}

func TestShepherdShimTranslatesChanges(t *testing.T) {
	body := strings.Join([]string{
		`{"type":"snapshot","items":[{"id":"a1","text":"one"},{"id":"a2","text":"two"}]}`,
		`{"type":"updated","item":{"id":"a1","text":"fix ci","link":"https://x/1","note":"n"}}`,
	}, "\n") + "\n"

	evs := drain(t, body, "work", "/src/work")
	if len(evs) != 1 {
		t.Fatalf("want only the change through, got %d: %+v", len(evs), evs)
	}
	e := evs[0]
	if e.Type != "shepherd.updated" || e.Source != "shepherd/work" {
		t.Fatalf("envelope wrong: %+v", e)
	}
	if e.Data["title"] != "fix ci" || e.Data["url"] != "https://x/1" || e.Data["note"] != "n" {
		t.Fatalf("item fields not carried: %v", e.Data)
	}
	// The board dir rides along so an action can set target = "{{dir}}" — drover
	// itself never resolves a board path.
	if e.Data["dir"] != "/src/work" {
		t.Fatalf("board dir not carried: %v", e.Data)
	}
	if e.Data["subject"] != "a1" {
		t.Fatalf("subject must be the item id: %v", e.Data)
	}
}

// The opening snapshot is a baseline, not a change: replaying it would park a
// duplicate run for every item on the board on every reconnect.
func TestShepherdShimSkipsTheSnapshot(t *testing.T) {
	body := `{"type":"snapshot","items":[{"id":"a1","text":"one"}]}` + "\n"
	if evs := drain(t, body, "work", ""); len(evs) != 0 {
		t.Fatalf("snapshot must not emit events, got %+v", evs)
	}
}

// Editing one item twice is two events. If both carried the same id, drover's
// dedup would swallow every edit after the first — the id must vary while the
// subject stays put.
func TestShepherdShimGivesEachChangeItsOwnID(t *testing.T) {
	it := shepherdItem{ID: "a1", Text: "x"}
	first := boardEvent("updated", it, "work", "", time.Unix(0, 1))
	second := boardEvent("updated", it, "work", "", time.Unix(0, 2))
	if first.ID == second.ID {
		t.Fatalf("two changes to one item share an id (%q); later edits would be deduped away", first.ID)
	}
	if first.Data["subject"] != second.Data["subject"] {
		t.Fatal("both changes concern the same item, so the subject must match")
	}
}

// A malformed line must not take the stream down — shepherd may print
// something unexpected, and one bad line should cost one event, not the source.
func TestShepherdShimSkipsUnparsableLines(t *testing.T) {
	body := strings.Join([]string{
		`not json`,
		`{"type":"added","item":{"id":"a1","text":"one"}}`,
	}, "\n") + "\n"
	evs := drain(t, body, "", "")
	if len(evs) != 1 || evs[0].Type != "shepherd.added" {
		t.Fatalf("want the good line through, got %+v", evs)
	}
	if evs[0].Source != "shepherd/" {
		t.Fatalf("source should carry the board id: %q", evs[0].Source)
	}
}

func TestBoardIDNamesTheDefaultBoard(t *testing.T) {
	if got := boardID(""); got != "default" {
		t.Fatalf("empty board should read as %q, got %q", "default", got)
	}
	if got := boardID("work"); got != "work" {
		t.Fatalf("named board = %q", got)
	}
}

// A board created after the shim started must still be watched: the board list
// is re-read, not frozen at startup. Regression — --all used to snapshot the
// list once, so todos on a newer board raised no events until a restart.
func TestWatchAllPicksUpBoardsCreatedLater(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "boards.json")
	spawned := filepath.Join(dir, "spawned.txt")
	write(t, list, `[{"name":"one"}]`)

	bin := filepath.Join(dir, "shepherd")
	write(t, bin, "#!/bin/sh\n"+
		"if [ \"$1\" = boards ]; then cat "+list+"; exit 0; fi\n"+
		"echo \"$@\" >> "+spawned+"\n"+
		"while :; do sleep 1; done\n")
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchAll(ctx, &lineWriter{w: io.Discard}, bin, 0, time.Millisecond, 20*time.Millisecond)
	}()
	defer func() { cancel(); <-done }()

	waitForLine(t, spawned, "watch --board one")
	write(t, list, `[{"name":"one"},{"name":"two"}]`)
	waitForLine(t, spawned, "watch --board two")
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func waitForLine(t *testing.T, path, want string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	b, _ := os.ReadFile(path)
	t.Fatalf("never spawned %q; got:\n%s", want, b)
}
