package exec

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jwarykowski/drover/loop"
	"github.com/jwarykowski/drover/registry"
	"github.com/jwarykowski/drover/store"
)

func TestBuildAgentPromptNamesRepoFromURL(t *testing.T) {
	// Repo-agnostic action (no Repo filter): repo must come from the PR url.
	a := registry.Action{On: "github.pull_request.merged", Do: "x"}
	p := buildAgentPrompt(a, map[string]string{"title": "t", "url": "https://github.com/acme/api/pull/7"})
	if !strings.Contains(p, "repo:  acme/api") {
		t.Fatalf("repo not derived from url:\n%s", p)
	}
}

func TestAgentExecutorDoneReconciles(t *testing.T) {
	reg := &registry.Registry{Actions: []registry.Action{
		{ID: "a1", On: "github.pull_request.merged", Target: "/tmp", Mode: "acceptEdits", Do: "do the thing"},
	}}
	st := &store.FakeStore{}
	st.Seed(loop.Item{ID: "t1", Text: "task", Agentic: true, Status: "running", Action: "a1"})

	var gotArgv []string
	x := AgentExecutor{Registry: reg, Store: st,
		run: func(_ context.Context, _ string, argv []string, _ time.Duration) ([]byte, error) {
			gotArgv = argv
			return []byte("working...\n{\"status\":\"done\",\"summary\":\"bumped\",\"followups\":[\"regen docs\"]}\n"), nil
		},
	}
	if err := x.Apply(context.Background(), []loop.Action{loop.RunAgent{
		ActionID: "a1", TaskID: "t1", Args: map[string]string{"title": "bump", "url": "u"},
	}}); err != nil {
		t.Fatal(err)
	}

	// claude -p <prompt> --permission-mode acceptEdits
	if len(gotArgv) != 5 || gotArgv[0] != "claude" || gotArgv[1] != "-p" || gotArgv[3] != "--permission-mode" || gotArgv[4] != "acceptEdits" {
		t.Fatalf("argv wrong: %v", gotArgv)
	}

	items, _ := st.List(context.Background(), loop.Filter{IncludeDone: true})
	var t1 loop.Item
	var followup bool
	for _, it := range items {
		if it.ID == "t1" {
			t1 = it
		}
		if it.Text == "regen docs" {
			followup = true
		}
	}
	if !t1.Done {
		t.Fatal("done verdict should close the task")
	}
	if t1.Note != "bumped" {
		t.Fatalf("summary not noted: %q", t1.Note)
	}
	if !followup {
		t.Fatal("followup not added as a todo")
	}
}

func TestAgentExecutorMalformedVerdictLeavesRunning(t *testing.T) {
	reg := &registry.Registry{Actions: []registry.Action{{ID: "a1", Target: "/tmp"}}}
	st := &store.FakeStore{}
	st.Seed(loop.Item{ID: "t1", Status: "running", Agentic: true, Action: "a1"})

	x := AgentExecutor{Registry: reg, Store: st,
		run: func(_ context.Context, _ string, _ []string, _ time.Duration) ([]byte, error) {
			return []byte("no json here"), nil
		},
	}
	if err := x.Apply(context.Background(), []loop.Action{loop.RunAgent{ActionID: "a1", TaskID: "t1"}}); err != nil {
		t.Fatal(err)
	}
	items, _ := st.List(context.Background(), loop.Filter{IncludeDone: true})
	if items[0].Done {
		t.Fatal("a malformed verdict must NOT close the task")
	}
	if items[0].Note == "" {
		t.Fatal("failure should be noted for inspection")
	}
}

func TestAgentExecutorUnknownActionNotesLeavesRunning(t *testing.T) {
	st := &store.FakeStore{}
	st.Seed(loop.Item{ID: "t1", Status: "running", Agentic: true, Action: "ghost"})
	ran := false
	x := AgentExecutor{Registry: &registry.Registry{}, Store: st,
		run: func(context.Context, string, []string, time.Duration) ([]byte, error) {
			ran = true
			return nil, nil
		},
	}
	if err := x.Apply(context.Background(), []loop.Action{loop.RunAgent{ActionID: "ghost", TaskID: "t1"}}); err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("a missing action id must never fall through to execution")
	}
	items, _ := st.List(context.Background(), loop.Filter{IncludeDone: true})
	if items[0].Done {
		t.Fatal("a missing action must NOT close the task")
	}
	if items[0].Note == "" {
		t.Fatal("a missing action should leave a note explaining why the task never ran")
	}
}
