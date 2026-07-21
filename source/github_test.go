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

func TestDecodeGitHubPRs(t *testing.T) {
	// Out of order and one unmerged PR (empty mergedAt) that must be dropped.
	raw := []byte(`[
	  {"number":6,"title":"b","url":"u6","mergedAt":"t","mergeCommit":{"oid":"s6"}},
	  {"number":5,"title":"a","url":"u5","mergedAt":"t","mergeCommit":{"oid":"s5"}},
	  {"number":7,"title":"open","url":"u7","mergedAt":"","mergeCommit":{"oid":""}}
	]`)
	evs, err := decodeGitHubPRs("acme/api", raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("want 2 merged events (unmerged dropped), got %d", len(evs))
	}
	// Ascending by number: 5 then 6.
	if evs[0].ID != "github/acme/api:pr:5:merged" || evs[0].Type != "github.pull_request.merged" {
		t.Fatalf("first event wrong: %+v", evs[0])
	}
	sig, ok := evs[0].Data.(loop.Signal)
	if !ok || sig.Title != "a" || sig.URL != "u5" || sig.Repo != "acme/api" {
		t.Fatalf("signal not populated: %#v", evs[0].Data)
	}
}

func TestGitHubSourceEmitsMergedPRs(t *testing.T) {
	src := GitHubSource{
		Repo:     "acme/api",
		Interval: time.Millisecond,
		run: func(_ context.Context, _, _ string) ([]byte, error) {
			return []byte(`[{"number":5,"title":"a","url":"u5","mergedAt":"t","mergeCommit":{"oid":"s5"}},{"number":6,"title":"b","url":"u6","mergedAt":"t","mergeCommit":{"oid":"s6"}}]`), nil
		},
	}
	got := collect(t, src, 2)
	if got[0].ID != "github/acme/api:pr:5:merged" || got[1].ID != "github/acme/api:pr:6:merged" {
		t.Fatalf("want PRs 5 then 6, got %q, %q", got[0].ID, got[1].ID)
	}
}

func TestGitHubSourceSeedIDs(t *testing.T) {
	src := GitHubSource{
		Repo: "acme/api",
		run: func(_ context.Context, _, _ string) ([]byte, error) {
			return []byte(`[{"number":5,"title":"a","url":"u5","mergedAt":"t","mergeCommit":{"oid":"s5"}},{"number":6,"title":"b","url":"u6","mergedAt":"t","mergeCommit":{"oid":"s6"}}]`), nil
		},
	}
	ids, err := src.SeedIDs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "github/acme/api:pr:5:merged" {
		t.Fatalf("seed ids wrong: %v", ids)
	}
}
