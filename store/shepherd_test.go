package store

import (
	"testing"

	"github.com/jwarykowski/drover/loop"
)

func TestBuildAddText(t *testing.T) {
	got := buildAddText(loop.Spec{
		Text: "CI failed: build broke", Category: "ci", Priority: "h",
		Due: "2026-08-01", Link: "https://ci/42", Note: "see logs",
	})
	want := "CI failed: build broke @ci !h due:2026-08-01 link:https://ci/42 note:see logs"
	if got != want {
		t.Errorf("\n got: %q\nwant: %q", got, want)
	}

	// Empty tokens are omitted; note always trails.
	if got := buildAddText(loop.Spec{Text: "bare"}); got != "bare" {
		t.Errorf("bare spec: got %q", got)
	}
}

func TestParseErrorEnvelope(t *testing.T) {
	if err := parseErrorEnvelope([]byte(`{"error":"not_found","detail":"999"}`)); err == nil {
		t.Error("not_found: want error, got nil")
	}
	if err := parseErrorEnvelope([]byte(`[{"id":"x"}]`)); err != nil {
		t.Errorf("success array: want nil, got %v", err)
	}
}
