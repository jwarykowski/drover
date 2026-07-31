// Package source ingests events over drover's common protocol and fans them
// into one stream.
//
// There are two transports and one envelope. A local source is a process drover
// spawns that writes NDJSON to stdout; a remote source POSTs the same NDJSON to
// a listener. Neither knows anything about drover beyond the envelope below, so
// a source can be written in any language — drover's own GitHub and shepherd
// shims are ordinary local sources with no privileged path.
package source

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jwarykowski/drover/loop"
)

// wire is the on-the-wire event: one JSON object per line.
//
//	{"id":"github/acme/api:pr:12:merged","type":"github.pull_request.merged",
//	 "source":"acme-api","at":"2026-07-30T09:00:00Z",
//	 "data":{"repo":"acme/api","title":"fix login","url":"https://…"}}
//
// Only id and type are required. at defaults to receipt time, source to the
// configured source name, and data to empty.
//
// id must be unique per logical event, not per subject: two edits to the same
// item are two events and need two ids, or Dedup will swallow the second one
// forever. Use the subject key below to say they concern the same thing.
//
// data is free-form — an action matches on it, the prompt renders it, and a
// target templates from it — but three keys are conventional, because drover
// itself reads them when it has to:
//
//	title    what the run is called in the lanes
//	url      a link a human can open
//	subject  a STABLE id for the thing this event concerns (a PR url, a board
//	         item id, an issue key). It is the one-run-at-a-time key: while a
//	         run for a subject is in flight, further events about that same
//	         subject do not park a second one. Falls back to url, then to the
//	         event id, so a source that sets none simply never dedups.
//
// A source is free to send anything else; unknown keys reach the prompt and the
// action filters untouched.
type wire struct {
	ID     string            `json:"id"`
	Type   string            `json:"type"`
	Source string            `json:"source,omitempty"`
	At     time.Time         `json:"at,omitempty"`
	Data   map[string]string `json:"data,omitempty"`
}

// Write emits one event in wire form, newline-terminated. Source shims use this
// so there is exactly one encoder and the protocol cannot drift between
// drover's own sources and third-party ones.
func Write(w io.Writer, e loop.Event) error {
	b, err := json.Marshal(wire{ID: e.ID, Type: e.Type, Source: e.Source, At: e.At, Data: e.Data})
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// decode parses one wire line into an event, defaulting the optional fields.
//
// A missing id is rejected rather than filled in: the id is the dedup key, and
// a synthesised one would silently defeat Dedup — every restart would re-fire
// every event. Failing the line is loud and fixable; a random id is neither.
func decode(line []byte, srcName string) (loop.Event, error) {
	var w wire
	if err := json.Unmarshal(line, &w); err != nil {
		return loop.Event{}, fmt.Errorf("bad event json: %w", err)
	}
	if strings.TrimSpace(w.ID) == "" {
		return loop.Event{}, fmt.Errorf("event has no id (the dedup key): %s", truncate(string(line), 120))
	}
	if strings.TrimSpace(w.Type) == "" {
		return loop.Event{}, fmt.Errorf("event %q has no type", w.ID)
	}
	if w.Source == "" {
		w.Source = srcName
	}
	if w.At.IsZero() {
		w.At = time.Now()
	}
	if w.Data == nil {
		w.Data = map[string]string{}
	}
	return loop.Event{ID: w.ID, Type: w.Type, Source: w.Source, Data: w.Data, At: w.At}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
