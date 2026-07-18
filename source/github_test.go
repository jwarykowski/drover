package source

import (
	"context"
	"testing"
	"time"

	"github.com/jwarykowski/drover/loop"
)

// collect drains up to n events or until the source closes / times out.
func collect(t *testing.T, src loop.Source, n int) []loop.Event {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := src.Events(ctx)
	var got []loop.Event
	for len(got) < n {
		select {
		case e, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, e)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out after %d events", len(got))
		}
	}
	return got
}

func TestGitHubSourceSeedsThenFiresOnlyNewer(t *testing.T) {
	// Poll 1 shows PRs 5 and 6 (seed, no fire). Poll 2 adds PR 7 (fire once).
	polls := [][]byte{
		[]byte(`[{"number":6,"title":"b","url":"u6","mergeCommit":{"oid":"s6"}},{"number":5,"title":"a","url":"u5","mergeCommit":{"oid":"s5"}}]`),
		[]byte(`[{"number":7,"title":"c","url":"u7","mergeCommit":{"oid":"s7"}},{"number":6,"title":"b","url":"u6","mergeCommit":{"oid":"s6"}}]`),
	}
	i := 0
	src := GitHubSource{
		Repo:     "acme/api",
		Interval: time.Millisecond,
		run: func(_ context.Context, _, _ string) ([]byte, error) {
			p := polls[i]
			if i < len(polls)-1 {
				i++
			}
			return p, nil
		},
	}
	got := collect(t, src, 1)
	if len(got) != 1 {
		t.Fatalf("want exactly 1 event (only PR 7), got %d", len(got))
	}
	if got[0].Kind != "github.merged" || got[0].Payload["pr"] != 7 {
		t.Fatalf("want github.merged for PR 7, got %s pr=%v", got[0].Kind, got[0].Payload["pr"])
	}
	if got[0].Payload["url"] != "u7" || got[0].Payload["repo"] != "acme/api" {
		t.Fatalf("payload not populated: %#v", got[0].Payload)
	}
}
