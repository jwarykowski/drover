package policy

import (
	"context"
	"testing"

	"github.com/jwarykowski/drover/loop"
)

func TestMergeFlowAddsHeldAgenticTask(t *testing.T) {
	p := MergeFlowPolicy{Action: "run-skill"}
	acts := p.Decide(context.Background(), loop.Context{Event: loop.Event{
		Kind:    "github.merged",
		Payload: map[string]any{"repo": "acme/api", "title": "bump deps", "url": "https://github.com/acme/api/pull/7"},
	}})
	if len(acts) != 1 {
		t.Fatalf("want 1 action, got %d", len(acts))
	}
	add, ok := acts[0].(loop.AddTask)
	if !ok {
		t.Fatalf("want AddTask, got %T", acts[0])
	}
	if !add.Spec.Agentic || add.Spec.Status != "hold" {
		t.Fatalf("want agentic status:hold, got agentic=%v status:%s", add.Spec.Agentic, add.Spec.Status)
	}
	if add.Spec.Action != "run-skill" {
		t.Fatalf("want action run-skill stamped, got %q", add.Spec.Action)
	}
	if add.Spec.Link != "https://github.com/acme/api/pull/7" {
		t.Fatalf("link not carried: %q", add.Spec.Link)
	}
}

func TestMergeFlowReleaseFiresTaskActionAndCloses(t *testing.T) {
	p := MergeFlowPolicy{Action: "run-skill"}
	// The task names its own action; the policy default differs, so this proves
	// the release reads the action from the item, not the policy.
	item := loop.Item{ID: "abc", Agentic: true, Status: "go", Action: "custom-skill", Note: "bump deps", Link: "https://x/pr/7"}
	acts := p.Decide(context.Background(), loop.Context{Event: loop.Event{
		Kind:    "board.updated",
		Payload: map[string]any{"item": item},
	}})
	if len(acts) != 3 {
		t.Fatalf("want 3 actions (claim, run, close), got %d", len(acts))
	}
	if s, ok := acts[0].(loop.SetStatus); !ok || s.Status != "running" || s.ID != "abc" {
		t.Fatalf("action 0 not SetStatus running: %#v", acts[0])
	}
	run, ok := acts[1].(loop.RunAction)
	if !ok || run.Name != "custom-skill" {
		t.Fatalf("action 1 should fire the item's action custom-skill: %#v", acts[1])
	}
	if run.Args["title"] != "bump deps" || run.Args["url"] != "https://x/pr/7" {
		t.Fatalf("run args not from item: %#v", run.Args)
	}
	if s, ok := acts[2].(loop.SetStatus); !ok || s.Status != "done" {
		t.Fatalf("action 2 not SetStatus done: %#v", acts[2])
	}
}

func TestMergeFlowReleaseFallsBackToPolicyAction(t *testing.T) {
	p := MergeFlowPolicy{Action: "run-skill"}
	item := loop.Item{ID: "z", Agentic: true, Status: "go"} // no action on the task
	acts := p.Decide(context.Background(), loop.Context{Event: loop.Event{
		Kind: "board.updated", Payload: map[string]any{"item": item},
	}})
	if run, ok := acts[1].(loop.RunAction); !ok || run.Name != "run-skill" {
		t.Fatalf("empty item action should fall back to policy default run-skill: %#v", acts[1])
	}
}

func TestMergeFlowIgnoresNonAgenticAndUnready(t *testing.T) {
	p := MergeFlowPolicy{Action: "run-skill"}
	cases := []loop.Item{
		{ID: "1", Agentic: false, Status: "go"},  // not agentic
		{ID: "2", Agentic: true, Status: "hold"}, // not released
		{ID: "3", Agentic: true, Status: ""},     // no status
	}
	for _, it := range cases {
		acts := p.Decide(context.Background(), loop.Context{Event: loop.Event{
			Kind: "board.updated", Payload: map[string]any{"item": it},
		}})
		if acts != nil {
			t.Fatalf("item %s should not fire, got %#v", it.ID, acts)
		}
	}
}
