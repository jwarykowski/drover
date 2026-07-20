package policy

import (
	"context"
	"strings"

	"github.com/jwarykowski/drover/loop"
)

// Route binds a policy to an Event.Type prefix. An empty Prefix is a catch-all.
type Route struct {
	Prefix string
	Policy loop.Policy
}

// PolicyRouter dispatches each event to the first route whose prefix matches
// Event.Type, mirroring exec.RouterExecutor on the sense side. Ordered: put
// specific prefixes (a bespoke per-source policy) before the "" catch-all
// (the generic table-driven Ingress). Adding a source is one route, or none if
// the generic Ingress + a registry row already cover it.
type PolicyRouter []Route

func (r PolicyRouter) Decide(ctx context.Context, c loop.Context) []loop.Action {
	for _, rt := range r {
		if strings.HasPrefix(c.Event.Type, rt.Prefix) {
			return rt.Policy.Decide(ctx, c)
		}
	}
	return nil
}

// Chain runs several policies for one event and concatenates their actions, so
// two policies can share a router prefix (PolicyRouter is first-match and would
// otherwise pick only one). Members are expected to be mutually exclusive on a
// given event — Dispatcher fires on agentic items, BoardTrigger on human-authored
// ones — so the concatenation never double-acts on the same item.
type Chain []loop.Policy

func (c Chain) Decide(ctx context.Context, cx loop.Context) []loop.Action {
	var out []loop.Action
	for _, p := range c {
		out = append(out, p.Decide(ctx, cx)...)
	}
	return out
}
