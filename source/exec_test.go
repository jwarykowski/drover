package source

import (
	"context"
	"strings"
	"testing"

	"github.com/jwarykowski/drover/loop"
)

// scanTo runs Scan over a literal NDJSON body and collects what it emitted.
func scanTo(t *testing.T, body, name string) ([]loop.Event, []string) {
	t.Helper()
	out := make(chan loop.Event, 16)
	var logs []string
	if err := Scan(context.Background(), strings.NewReader(body), out, name,
		func(f string, a ...any) { logs = append(logs, f) }); err != nil {
		t.Fatalf("scan: %v", err)
	}
	close(out)
	var evs []loop.Event
	for e := range out {
		evs = append(evs, e)
	}
	return evs, logs
}

func TestScanDecodesTheEnvelope(t *testing.T) {
	body := `{"id":"e1","type":"jira.issue.created","source":"jira/eng","at":"2026-07-30T09:00:00Z","data":{"title":"login 500s","team":"ENG"}}` + "\n"
	evs, _ := scanTo(t, body, "jira")
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	e := evs[0]
	if e.ID != "e1" || e.Type != "jira.issue.created" || e.Source != "jira/eng" {
		t.Fatalf("envelope not decoded: %+v", e)
	}
	if e.Data["team"] != "ENG" || e.Data["title"] != "login 500s" {
		t.Fatalf("data not carried: %v", e.Data)
	}
	if e.At.IsZero() {
		t.Fatal("at not parsed")
	}
}

// Only id and type are required — a plugin shouldn't have to fill in fields
// drover can supply itself.
func TestScanDefaultsOptionalFields(t *testing.T) {
	evs, _ := scanTo(t, `{"id":"e1","type":"demo.ping"}`+"\n", "demo")
	e := evs[0]
	if e.Source != "demo" {
		t.Fatalf("source should default to the config name, got %q", e.Source)
	}
	if e.At.IsZero() {
		t.Fatal("at should default to receipt time")
	}
	if e.Data == nil {
		t.Fatal("data should default to an empty map, not nil")
	}
}

// One bad line must not take the source down, and an event with no id must be
// dropped rather than given a synthetic one — a synthetic id would defeat
// Dedup, re-firing everything on every restart.
func TestScanSkipsBadLinesAndKeepsGoing(t *testing.T) {
	body := strings.Join([]string{
		`not json at all`,
		`{"id":"","type":"demo.ping"}`,
		`{"id":"e2","type":""}`,
		``,
		`{"id":"e3","type":"demo.ping"}`,
	}, "\n") + "\n"
	evs, logs := scanTo(t, body, "demo")
	if len(evs) != 1 || evs[0].ID != "e3" {
		t.Fatalf("want only the good event through, got %+v", evs)
	}
	if len(logs) != 3 {
		t.Fatalf("each bad line should be logged; got %d logs", len(logs))
	}
}

// Write and Scan are the two ends of the same contract, so a shim's output must
// decode back to what it sent.
func TestWriteScanRoundTrip(t *testing.T) {
	var buf strings.Builder
	sent := loop.Event{ID: "e1", Type: "shepherd.updated", Source: "shepherd/work",
		Data: map[string]string{"subject": "a1", "title": "fix ci"}}
	if err := Write(&buf, sent); err != nil {
		t.Fatal(err)
	}
	evs, _ := scanTo(t, buf.String(), "shepherd")
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	got := evs[0]
	if got.ID != sent.ID || got.Type != sent.Type || got.Source != sent.Source {
		t.Fatalf("round trip lost the envelope: %+v", got)
	}
	if got.Data["subject"] != "a1" || got.Data["title"] != "fix ci" {
		t.Fatalf("round trip lost data: %v", got.Data)
	}
}

// A real plugin process: any command that writes the envelope drives the loop.
func TestExecSourceReadsAPluginProcess(t *testing.T) {
	line := `{"id":"e1","type":"demo.ping","data":{"title":"hi"}}`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	src := ExecSource{Name: "demo", Cmd: []string{"printf", `%s\n`, line}}
	e := <-src.Events(ctx)
	if e.ID != "e1" || e.Type != "demo.ping" || e.Data["title"] != "hi" {
		t.Fatalf("plugin event not received: %+v", e)
	}
	if e.Source != "demo" {
		t.Fatalf("source should default to the config name, got %q", e.Source)
	}
}

// A source row with no command must not spin: it logs once and stops.
func TestExecSourceWithoutCmdClosesCleanly(t *testing.T) {
	var logs []string
	src := ExecSource{Name: "broken", Logf: func(f string, a ...any) { logs = append(logs, f) }}
	ch := src.Events(context.Background())
	if _, open := <-ch; open {
		t.Fatal("a source with no cmd must emit nothing")
	}
	if len(logs) != 1 {
		t.Fatalf("want one explanatory log, got %v", logs)
	}
}
