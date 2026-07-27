package exec

import (
	"context"
	"io"
	"os"
	"path/filepath"
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

func TestResolveTargetEmptyBoardDirRunsInDefaultCwd(t *testing.T) {
	// A board with no working dir is legal: the action may not touch a board.
	// It must resolve to an empty cwd (drover's own dir), not error.
	x := &AgentExecutor{BoardDir: func(context.Context, string) (string, error) { return "", nil }}
	got, err := x.resolveTarget(context.Background(), registry.Action{ID: "a1", TargetBoard: "b1"})
	if err != nil {
		t.Fatalf("empty board dir must not error: %v", err)
	}
	if got != "" {
		t.Fatalf("empty board dir must resolve to empty cwd, got %q", got)
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
		run: func(_ context.Context, _ string, argv []string, _ time.Duration, _ io.Writer) ([]byte, error) {
			gotArgv = argv
			return []byte("working...\n{\"status\":\"done\",\"summary\":\"bumped\",\"followups\":[\"regen docs\"]}\n"), nil
		},
	}
	if err := x.Apply(context.Background(), []loop.Action{loop.RunAgent{
		ActionID: "a1", TaskID: "t1", Args: map[string]string{"title": "bump", "url": "u"},
	}}); err != nil {
		t.Fatal(err)
	}

	// claude -p <prompt> --permission-mode acceptEdits --output-format stream-json --verbose
	if len(gotArgv) != 8 || gotArgv[0] != "claude" || gotArgv[1] != "-p" || gotArgv[3] != "--permission-mode" || gotArgv[4] != "acceptEdits" {
		t.Fatalf("argv wrong: %v", gotArgv)
	}
	if gotArgv[5] != "--output-format" || gotArgv[6] != "stream-json" || gotArgv[7] != "--verbose" {
		t.Fatalf("stream-json flags missing: %v", gotArgv)
	}

	// A done verdict archives the task off the live board (leaving the followup).
	items, _ := st.List(context.Background(), loop.Filter{IncludeDone: true})
	var followup bool
	for _, it := range items {
		if it.ID == "t1" {
			t.Fatal("done task should be archived off the live board")
		}
		if it.Text == "regen docs" {
			followup = true
		}
	}
	if !followup {
		t.Fatal("followup not added as a todo")
	}
	arc := st.Archived()
	if len(arc) != 1 || arc[0].ID != "t1" {
		t.Fatalf("task not archived: %+v", arc)
	}
	if !arc[0].Done {
		t.Fatal("archived task should be stamped done")
	}
	if arc[0].Note != "bumped" {
		t.Fatalf("summary not noted: %q", arc[0].Note)
	}
}

func TestParseVerdictStreamJSON(t *testing.T) {
	// A realistic stream-json tail: system/assistant/tool events, then the final
	// result event whose `result` text ends with the verdict line.
	out := strings.Join([]string{
		`{"type":"system","subtype":"init","tools":["Bash"]}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}`,
		`{"type":"result","subtype":"success","result":"did the thing\n{\"status\":\"done\",\"summary\":\"shipped\",\"followups\":[\"docs\"]}"}`,
	}, "\n")
	v := parseVerdict([]byte(out))
	if v.Status != "done" || v.Summary != "shipped" || len(v.Followups) != 1 {
		t.Fatalf("stream-json verdict not parsed: %#v", v)
	}
}

func TestParseVerdictPlainTextFallback(t *testing.T) {
	v := parseVerdict([]byte("chatter\n{\"status\":\"blocked\",\"summary\":\"nope\"}\n"))
	if v.Status != "blocked" || v.Summary != "nope" {
		t.Fatalf("plain-text verdict not parsed: %#v", v)
	}
}

func TestAgentExecutorWritesJobLog(t *testing.T) {
	reg := &registry.Registry{Actions: []registry.Action{{ID: "a1", Target: "/tmp"}}}
	st := &store.FakeStore{}
	st.Seed(loop.Item{ID: "t1", Status: "running", Agentic: true, Action: "a1"})
	dir := t.TempDir()

	x := AgentExecutor{Registry: reg, Store: st, LogDir: dir,
		run: func(_ context.Context, _ string, _ []string, _ time.Duration, logW io.Writer) ([]byte, error) {
			if logW == nil {
				t.Fatal("expected a per-job log writer")
			}
			_, _ = io.WriteString(logW, `{"type":"assistant"}`+"\n")
			return []byte(`{"status":"done","summary":"ok"}`), nil
		},
	}
	if err := x.Apply(context.Background(), []loop.Action{loop.RunAgent{ActionID: "a1", TaskID: "t1"}}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "t1.jsonl"))
	if err != nil {
		t.Fatalf("job log not written: %v", err)
	}
	if !strings.Contains(string(b), `"type":"assistant"`) {
		t.Fatalf("job log missing stream content: %q", b)
	}
}

func TestAgentExecutorDetachedRunSkipsJobLog(t *testing.T) {
	reg := &registry.Registry{Actions: []registry.Action{{ID: "a1", Target: "/tmp"}}}
	st := &store.FakeStore{}
	dir := t.TempDir()
	x := AgentExecutor{Registry: reg, Store: st, LogDir: dir,
		run: func(_ context.Context, _ string, _ []string, _ time.Duration, logW io.Writer) ([]byte, error) {
			if logW != nil {
				t.Fatal("detached run (empty task id) must not get a job log")
			}
			return []byte(`{"status":"done","summary":"ok"}`), nil
		},
	}
	if err := x.Apply(context.Background(), []loop.Action{loop.RunAgent{ActionID: "a1", TaskID: ""}}); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("detached run wrote a log file: %v", entries)
	}
}

func TestAgentExecutorMalformedVerdictLeavesRunning(t *testing.T) {
	reg := &registry.Registry{Actions: []registry.Action{{ID: "a1", Target: "/tmp"}}}
	st := &store.FakeStore{}
	st.Seed(loop.Item{ID: "t1", Status: "running", Agentic: true, Action: "a1"})

	x := AgentExecutor{Registry: reg, Store: st,
		run: func(_ context.Context, _ string, _ []string, _ time.Duration, _ io.Writer) ([]byte, error) {
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

func TestAgentExecutorPoolDoesNotBlockSensing(t *testing.T) {
	reg := &registry.Registry{Actions: []registry.Action{{ID: "a1", Target: "/tmp"}}}
	st := &store.FakeStore{}
	st.Seed(loop.Item{ID: "t1", Status: "running", Agentic: true, Action: "a1"})
	st.Seed(loop.Item{ID: "t2", Status: "running", Agentic: true, Action: "a1"})

	release := make(chan struct{})
	x := &AgentExecutor{Registry: reg, Store: st, Concurrency: 2,
		run: func(context.Context, string, []string, time.Duration, io.Writer) ([]byte, error) {
			<-release // hold the run open so a synchronous Apply would block
			return []byte(`{"status":"done","summary":"ok"}`), nil
		},
	}
	x.Start(context.Background())

	returned := make(chan struct{})
	go func() {
		_ = x.Apply(context.Background(), []loop.Action{loop.RunAgent{ActionID: "a1", TaskID: "t1"}})
		_ = x.Apply(context.Background(), []loop.Action{loop.RunAgent{ActionID: "a1", TaskID: "t2"}})
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("Apply blocked on a running agent — sensing would stall")
	}

	close(release) // let both runs finish; both tasks must reconcile (done → archived)
	deadline := time.After(2 * time.Second)
	for {
		done := 0
		for _, it := range st.Archived() {
			if it.Done {
				done++
			}
		}
		if done == 2 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("both tasks not reconciled; archived-done=%d", done)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestAgentExecutorDetachedRunSkipsReconcile(t *testing.T) {
	reg := &registry.Registry{Actions: []registry.Action{{ID: "a1", Target: "/tmp"}}}
	st := &store.FakeStore{}
	ran := false
	x := AgentExecutor{Registry: reg, Store: st,
		run: func(context.Context, string, []string, time.Duration, io.Writer) ([]byte, error) {
			ran = true
			return []byte(`{"status":"done","summary":"cleaned","followups":["x"]}`), nil
		},
	}
	// Empty TaskID (terminal board event) → run, but no board write-back.
	if err := x.Apply(context.Background(), []loop.Action{loop.RunAgent{ActionID: "a1", TaskID: ""}}); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("detached action should still run the agent")
	}
	items, _ := st.List(context.Background(), loop.Filter{IncludeDone: true})
	if len(items) != 0 {
		t.Fatalf("detached run must not touch the board (no reconcile, no followups): %#v", items)
	}
}

func TestAgentExecutorUnknownActionNotesLeavesRunning(t *testing.T) {
	st := &store.FakeStore{}
	st.Seed(loop.Item{ID: "t1", Status: "running", Agentic: true, Action: "ghost"})
	ran := false
	x := AgentExecutor{Registry: &registry.Registry{}, Store: st,
		run: func(context.Context, string, []string, time.Duration, io.Writer) ([]byte, error) {
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
