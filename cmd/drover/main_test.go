package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/jwarykowski/drover/registry"
)

func TestGithubWatchesDistinctAndPerAction(t *testing.T) {
	reg := &registry.Registry{Actions: []registry.Action{
		{On: "github.pull_request.merged", Repo: "acme/api", Source: "poll", Base: "main", Interval: "30s"},
		{On: "github.issues.opened", Repo: "acme/api"},       // dup repo, first wins
		{On: "github.pull_request.merged", Repo: "acme/web"}, // defaults
		{On: "github.pull_request.merged", Repo: ""},         // agnostic, skipped
		{On: "sentry.issue.opened", Repo: "acme/api"},        // not github, skipped
		{On: "board.updated"}, // not github, skipped
	}}
	got := githubWatches(reg, "master", "forward", time.Minute, nil)
	want := []repoWatch{
		{repo: "acme/api", base: "main", source: "poll", interval: 30 * time.Second},
		{repo: "acme/web", base: "master", source: "forward", interval: time.Minute},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("githubWatches = %#v, want %#v", got, want)
	}
	if bare := agnosticGithubActions(reg); len(bare) != 1 {
		t.Fatalf("agnosticGithubActions = %v, want 1 entry", bare)
	}
}

func TestAddrForIncrementsPort(t *testing.T) {
	cases := []struct {
		base string
		i    int
		want string
	}{
		{"127.0.0.1:9099", 0, "127.0.0.1:9099"},
		{"127.0.0.1:9099", 1, "127.0.0.1:9100"},
		{"127.0.0.1:9099", 3, "127.0.0.1:9102"},
		{"garbage", 2, "garbage"}, // unparseable → base
	}
	for _, c := range cases {
		if got := addrFor(c.base, c.i); got != c.want {
			t.Errorf("addrFor(%q, %d) = %q, want %q", c.base, c.i, got, c.want)
		}
	}
}
