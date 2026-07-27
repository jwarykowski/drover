package tui

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	dctx "github.com/jwarykowski/drover/context"
	"github.com/jwarykowski/drover/exec"
	"github.com/jwarykowski/drover/loop"
	"github.com/jwarykowski/drover/policy"
	"github.com/jwarykowski/drover/registry"
	"github.com/jwarykowski/drover/source"
	"github.com/jwarykowski/drover/store"
)

// Config is everything RunDaemon needs to wire and run the watch loop. The CLI
// (`drover watch`) fills it from flags; the dashboard fills it from its state.
type Config struct {
	Repo     string // explicit repo override; empty = derive from the registry
	Base     string // poll-mode branch default
	Board    string // board whose dir defaults the agent cwd; empty = default
	Source   string // github sense default: forward | poll
	Addr     string // webhook receiver bind (forward mode)
	Interval time.Duration
	SeenPath string
	RegPath  string
	LogDir   string // per-job claude stream logs; empty = store.DefaultLogDir()
	Agents   int

	// Store is the shared agentic-task datastore. The dashboard owns one instance
	// and passes it here so the daemon and the UI mutate the same tasks; nil means
	// open the default FileStore (the `drover watch` CLI path, a single writer).
	Store loop.Store
}

// RunDaemon wires the sources, policy and executor around a shepherd board and
// runs the loop until ctx is cancelled. prov receives one JSON record per agent
// run; logf receives operational log lines. Both are injected so callers choose
// the sink — the CLI uses stdout/stderr, the dashboard a ring buffer.
func RunDaemon(ctx context.Context, cfg Config, prov io.Writer, logf func(string, ...any)) error {
	reg, err := registry.Load(cfg.RegPath)
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

	// GitHub sensing is registry-driven: each github.* action naming a repo
	// contributes a watch carrying that action's base/source/interval. An
	// explicit Repo overrides with the config defaults.
	var watches []repoWatch
	if cfg.Repo != "" {
		watches = []repoWatch{{repo: cfg.Repo, base: cfg.Base, source: cfg.Source, interval: cfg.Interval}}
	} else {
		watches = githubWatches(reg, cfg.Base, cfg.Source, cfg.Interval, logf)
		if bare := agnosticGithubActions(reg); len(bare) > 0 {
			logf("%d github action(s) have no repo filter, so can't be auto-watched — add repo: to them or pass --repo (%s)", len(bare), strings.Join(bare, ", "))
		}
	}

	// Cold-start seeding is one-time per FileSeen; capture emptiness once, before
	// any repo seeds and flips it non-empty.
	firstRun := false
	if fseen, ok := seen.(*source.FileSeen); ok && fseen.Empty() {
		firstRun = true
	}

	ghSrcs := make([]loop.Source, 0, len(watches))
	for i, w := range watches {
		if w.source == "poll" {
			gh := source.GitHubSource{Repo: w.repo, Base: w.base, Interval: w.interval, Logf: logf}
			if firstRun {
				if ids, err := gh.SeedIDs(ctx); err == nil {
					for _, id := range ids {
						_ = seen.Add(id)
					}
					logf("seeded %d merged PR(s) at head for %s; not firing history", len(ids), w.repo)
				} else {
					logf("seed %s: %v", w.repo, err)
				}
			}
			ghSrcs = append(ghSrcs, source.Dedup{Src: gh, Seen: seen, Logf: logf})
		} else { // forward
			wh := source.WebhookSource{Repo: w.repo, Addr: addrFor(cfg.Addr, i), Forward: true, Logf: logf}
			ghSrcs = append(ghSrcs, source.Dedup{Src: wh, Seen: seen, Logf: logf})
		}
	}

	// Two task worlds, each bound to its own store — no cross-store routing,
	// because the two policies are disjoint by task origin:
	//
	//   - drover's OWN agentic tasks (github/sentry sensed, gated hold→go) live in
	//     a FileStore, invisible to shepherd. Dispatcher fires them; the FileStore
	//     also re-drives itself as board.* events so a released hold→go dispatches.
	//   - HUMAN-authored shepherd board items that trigger an action live on the
	//     shepherd board. WatchSource streams them; BoardTrigger fires them and the
	//     agent's verdict reconciles straight back to the shepherd item.
	//
	// Binding each loop to one store is what makes reconcile write back to the
	// task's origin without any per-task routing.
	drv := cfg.Store
	if drv == nil {
		fs, err := store.OpenFileStore(store.DefaultTasksPath())
		if err != nil {
			return err
		}
		drv = fs
	}
	drvSrc, ok := drv.(loop.Source)
	if !ok {
		return fmt.Errorf("task store %T is not a source", drv)
	}
	// Shared shepherd handle: used only to READ — resolve a board's working dir
	// (agent cwd) and read the board slice a board change refers to. drover never
	// writes to shepherd; a board change parks a run in drv (the FileStore).
	shep := store.ShepherdStore{Board: cfg.Board}

	// One agent pool, one store (the FileStore): every run — github/sentry-sensed
	// or board-triggered — is tracked and reconciled in drv, so shepherd stays a
	// pure trigger source and the dashboard sees all runs in one place.
	logDir := cfg.LogDir
	if logDir == "" {
		logDir = store.DefaultLogDir()
	}
	drvAgent := &exec.AgentExecutor{Registry: reg, Store: drv, Provenance: prov, LogDir: logDir, Timeout: 20 * time.Minute, Concurrency: cfg.Agents, Logf: logf, BoardDir: shep.BoardDir}
	drvAgent.Start(ctx)

	drvLoop := loop.Loop{
		Assembler: dctx.WorkingContext{Store: drv},
		Policy: policy.PolicyRouter{
			{Prefix: "board.", Policy: policy.Dispatcher{}},     // agentic tasks, gated hold→go
			{Prefix: "", Policy: policy.Ingress{Registry: reg}}, // sense → park a held task
		},
		Executor: exec.RouterExecutor{Store: exec.StoreExecutor{Store: drv}, Agent: drvAgent},
	}
	// The board loop only ever parks a task in drv — the Dispatcher above (fed by
	// drv's re-drive) fires it — so it needs a store executor, not an agent.
	shepLoop := loop.Loop{
		Assembler: dctx.WorkingContext{Store: shep},
		Policy:    policy.BoardTrigger{Registry: reg}, // board change → parks a drover run
		Executor:  exec.StoreExecutor{Store: drv},
	}

	watched := "(board only)"
	if len(watches) > 0 {
		names := make([]string, len(watches))
		for i, w := range watches {
			names[i] = w.repo
		}
		watched = strings.Join(names, ", ")
	}
	logf("watching board %q + repos [%s]; %d action(s) registered, %d agent(s)", boardName(cfg.Board), watched, len(reg.Actions), cfg.Agents)

	// run drives one source's events through one loop until ctx is cancelled,
	// reloading the registry per event so `drover action add|edit|rm` take effect
	// live; reg is shared (guarded by its own lock) with the agent workers.
	run := func(src loop.Source, l loop.Loop) {
		for e := range src.Events(ctx) {
			if err := reg.Reload(cfg.RegPath); err != nil {
				logf("registry reload: %v", err)
			}
			if err := l.Run(ctx, e); err != nil && ctx.Err() == nil {
				logf("processing %s: %v", e.Type, err)
			}
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); run(source.Merge(append(ghSrcs, drvSrc)...), drvLoop) }()
	go func() {
		defer wg.Done()
		// No Interval: the board is a local file, so let shepherd's own fast
		// default (~1s) drive it. cfg.Interval is the GitHub poll cadence (~1m,
		// rate-limit driven) — wiring it here made board edits lag up to a minute.
		run(boardWatch(ctx, shep, cfg.Board, logf), shepLoop)
	}()
	wg.Wait()
	logf("watch stopped")
	return nil
}

// repoWatch is one GitHub sense target derived from the registry (or an explicit repo).
type repoWatch struct {
	repo     string
	base     string
	source   string
	interval time.Duration
}

// githubWatches builds one watch per distinct repo named by a github.* action,
// carrying that action's base/source/interval — the first action naming a repo
// wins, and empty fields fall back to the daemon defaults. This is what lets
// `drover watch` run with no flags: the registry defines what and how to sense.
func githubWatches(reg *registry.Registry, defBase, defSource string, defInterval time.Duration, logf func(string, ...any)) []repoWatch {
	var out []repoWatch
	seen := map[string]bool{}
	for _, a := range reg.Actions {
		if !strings.HasPrefix(a.On, "github.") || a.Repo == "" || seen[a.Repo] {
			continue
		}
		seen[a.Repo] = true
		w := repoWatch{
			repo:     a.Repo,
			base:     cmp.Or(a.Base, defBase),
			source:   cmp.Or(a.Source, defSource),
			interval: defInterval,
		}
		if a.Interval != "" {
			if d, err := time.ParseDuration(a.Interval); err == nil {
				w.interval = d
			} else if logf != nil {
				logf("action %s: bad interval %q, using default: %v", a.Name, a.Interval, err)
			}
		}
		out = append(out, w)
	}
	return out
}

// agnosticGithubActions names github.* actions with no repo filter. They match
// any repo, so there's no concrete repo to poll/forward — auto-watch skips them.
func agnosticGithubActions(reg *registry.Registry) []string {
	var out []string
	for _, a := range reg.Actions {
		if strings.HasPrefix(a.On, "github.") && a.Repo == "" {
			out = append(out, a.Name)
		}
	}
	return out
}

// addrFor gives each forwarded repo its own local port (base + i) so multiple
// `gh webhook forward` receivers don't collide on one bind. Falls back to base
// if it can't parse a host:port.
func addrFor(base string, i int) string {
	if i == 0 {
		return base
	}
	host, port, err := net.SplitHostPort(base)
	if err != nil {
		return base
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return base
	}
	return net.JoinHostPort(host, strconv.Itoa(p+i))
}

// boardWatch senses board changes as one loop.Source. A selected board watches
// just that board; the empty default watches ALL boards — one `shepherd watch`
// per board, fanned into a single stream, each pre-tagged with its board. A new
// board created mid-run is picked up on the next daemon (re)start.
func boardWatch(ctx context.Context, shep store.ShepherdStore, board string, logf func(string, ...any)) loop.Source {
	if board != "" {
		return source.WatchSource{Board: board, Logf: logf}
	}
	boards, err := shep.Boards(ctx)
	if err != nil || len(boards) == 0 {
		if err != nil {
			logf("watch all boards: %v; falling back to the default board", err)
		}
		return source.WatchSource{Logf: logf}
	}
	srcs := make([]loop.Source, 0, len(boards))
	for _, b := range boards {
		name := b.Name
		if name == "default" {
			name = "" // the default board is the empty --board arg
		}
		srcs = append(srcs, source.WatchSource{Board: name, Logf: logf})
	}
	return source.Merge(srcs...)
}

func boardName(p string) string {
	if p == "" {
		return "default"
	}
	return p
}
