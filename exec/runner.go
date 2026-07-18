package exec

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jwarykowski/drover/loop"
)

// RunnerExecutor applies RunAction actions against a trusted allowlist. It is
// the safe-actuation boundary: an action name is resolved to a command body
// that lives only in config, args are substituted as whole argv elements (no
// shell), and every run is recorded. Arbitrary board content can never cause
// execution — the name must be in the allowlist and the command never comes
// from an item field.
type RunnerExecutor struct {
	Allow map[string]ActionSpec
	// Confirm gates actions whose spec sets confirm=true. Nil means such actions
	// are always skipped (fail closed).
	Confirm func(loop.RunAction, ActionSpec) bool
	// Provenance, if set, receives one JSON record per action (fired or skipped).
	Provenance io.Writer
	// run executes a resolved command; injectable so tests don't spawn processes.
	// Nil uses the real os/exec runner.
	run func(ctx context.Context, cwd string, argv []string, timeout time.Duration) error
}

// record is the provenance line: what ran, why, and what happened.
type record struct {
	At      string            `json:"at"`
	Action  string            `json:"action"`
	Reason  string            `json:"reason,omitempty"`
	Args    map[string]string `json:"args,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Argv    []string          `json:"argv"`
	Outcome string            `json:"outcome"` // fired | skipped | error: ...
}

// Apply runs each RunAction. A non-RunAction, an unknown name, or a bad template
// is a hard error — the loop must not silently swallow an unactuated action.
func (x RunnerExecutor) Apply(ctx context.Context, actions []loop.Action) error {
	for _, a := range actions {
		run, ok := a.(loop.RunAction)
		if !ok {
			return fmt.Errorf("runner: unsupported action %T", a)
		}
		spec, ok := x.Allow[run.Name]
		if !ok {
			return fmt.Errorf("runner: action %q not in allowlist", run.Name)
		}
		argv, err := renderAll(spec.Cmd, run.Args)
		if err != nil {
			return fmt.Errorf("runner: action %q: %w", run.Name, err)
		}
		cwd, err := renderOne(spec.Cwd, run.Args)
		if err != nil {
			return fmt.Errorf("runner: action %q cwd: %w", run.Name, err)
		}

		if spec.Confirm && (x.Confirm == nil || !x.Confirm(run, spec)) {
			x.write(record{At: now(), Action: run.Name, Reason: run.Reason, Args: run.Args, Cwd: cwd, Argv: argv, Outcome: "skipped"})
			continue
		}

		d, err := parseTimeout(spec.Timeout)
		if err != nil {
			return fmt.Errorf("runner: action %q timeout: %w", run.Name, err)
		}
		runErr := x.runner()(ctx, cwd, argv, d)
		outcome := "fired"
		if runErr != nil {
			outcome = "error: " + runErr.Error()
		}
		x.write(record{At: now(), Action: run.Name, Reason: run.Reason, Args: run.Args, Cwd: cwd, Argv: argv, Outcome: outcome})
		if runErr != nil {
			return fmt.Errorf("runner: action %q: %w", run.Name, runErr)
		}
	}
	return nil
}

func (x RunnerExecutor) runner() func(context.Context, string, []string, time.Duration) error {
	if x.run != nil {
		return x.run
	}
	return execRun
}

func (x RunnerExecutor) write(r record) {
	if x.Provenance == nil {
		return
	}
	if b, err := json.Marshal(r); err == nil {
		_, _ = x.Provenance.Write(append(b, '\n'))
	}
}

// execRun runs argv with no shell, in cwd, under an optional timeout.
func execRun(ctx context.Context, cwd string, argv []string, timeout time.Duration) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr // action output is operator-facing
	return cmd.Run()
}

// renderAll fills {{key}} placeholders in each argv element from args.
func renderAll(tmpl []string, args map[string]string) ([]string, error) {
	out := make([]string, len(tmpl))
	for i, e := range tmpl {
		r, err := renderOne(e, args)
		if err != nil {
			return nil, err
		}
		out[i] = r
	}
	return out, nil
}

// renderOne substitutes {{key}} tokens with args[key]. An unknown placeholder or
// a value carrying a newline/NUL is rejected — fail closed so a crafted arg
// can't smuggle extra lines or truncate the command.
func renderOne(s string, args map[string]string) (string, error) {
	var b strings.Builder
	for {
		i := strings.Index(s, "{{")
		if i < 0 {
			b.WriteString(s)
			break
		}
		j := strings.Index(s[i:], "}}")
		if j < 0 {
			return "", fmt.Errorf("unterminated placeholder in %q", s)
		}
		key := strings.TrimSpace(s[i+2 : i+j])
		val, ok := args[key]
		if !ok {
			return "", fmt.Errorf("no arg for placeholder {{%s}}", key)
		}
		if strings.ContainsAny(val, "\n\x00") {
			return "", fmt.Errorf("arg %q contains a newline or NUL", key)
		}
		b.WriteString(s[:i])
		b.WriteString(val)
		s = s[i+j+2:]
	}
	return b.String(), nil
}

func parseTimeout(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

// now stamps provenance. Kept as a var so tests can pin it.
var now = func() string { return time.Now().UTC().Format(time.RFC3339) }
