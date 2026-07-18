// Package policy decides actions from an assembled context. RulesPolicy is the
// deterministic Phase 1 implementation; an LLM reasoner slots in behind the same
// loop.Policy interface later.
package policy

import (
	"context"
	"fmt"

	"github.com/jwarykowski/drover/loop"
)

// RulesPolicy is a pure function of Context to Actions — no I/O, table-testable.
type RulesPolicy struct{}

// Decide applies the Phase 1 rules. Unknown event kinds produce no action.
func (RulesPolicy) Decide(_ context.Context, c loop.Context) []loop.Action {
	switch c.Event.Kind {
	case "ci.failed":
		return []loop.Action{loop.AddTask{Spec: loop.Spec{
			Text:     fmt.Sprintf("CI failed: %v", c.Event.Payload["title"]),
			Category: "ci",
			Priority: "H",
			Link:     str(c.Event.Payload["link"]),
		}}}
	default:
		return nil
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
