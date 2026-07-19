package policy

import (
	"context"
	"fmt"

	"github.com/jwarykowski/drover/loop"
	"github.com/jwarykowski/drover/registry"
)

// Ingress turns an upstream signal into a held agentic task, for ALL sources.
// It matches the event (type [+ repo]) against the trusted registry and parks
// one task per matching action, carrying that action's stable id. The task sits
// at Hold until a human releases it — the review gate that makes running an
// agent on untrusted event text safe.
//
// It is data-driven: adding "park with action X" for a new source is a registry
// row, not a new policy. A source needing bespoke logic gets its own Go policy
// routed above the catch-all (see PolicyRouter).
type Ingress struct {
	Registry *registry.Registry
	Hold     string // park status; defaults to "hold"
}

func (p Ingress) hold() string {
	if p.Hold == "" {
		return "hold"
	}
	return p.Hold
}

func (p Ingress) Decide(_ context.Context, c loop.Context) []loop.Action {
	sig, ok := c.Event.Data.(loop.Signal)
	if !ok || p.Registry == nil {
		return nil
	}
	var acts []loop.Action
	for _, a := range p.Registry.Match(c.Event.Type, sig.Repo) {
		acts = append(acts, loop.AddTask{Spec: loop.Spec{
			Text:    fmt.Sprintf("%s: %s", sig.Repo, sig.Title),
			Agentic: true,
			Status:  p.hold(),
			Action:  a.ID, // reference only; the prompt lives in the registry
			Link:    sig.URL,
			Note:    sig.Title,
		}})
	}
	return acts
}
