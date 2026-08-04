package main

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/jwarykowski/drover/loop"
	"github.com/jwarykowski/drover/source"
)

func TestMapPayloadToEnvelope(t *testing.T) {
	requireJQ(t)
	prog := `{id: "dd:\(.id)", type: "datadog.alert", data: {title: .title, subject: .monitor_id}}`

	out, err := mapPayload(context.Background(), prog, []byte(`{"id":7,"title":"cpu high","monitor_id":"m-3"}`))
	if err != nil {
		t.Fatal(err)
	}
	// One delivery must be one line, or ExecSource reads a torn event.
	if strings.Count(strings.TrimSpace(string(out)), "\n") != 0 {
		t.Fatalf("mapping must emit one line per object: %q", out)
	}

	// Validate through drover's real decoder rather than a local copy — Scan is
	// exactly what the daemon runs over this shim's stdout.
	evs := scanEvents(t, out, "datadog")
	if len(evs) != 1 {
		t.Fatalf("drover could not decode the mapping's output: got %d events", len(evs))
	}
	e := evs[0]
	if e.ID != "dd:7" || e.Type != "datadog.alert" {
		t.Fatalf("id/type not carried: %+v", e)
	}
	if e.Data["subject"] != "m-3" {
		t.Fatalf("subject (the one-run-at-a-time key) not carried: %v", e.Data)
	}
	if e.Source != "datadog" {
		t.Fatalf("source should default to the configured name: %q", e.Source)
	}
}

// A batched delivery maps to several objects, and each must arrive as its own
// event — that is why mapPayload runs jq with -c.
func TestMapPayloadFansOutBatch(t *testing.T) {
	requireJQ(t)
	prog := `.alerts[] | {id: "a:\(.k)", type: "batch.alert", data: {subject: .k}}`

	out, err := mapPayload(context.Background(), prog, []byte(`{"alerts":[{"k":"one"},{"k":"two"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	evs := scanEvents(t, out, "batch")
	if len(evs) != 2 {
		t.Fatalf("batch should fan out to 2 events, got %d: %q", len(evs), out)
	}
	if evs[0].ID == evs[1].ID {
		t.Fatalf("fanned-out events need distinct ids or Dedup drops the second: %s", evs[0].ID)
	}
}

func TestMapPayloadRejectsEmptyAndBad(t *testing.T) {
	requireJQ(t)
	// A filter that selects nothing must fail loudly rather than emit a blank
	// line the daemon then logs as a bad event.
	if _, err := mapPayload(context.Background(), `select(.type=="nope")`, []byte(`{"type":"yes"}`)); err == nil {
		t.Fatal("empty mapping output should be an error")
	}
	if _, err := mapPayload(context.Background(), ".", []byte(`not json`)); err == nil {
		t.Fatal("unparsable payload should be an error")
	}
}

func requireJQ(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}
}

func scanEvents(t *testing.T, ndjson []byte, name string) []loop.Event {
	t.Helper()
	ch := make(chan loop.Event, 16)
	if err := source.Scan(context.Background(), bytes.NewReader(ndjson), ch, name, nil); err != nil {
		t.Fatal(err)
	}
	close(ch)
	var evs []loop.Event
	for e := range ch {
		evs = append(evs, e)
	}
	return evs
}
