package daemon

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jwarykowski/drover/config"
	"github.com/jwarykowski/drover/source"
)

func cfg(sources ...config.Source) *config.Config {
	c := &config.Config{}
	c.Source = sources
	return c
}

// One transport per row, chosen by which field is set. This is the whole of
// "everything is a plugin": nothing here knows what any source actually is.
func TestBuildChoosesTransportPerRow(t *testing.T) {
	cf := cfg(
		config.Source{Name: "shepherd", Cmd: []string{"drover", "source", "shepherd"}},
		config.Source{Name: "sentry", HTTP: "127.0.0.1:9100"},
	)
	srcs, names := build(cf, source.NewMemSeen(), func(string, ...any) {})
	if len(srcs) != 2 {
		t.Fatalf("want 2 sources, got %d", len(srcs))
	}
	if names[0] != "shepherd" || names[1] != "sentry" {
		t.Fatalf("names = %v", names)
	}
	// Every configured source is deduped by event id; only the task re-drive is
	// exempt, and that is added outside build.
	for i, s := range srcs {
		if _, ok := s.(source.Dedup); !ok {
			t.Fatalf("source %d is not deduped: %T", i, s)
		}
	}
}

// A malformed row must not take the daemon down: the other sources still run,
// and the operator is told which row was dropped and why.
func TestBuildSkipsUnusableRowsWithoutFailing(t *testing.T) {
	cf := cfg(
		config.Source{Name: "neither"},
		config.Source{Name: "both", Cmd: []string{"x"}, HTTP: "127.0.0.1:1"},
		config.Source{Name: "good", Cmd: []string{"drover", "source", "github"}},
	)
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	srcs, names := build(cf, source.NewMemSeen(), logf)

	if len(srcs) != 1 || len(names) != 1 || names[0] != "good" {
		t.Fatalf("only the usable row should run, got %v", names)
	}
	if len(logs) != 2 {
		t.Fatalf("each skipped row should be logged, got %v", logs)
	}
	// The operator has to be able to tell WHICH row was dropped.
	for _, want := range []string{"neither", "both"} {
		found := false
		for _, l := range logs {
			if strings.Contains(l, want) && strings.Contains(l, "skipping") {
				found = true
			}
		}
		if !found {
			t.Fatalf("no skip log naming %q: %v", want, logs)
		}
	}
}

func TestBuildWithNoSourcesIsEmptyNotAnError(t *testing.T) {
	srcs, names := build(cfg(), source.NewMemSeen(), func(string, ...any) {})
	if len(srcs) != 0 || len(names) != 0 {
		t.Fatalf("an empty config should build nothing, got %v", names)
	}
}
