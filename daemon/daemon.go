// Package daemon is drover's composition root: it builds the sources, policy
// and executor from config and runs the loop.
//
// It lives outside the TUI so a headless `drover watch` — and every source shim
// spawned from this same binary — links none of the terminal machinery.
package daemon

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jwarykowski/drover/config"
	dctx "github.com/jwarykowski/drover/context"
	"github.com/jwarykowski/drover/exec"
	"github.com/jwarykowski/drover/loop"
	"github.com/jwarykowski/drover/policy"
	"github.com/jwarykowski/drover/source"
	"github.com/jwarykowski/drover/store"
)

// Config is everything Run needs. The CLI fills it from flags; the dashboard
// fills it from its own state.
type Config struct {
	ConfigPath string        // drover.toml
	SeenPath   string        // persisted dedup set; empty = in-memory
	LogDir     string        // per-job agent stream logs; empty = store.DefaultLogDir()
	Agents     int           // agent worker pool size
	Timeout    time.Duration // per-run deadline; 0 = 20m

	// Store is the shared task datastore. The dashboard owns one instance and
	// passes it here so the daemon and the UI mutate the same tasks; nil opens
	// the default FileStore (the `drover watch` path, a single writer).
	Store loop.Store
}

// Run wires everything from config and drives the loop until ctx is cancelled.
// prov receives one JSON record per agent run; logf receives operational log
// lines. Both are injected so callers choose the sink — the CLI uses
// stdout/stderr, the dashboard a ring buffer.
func Run(ctx context.Context, cfg Config, prov io.Writer, logf func(string, ...any)) error {
	cf, err := config.Load(cfg.ConfigPath)
	if err != nil {
		return err
	}

	var seen source.Seen
	if cfg.SeenPath != "" {
		fseen, err := source.OpenFileSeen(cfg.SeenPath)
		if err != nil {
			return err
		}
		seen = fseen
	} else {
		seen = source.NewMemSeen()
	}

	tasks := cfg.Store
	if tasks == nil {
		fs, err := store.OpenFileStore(store.DefaultTasksPath())
		if err != nil {
			return err
		}
		tasks = fs
	}
	redrive, ok := tasks.(loop.Source)
	if !ok {
		return fmt.Errorf("task store %T cannot re-drive itself", tasks)
	}

	srcs, names := build(cf, seen, logf)
	if len(srcs) == 0 {
		logf("no usable [[source]] in %s — drover will only react to its own tasks", cfg.ConfigPath)
	}
	// The re-drive is deliberately NOT deduped: it replays the same task id every
	// tick on purpose, and Seen would swallow every replay after the first.
	srcs = append(srcs, redrive)

	logDir := cfg.LogDir
	if logDir == "" {
		logDir = store.DefaultLogDir()
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 20 * time.Minute
	}
	agent := &exec.AgentExecutor{
		Config: cf, Store: tasks, Provenance: prov, LogDir: logDir,
		Timeout: timeout, Concurrency: cfg.Agents, Logf: logf,
	}
	agent.Start(ctx)

	l := loop.Loop{
		Assembler: dctx.WorkingContext{Store: tasks},
		Policy:    policy.Policy{Config: cf},
		Executor:  exec.RouterExecutor{Store: exec.StoreExecutor{Store: tasks}, Agent: agent},
	}

	logf("watching %d source(s) [%s]; %d action(s), %d agent(s)",
		len(names), join(names), len(cf.Actions()), cfg.Agents)

	// The config is reloaded per event so `drover action add|edit|rm` take effect
	// without a restart; cf is shared with the agent workers under its own lock.
	// Source rows are NOT re-read here — a source is a running process, so adding
	// one needs a restart.
	for e := range source.Merge(srcs...).Events(ctx) {
		if err := cf.Reload(cfg.ConfigPath); err != nil {
			logf("config reload: %v", err)
		}
		if err := l.Run(ctx, e); err != nil && ctx.Err() == nil {
			logf("processing %s: %v", e.Type, err)
		}
	}
	logf("watch stopped")
	return nil
}

// build turns the [[source]] rows into running sources. Exactly one transport
// per row; a row naming neither (or both) is skipped with a log rather than
// failing the daemon, so one bad row can't stop every other source.
func build(cf *config.Config, seen source.Seen, logf func(string, ...any)) ([]loop.Source, []string) {
	var srcs []loop.Source
	var names []string
	for _, s := range cf.Sources() {
		var src loop.Source
		switch {
		case len(s.Cmd) > 0 && s.HTTP != "":
			logf("source %q sets both cmd and http; skipping", s.Name)
			continue
		case len(s.Cmd) > 0:
			src = source.ExecSource{Name: s.Name, Cmd: s.Cmd, Logf: logf}
		case s.HTTP != "":
			src = source.HTTPSource{Name: s.Name, Addr: s.HTTP, Logf: logf}
		default:
			logf("source %q sets neither cmd nor http; skipping", s.Name)
			continue
		}
		srcs = append(srcs, source.Dedup{Src: src, Seen: seen, Logf: logf})
		names = append(names, s.Name)
	}
	return srcs, names
}

func join(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
