package exec

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jwarykowski/drover/loop"
)

// captureRunner records what would have executed instead of spawning a process.
type captureRunner struct {
	calls [][]string
	cwds  []string
}

func (c *captureRunner) run(_ context.Context, cwd string, argv []string, _ time.Duration) error {
	c.calls = append(c.calls, argv)
	c.cwds = append(c.cwds, cwd)
	return nil
}

func allow() map[string]ActionSpec {
	return map[string]ActionSpec{
		"fix-ci": {Cmd: []string{"claude", "-p", "{{task}}"}, Cwd: "/repos/{{repo}}"},
		"gated":  {Cmd: []string{"deploy"}, Confirm: true},
	}
}

func TestRunnerRendersAndFires(t *testing.T) {
	cap := &captureRunner{}
	x := RunnerExecutor{Allow: allow(), run: cap.run}
	err := x.Apply(context.Background(), []loop.Action{loop.RunAction{
		Name: "fix-ci", Args: map[string]string{"task": "fix the build", "repo": "acme/api"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cap.calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(cap.calls))
	}
	want := []string{"claude", "-p", "fix the build"}
	for i, w := range want {
		if cap.calls[0][i] != w {
			t.Errorf("argv[%d] = %q, want %q", i, cap.calls[0][i], w)
		}
	}
	if cap.cwds[0] != "/repos/acme/api" {
		t.Errorf("cwd = %q", cap.cwds[0])
	}
}

func TestRunnerRejectsUnknownAction(t *testing.T) {
	cap := &captureRunner{}
	x := RunnerExecutor{Allow: allow(), run: cap.run}
	err := x.Apply(context.Background(), []loop.Action{loop.RunAction{Name: "rm-rf"}})
	if err == nil {
		t.Fatal("want error for un-allowlisted action, got nil")
	}
	if len(cap.calls) != 0 {
		t.Errorf("un-allowlisted action executed: %v", cap.calls)
	}
}

func TestRunnerRejectsInjectedNewline(t *testing.T) {
	cap := &captureRunner{}
	x := RunnerExecutor{Allow: allow(), run: cap.run}
	// A crafted arg value must not smuggle extra lines into the command.
	err := x.Apply(context.Background(), []loop.Action{loop.RunAction{
		Name: "fix-ci", Args: map[string]string{"task": "ok\nrm -rf /", "repo": "x"},
	}})
	if err == nil {
		t.Fatal("want error for newline in arg, got nil")
	}
	if len(cap.calls) != 0 {
		t.Errorf("command ran with injected arg: %v", cap.calls)
	}
}

func TestRunnerMissingPlaceholderArg(t *testing.T) {
	cap := &captureRunner{}
	x := RunnerExecutor{Allow: allow(), run: cap.run}
	err := x.Apply(context.Background(), []loop.Action{loop.RunAction{
		Name: "fix-ci", Args: map[string]string{"repo": "x"}, // no "task"
	}})
	if err == nil {
		t.Fatal("want error for missing placeholder arg, got nil")
	}
}

func TestRunnerConfirmGate(t *testing.T) {
	cap := &captureRunner{}
	// Confirm nil => confirm-required action is skipped (fail closed).
	x := RunnerExecutor{Allow: allow(), run: cap.run}
	if err := x.Apply(context.Background(), []loop.Action{loop.RunAction{Name: "gated"}}); err != nil {
		t.Fatal(err)
	}
	if len(cap.calls) != 0 {
		t.Errorf("gated action ran without confirmation: %v", cap.calls)
	}

	// Confirm returning true fires it.
	cap2 := &captureRunner{}
	x2 := RunnerExecutor{Allow: allow(), run: cap2.run, Confirm: func(loop.RunAction, ActionSpec) bool { return true }}
	if err := x2.Apply(context.Background(), []loop.Action{loop.RunAction{Name: "gated"}}); err != nil {
		t.Fatal(err)
	}
	if len(cap2.calls) != 1 {
		t.Errorf("confirmed action did not fire")
	}
}

func TestRunnerProvenance(t *testing.T) {
	cap := &captureRunner{}
	var buf strings.Builder
	x := RunnerExecutor{Allow: allow(), run: cap.run, Provenance: &buf}
	_ = x.Apply(context.Background(), []loop.Action{loop.RunAction{
		Name: "fix-ci", Args: map[string]string{"task": "t", "repo": "r"}, Reason: "ci.failed",
	}})
	line := buf.String()
	if !strings.Contains(line, `"action":"fix-ci"`) || !strings.Contains(line, `"outcome":"fired"`) || !strings.Contains(line, `"reason":"ci.failed"`) {
		t.Errorf("provenance record missing fields: %s", line)
	}
}
