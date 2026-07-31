package tui

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/jwarykowski/drover/loop"
	"github.com/jwarykowski/drover/store"
)

func TestLaneGroups(t *testing.T) {
	m := dashboardModel{items: []loop.Task{
		{ID: "1", Status: "hold"},
		{ID: "2", Status: "go"},
		{ID: "3", Status: "running"},
		{ID: "4", Status: "done", Done: true},
		{ID: "5", Status: "running", Done: true}, // done wins over a stale status
	}}
	if got := len(m.lane("held")); got != 2 {
		t.Fatalf("held = %d, want 2 (hold+go)", got)
	}
	if got := len(m.lane("running")); got != 1 {
		t.Fatalf("running = %d, want 1", got)
	}
	if got := len(m.lane("done")); got != 2 {
		t.Fatalf("done = %d, want 2 (Done flag wins)", got)
	}
}

// ringTexts pulls just the message text out of a snapshot, dropping timestamps
// so tests stay deterministic.
func ringTexts(es []logEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.text
	}
	return out
}

func TestFormatJobLog(t *testing.T) {
	// Stamped lines ("15:04:05\t<json>"), plus a raw stderr line with no stamp.
	raw := strings.Join([]string{
		"15:04:01\t" + `{"type":"system","subtype":"init"}`,
		"15:04:02\t" + `{"type":"assistant","message":{"content":[{"type":"text","text":"on it"},{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`,
		"15:04:03\t" + `{"type":"result","subtype":"success","result":"done"}`,
		"some stderr diagnostic line",
	}, "\n")
	rows := formatJobLog([]byte(raw))
	want := []jobRow{
		{"15:04:01", "system", "init"},
		{"15:04:02", "agent", "on it"},
		{"15:04:02", "tool", `Bash {"command":"ls"}`},
		{"15:04:03", "result", "success: done"},
		{"", "stderr", "some stderr diagnostic line"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("formatJobLog:\n got %#v\nwant %#v", rows, want)
	}
}

func TestJobDetailNavOpensSelectedLane(t *testing.T) {
	m := dashboardModel{items: []loop.Task{
		{ID: "h1", Status: "hold", Text: "held one"},
		{ID: "r1", Status: "running", Text: "running one"},
	}}
	// right → move to the running lane; enter → open its detail.
	mod, _ := m.updateMain(tea.KeyMsg{Type: tea.KeyRight})
	m = mod.(dashboardModel)
	if m.laneIdx != 1 {
		t.Fatalf("right should select the running lane, got laneIdx=%d", m.laneIdx)
	}
	mod, _ = m.updateMain(tea.KeyMsg{Type: tea.KeyEnter})
	m = mod.(dashboardModel)
	if m.mode != modeJobDetail {
		t.Fatalf("enter should open job detail, mode=%d", m.mode)
	}
	if m.job.ID != "r1" {
		t.Fatalf("opened wrong job: %q", m.job.ID)
	}
}

func TestReleaseOnlyOnHeldLane(t *testing.T) {
	// `g` on a non-held lane must not attempt a release (no panic, no nil-store call).
	m := dashboardModel{laneIdx: 1, items: []loop.Task{{ID: "r1", Status: "running"}}}
	mod, _ := m.updateMain(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	if mod.(dashboardModel).mode != modeMain {
		t.Fatal("release key should be a no-op on the running lane")
	}
}

func TestJobDetailOpensLogWindow(t *testing.T) {
	m := dashboardModel{mode: modeJobDetail, job: loop.Task{ID: "j1", Status: "hold"}}
	mod, _ := m.updateJobDetail(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	if mod.(dashboardModel).mode != modeJobLog {
		t.Fatal("l should open the full-window log")
	}
	// esc from the log returns to the detail, not all the way out.
	back, _ := mod.(dashboardModel).updateJobLog(tea.KeyMsg{Type: tea.KeyEsc})
	if back.(dashboardModel).mode != modeJobDetail {
		t.Fatal("esc in the log window should return to the job detail")
	}
}

func TestJobDetailDeleteRemovesJobAndLog(t *testing.T) {
	dir := t.TempDir()
	fs, _ := store.OpenFileStore("")
	it, _ := fs.Add(context.Background(), loop.Spec{Text: "run", Status: "hold"})
	logPath := filepath.Join(dir, it.ID+".jsonl")
	if err := os.WriteFile(logPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := dashboardModel{mode: modeJobDetail, job: it, logDir: dir,
		ctrl: &Controller{tasks: fs, ring: newRing(4)}}

	mod, _ := m.updateJobDetail(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if mod.(dashboardModel).mode != modeMain {
		t.Fatal("delete should return to the board")
	}
	if got, _ := fs.List(context.Background(), loop.Filter{IncludeDone: true}); len(got) != 0 {
		t.Fatalf("delete must drop the task: %+v", got)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatal("delete must remove the job log")
	}
}

func TestJobDetailRestartRequeuesAtHold(t *testing.T) {
	fs, _ := store.OpenFileStore("")
	it, _ := fs.Add(context.Background(), loop.Spec{Text: "run", Status: "go"})
	_ = fs.SetStatus(context.Background(), it.ID, "done")
	_ = fs.Archive(context.Background(), it.ID)
	m := dashboardModel{mode: modeJobDetail, job: it, logDir: t.TempDir(),
		ctrl: &Controller{tasks: fs, ring: newRing(4)}}

	mod, _ := m.updateJobDetail(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	rm := mod.(dashboardModel)
	if rm.mode != modeJobDetail {
		t.Fatal("restart should stay in the detail view")
	}
	if rm.job.Done || rm.job.Status != "hold" {
		t.Fatalf("restart must re-park the shown job at hold, got %+v", rm.job)
	}
	if got, _ := fs.List(context.Background(), loop.Filter{IncludeDone: true}); len(got) != 1 || got[0].Status != "hold" {
		t.Fatalf("restart must re-queue the task at hold: %+v", got)
	}
}

func TestJobDetailResyncsFromStore(t *testing.T) {
	fs, _ := store.OpenFileStore("")
	it, _ := fs.Add(context.Background(), loop.Spec{Text: "run", Status: "go"})
	m := dashboardModel{mode: modeJobDetail, job: it,
		ctrl: &Controller{tasks: fs, ring: newRing(4)}}

	// daemon claims the task while the detail view is open.
	_ = fs.SetStatus(context.Background(), it.ID, "running")
	m.refresh()
	if m.job.Status != "running" {
		t.Fatalf("detail view stale: job status = %q, want running", m.job.Status)
	}
}

func TestAllBoardsGroupsLanesByBoard(t *testing.T) {
	items := []loop.Task{
		{ID: "1", Status: "hold", Source: "work", Text: "a"},
		{ID: "2", Status: "hold", Source: "home", Text: "b"},
		{ID: "3", Status: "hold", Source: "", Text: "c"}, // sensed
	}
	// all-sources view (source == "") groups with sub-headers.
	all := ansi.Strip(strings.Join(dashboardModel{source: "", items: items}.laneCol("held", "held", -1, 20, 40, false), "\n"))
	for _, want := range []string{"─ home", "─ work", "unattributed"} {
		if !strings.Contains(all, want) {
			t.Fatalf("all-sources lane missing group %q:\n%s", want, all)
		}
	}
	// a selected source shows no sub-headers.
	one := ansi.Strip(strings.Join(dashboardModel{source: "work", items: items[:1]}.laneCol("held", "held", -1, 20, 40, false), "\n"))
	if strings.Contains(one, "─ work") {
		t.Fatalf("single-source lane should not group by source:\n%s", one)
	}
}

func TestPickerIndex(t *testing.T) {
	names := []string{"shepherd/default", "acme-api"}
	if got := pickerIndex(names, ""); got != 0 {
		t.Fatalf("all-sources should be row 0, got %d", got)
	}
	if got := pickerIndex(names, "acme-api"); got != 2 {
		t.Fatalf("acme-api should be row 2 (after all-sources + the first), got %d", got)
	}
	if got := pickerIndex(names, "ghost"); got != 0 {
		t.Fatalf("unknown source should fall back to all-sources, got %d", got)
	}
}

// The source filter is exact: a run belongs to the source that raised it and to
// no other, so selecting one hides every other source's runs.
func TestForSourceFiltersBySelection(t *testing.T) {
	items := []loop.Task{
		{ID: "1", Source: "work"},
		{ID: "2", Source: "home"},
		{ID: "3", Source: "acme-api"},
		{ID: "4", Source: ""},
	}
	got := dashboardModel{source: "work"}.forSource(append([]loop.Task{}, items...))
	if ids := itemIDs(got); !reflect.DeepEqual(ids, []string{"1"}) {
		t.Fatalf("work source = %v, want [1]", ids)
	}
	got = dashboardModel{source: "acme-api"}.forSource(append([]loop.Task{}, items...))
	if ids := itemIDs(got); !reflect.DeepEqual(ids, []string{"3"}) {
		t.Fatalf("acme-api source = %v, want [3]", ids)
	}
	// all sources (empty selection): no filter.
	got = dashboardModel{source: ""}.forSource(append([]loop.Task{}, items...))
	if ids := itemIDs(got); !reflect.DeepEqual(ids, []string{"1", "2", "3", "4"}) {
		t.Fatalf("all sources = %v, want [1 2 3 4]", ids)
	}
}

// knownSources drives the picker, so it must list each source once, sorted, and
// ignore runs with no source.
func TestKnownSources(t *testing.T) {
	m := dashboardModel{items: []loop.Task{
		{Source: "work"}, {Source: "acme-api"}, {Source: "work"}, {Source: ""},
	}}
	if got := m.knownSources(); !reflect.DeepEqual(got, []string{"acme-api", "work"}) {
		t.Fatalf("knownSources = %v, want [acme-api work]", got)
	}
}

func itemIDs(items []loop.Task) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}

func TestRingCapAndOrder(t *testing.T) {
	r := newRing(3)
	r.Logf("a")
	r.Logf("b")
	r.Logf("c")
	r.Logf("d") // evicts oldest
	if got := ringTexts(r.Snapshot()); !reflect.DeepEqual(got, []string{"b", "c", "d"}) {
		t.Fatalf("snapshot = %v, want [b c d]", got)
	}
}

func TestRingWriteSplitsLines(t *testing.T) {
	r := newRing(10)
	_, _ = r.Write([]byte("line1\nline2\n"))
	_, _ = r.Write([]byte("line3"))
	if got := ringTexts(r.Snapshot()); !reflect.DeepEqual(got, []string{"line1", "line2", "line3"}) {
		t.Fatalf("snapshot = %v", got)
	}
}

func TestRingSkipsBlank(t *testing.T) {
	r := newRing(10)
	_, _ = r.Write([]byte("\n\n"))
	r.Logf("   ")
	if got := r.Snapshot(); len(got) != 0 {
		t.Fatalf("blank lines should be skipped, got %v", got)
	}
}

func TestRingSnapshotIsCopy(t *testing.T) {
	r := newRing(10)
	r.Logf("x")
	s := r.Snapshot()
	s[0].text = "mutated"
	if r.Snapshot()[0].text != "x" {
		t.Fatal("snapshot must return a copy")
	}
}

func TestRenderLogWrapsAndColumns(t *testing.T) {
	e := logEntry{ts: time.Date(2026, 7, 26, 15, 4, 5, 0, time.UTC), text: strings.Repeat("word ", 40)}
	rows := renderLog(e, 40)
	if len(rows) < 2 {
		t.Fatalf("expected wrapping into multiple rows, got %d", len(rows))
	}
	if !strings.Contains(rows[0], "15:04:05") {
		t.Fatalf("first row missing timestamp: %q", rows[0])
	}
	// continuation rows blank the timestamp column so text stays aligned.
	if strings.Contains(ansi.Strip(rows[1]), "15:04:05") {
		t.Fatalf("continuation row should not repeat the timestamp: %q", rows[1])
	}
	for i, r := range rows {
		if w := ansi.StringWidth(r); w > 40 {
			t.Fatalf("row %d width %d exceeds 40: %q", i, w, r)
		}
	}
}
