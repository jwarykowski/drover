package policy

import (
	"context"
	"testing"

	"github.com/jwarykowski/drover/loop"
)

// routeStub tags its output so a test can tell which route fired.
type routeStub struct{ tag string }

func (s routeStub) Decide(context.Context, loop.Context) []loop.Action {
	return []loop.Action{loop.SetStatus{ID: s.tag}}
}

func TestPolicyRouterPrefix(t *testing.T) {
	r := PolicyRouter{
		{Prefix: "board.", Policy: routeStub{"board"}},
		{Prefix: "github.pull_request.merged", Policy: routeStub{"merge"}},
		{Prefix: "", Policy: routeStub{"catchall"}},
	}
	tag := func(evType string) string {
		acts := r.Decide(context.Background(), loop.Context{Event: loop.Event{Type: evType}})
		return acts[0].(loop.SetStatus).ID
	}
	if got := tag("board.updated"); got != "board" {
		t.Errorf("board prefix: %q", got)
	}
	if got := tag("github.pull_request.merged"); got != "merge" {
		t.Errorf("merge prefix: %q", got)
	}
	if got := tag("sentry.issue.opened"); got != "catchall" {
		t.Errorf("catch-all: %q", got)
	}
}

func TestPolicyRouterNoMatch(t *testing.T) {
	r := PolicyRouter{{Prefix: "board.", Policy: routeStub{"board"}}}
	if acts := r.Decide(context.Background(), loop.Context{Event: loop.Event{Type: "x"}}); acts != nil {
		t.Fatalf("no matching route should be nil: %#v", acts)
	}
}
