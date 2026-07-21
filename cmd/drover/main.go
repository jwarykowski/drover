// Command drover runs the loop around a shepherd board. Subcommands: watch
// (the closed loop), action (author the registry), run (fire an allowlisted
// action), doctor (prove the boundary).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	osexec "os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	dctx "github.com/jwarykowski/drover/context"
	"github.com/jwarykowski/drover/exec"
	"github.com/jwarykowski/drover/loop"
	"github.com/jwarykowski/drover/policy"
	"github.com/jwarykowski/drover/registry"
	"github.com/jwarykowski/drover/source"
	"github.com/jwarykowski/drover/store"
	"github.com/jwarykowski/drover/tui"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "doctor":
		err = doctor(ctx, os.Args[2:])
	case "run":
		err = runAction(ctx, os.Args[2:])
	case "watch":
		err = watch(ctx, os.Args[2:])
	case "action":
		err = actionCmd(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("drover", version())
	case "help", "--help", "-h":
		fmt.Println(usageText)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "drover:", err)
		os.Exit(1)
	}
}

func usage() { fmt.Fprintln(os.Stderr, usageText) }

// Version is drover's version, the single source of truth. Bump on release;
// there's no separate manifest to drift against (unlike shepherd's plugin file).
const Version = "0.1.0"

func version() string { return Version }

const usageText = `drover — the sense→assemble→act loop around a shepherd board

Usage:
  drover <command> [flags]

Commands:
  watch      sense GitHub + the board and drive the loop
  action     author the trusted action registry (opens a TUI)
  run        fire an allowlisted command directly
  doctor     prove the shepherd boundary (read the board, add a probe)
  version    print the version and exit
  help       print this help

watch:  (needs no flags — repos are derived from the registry)
  --repo owner/name       repo to watch (optional; else every repo named by a github.* action)
  --project <board>       shepherd board to park tasks on
  --source forward|poll   GitHub sense mode (default forward)
  --agents <n>            agent runs allowed in parallel (default 1)
  --seen <file>           persist handled event ids across restarts
  --provenance <file>     append a JSON record per agent run
  --registry <path>       action registry (default ~/.config/drover/actions.toml)

action:
  (bare)                  interactive TUI: create/view/edit/delete actions
  add|list|edit|rm        scriptable registry management

run <name>:
  --arg key=value         substitute into the allowlisted command (repeatable)
  --yes                   skip the confirm prompt
  --config <path>         allowlist file (default config/config.toml)

Runtime needs shepherd, gh and claude on PATH.`

// watch runs the closed loop over all configured sources: sense GitHub (+ the
// board), match each event against the trusted registry to park a held agentic
// task, and when a human releases one fire its registered agent action and
// reconcile the task from the agent's verdict. The GitHub sense is push (`gh
// webhook forward`) by default, poll as a fallback; both dedup on event id.
func watch(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	repo := fs.String("repo", "", "GitHub repo to watch, owner/name (optional; else derived from the registry)")
	base := fs.String("base", "master", "branch whose merges are sensed (poll mode)")
	project := fs.String("project", "", "shepherd board to park tasks on")
	sourceMode := fs.String("source", "forward", "GitHub sense: forward (gh webhook forward) | poll")
	addr := fs.String("addr", "127.0.0.1:9099", "local bind for the webhook receiver (forward mode)")
	interval := fs.Duration("interval", time.Minute, "GitHub poll interval (poll mode)")
	seenPath := fs.String("seen", "", "file recording handled event ids (survives restarts)")
	regPath := fs.String("registry", registry.DefaultPath(), "path to the action registry")
	provPath := fs.String("provenance", "", "append a JSON provenance record per agent run to this file")
	agents := fs.Int("agents", 1, "number of agent runs to allow in parallel")
	fs.Parse(argv)

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger := log.New(os.Stderr, "drover: ", 0)
	reg, err := registry.Load(*regPath)
	if err != nil {
		return err
	}

	var prov *os.File
	if *provPath != "" {
		prov, err = os.OpenFile(*provPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer prov.Close()
	}

	var seen source.Seen
	if *seenPath != "" {
		fseen, err := source.OpenFileSeen(*seenPath)
		if err != nil {
			return err
		}
		seen = fseen
	} else {
		seen = source.NewMemSeen()
	}

	// GitHub sensing is registry-driven: each github.* action naming a repo
	// contributes a watch carrying that action's base/source/interval (empty
	// fields fall back to the flag defaults), so `drover watch` needs no flags.
	// An explicit --repo overrides with the flags.
	var watches []repoWatch
	if *repo != "" {
		watches = []repoWatch{{repo: *repo, base: *base, source: *sourceMode, interval: *interval}}
	} else {
		watches = githubWatches(reg, *base, *sourceMode, *interval, logger.Printf)
		if bare := agnosticGithubActions(reg); len(bare) > 0 {
			logger.Printf("%d github action(s) have no repo filter, so can't be auto-watched — add repo: to them or pass --repo (%s)", len(bare), strings.Join(bare, ", "))
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
			gh := source.GitHubSource{Repo: w.repo, Base: w.base, Interval: w.interval, Logf: logger.Printf}
			if firstRun {
				if ids, err := gh.SeedIDs(ctx); err == nil {
					for _, id := range ids {
						_ = seen.Add(id)
					}
					logger.Printf("seeded %d merged PR(s) at head for %s; not firing history", len(ids), w.repo)
				} else {
					logger.Printf("seed %s: %v", w.repo, err)
				}
			}
			ghSrcs = append(ghSrcs, source.Dedup{Src: gh, Seen: seen, Logf: logger.Printf})
		} else { // forward
			wh := source.WebhookSource{Repo: w.repo, Addr: addrFor(*addr, i), Forward: true, Logf: logger.Printf}
			ghSrcs = append(ghSrcs, source.Dedup{Src: wh, Seen: seen, Logf: logger.Printf})
		}
	}

	// One locked store shared by the assembler, the store executor and the agent
	// workers so concurrent shepherd calls (file-locked) never overlap.
	st := &store.Locking{Store: store.ShepherdStore{Project: *project}}
	ae := &exec.AgentExecutor{Registry: reg, Store: st, Provenance: prov, Timeout: 20 * time.Minute, Concurrency: *agents, Logf: logger.Printf}
	ae.Start(ctx)

	// The board watch is always on; the GitHub sources (0..N) fan in beside it.
	src := source.Merge(append(ghSrcs, source.WatchSource{Project: *project, Logf: logger.Printf})...)
	l := loop.Loop{
		Assembler: dctx.WorkingContext{Store: st},
		Policy: policy.PolicyRouter{
			{Prefix: "board.", Policy: policy.Chain{
				policy.Dispatcher{},                // agentic tasks, gated hold→go
				policy.BoardTrigger{Registry: reg}, // human-authored items, by type
			}},
			{Prefix: "", Policy: policy.Ingress{Registry: reg}},
		},
		Executor: exec.RouterExecutor{
			Store: exec.StoreExecutor{Store: st},
			Agent: ae,
		},
	}

	watched := "(board only)"
	if len(watches) > 0 {
		names := make([]string, len(watches))
		for i, w := range watches {
			names[i] = w.repo
		}
		watched = strings.Join(names, ", ")
	}
	logger.Printf("watching board %q + repos [%s] via %s; %d action(s) registered, %d agent(s)", boardName(*project), watched, *sourceMode, len(reg.Actions), *agents)
	for e := range src.Events(ctx) {
		// Reload the registry each event so `drover action add|edit|rm` take
		// effect without restarting the daemon. reg is shared (guarded by its own
		// lock) with the concurrent agent workers.
		// unconditional reload of a small TOML; gate on mtime if it grows hot.
		if err := reg.Reload(*regPath); err != nil {
			logger.Printf("registry reload: %v", err)
		}
		if err := l.Run(ctx, e); err != nil && ctx.Err() == nil {
			logger.Printf("processing %s: %v", e.Type, err)
		}
	}
	logger.Printf("watch stopped")
	return nil
}

// actionCmd is the CRUD UI over the trusted registry: `drover action
// add|list|edit|rm`. This is the only writer of the registry the board
// references, so it is where events bind to what an agent does.
func actionCmd(argv []string) error {
	// Bare `drover action` (or with only flags) opens the interactive TUI; the
	// flag verbs stay for scripting.
	if len(argv) == 0 || strings.HasPrefix(argv[0], "-") {
		return actionTUI(argv)
	}
	sub, rest := argv[0], argv[1:]
	switch sub {
	case "add":
		return actionAdd(rest)
	case "list":
		return actionList(rest)
	case "edit":
		return actionEdit(rest)
	case "rm":
		return actionRm(rest)
	default:
		return fmt.Errorf("action: unknown subcommand %q", sub)
	}
}

// actionTUI opens the interactive registry manager. It needs a terminal; when
// stdin isn't one (piped/CI), it prints the scriptable usage instead of crashing.
func actionTUI(argv []string) error {
	fs := flag.NewFlagSet("action", flag.ExitOnError)
	regPath := fs.String("registry", registry.DefaultPath(), "path to the action registry")
	fs.Parse(argv)
	if !term.IsTerminal(os.Stdin.Fd()) {
		fmt.Fprintln(os.Stderr, "usage: drover action <add|list|edit|rm> [flags]  (bare `drover action` opens the TUI on a terminal)")
		return fmt.Errorf("action: not a terminal")
	}
	return tui.Run(*regPath)
}

func actionAdd(argv []string) error {
	fs := flag.NewFlagSet("action add", flag.ExitOnError)
	regPath := fs.String("registry", registry.DefaultPath(), "path to the action registry")
	name := fs.String("name", "", "friendly label (required)")
	on := fs.String("on", "", "event type to match (required)")
	repo := fs.String("repo", "", "optional source repo filter, owner/name")
	target := fs.String("target", "", "directory the agent runs in (required)")
	mode := fs.String("mode", "acceptEdits", "claude permission mode")
	doFile := fs.String("do-file", "", "read the prompt body from this file instead of $EDITOR")
	fs.Parse(argv)

	if *name == "" || *on == "" || *target == "" {
		return fmt.Errorf("action add: --name, --on and --target are required")
	}
	if !registry.ValidType(*on) {
		return fmt.Errorf("action add: unknown event type %q; known: %s", *on, strings.Join(registry.KnownEventTypes, ", "))
	}
	if !registry.ValidMode(*mode) {
		return fmt.Errorf("action add: invalid mode %q; valid: %s", *mode, strings.Join(registry.ValidModes, ", "))
	}
	do, err := promptBody(*doFile)
	if err != nil {
		return err
	}
	if strings.TrimSpace(do) == "" {
		return fmt.Errorf("action add: empty prompt body")
	}

	reg, err := registry.Load(*regPath)
	if err != nil {
		return err
	}
	a, _ := reg.Add(registry.Action{Name: *name, On: *on, Repo: *repo, Target: *target, Mode: *mode, Do: do})
	if err := reg.Save(); err != nil {
		return err
	}
	fmt.Printf("added action %s (%s)\n", a.ID, a.Name)
	return nil
}

func actionList(argv []string) error {
	fs := flag.NewFlagSet("action list", flag.ExitOnError)
	regPath := fs.String("registry", registry.DefaultPath(), "path to the action registry")
	fs.Parse(argv)
	reg, err := registry.Load(*regPath)
	if err != nil {
		return err
	}
	if len(reg.Actions) == 0 {
		fmt.Println("no actions registered")
		return nil
	}
	fmt.Println("id        name  on  repo")
	for _, a := range reg.Actions {
		fmt.Println(a.Summary())
	}
	return nil
}

func actionEdit(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("action edit: missing id")
	}
	id, rest := argv[0], argv[1:]
	fs := flag.NewFlagSet("action edit", flag.ExitOnError)
	regPath := fs.String("registry", registry.DefaultPath(), "path to the action registry")
	name := fs.String("name", "", "new label")
	repo := fs.String("repo", "", "new repo filter")
	target := fs.String("target", "", "new target directory")
	mode := fs.String("mode", "", "new permission mode")
	doFile := fs.String("do-file", "", "replace the prompt body from this file")
	editDo := fs.Bool("do", false, "replace the prompt body in $EDITOR")
	fs.Parse(rest)

	reg, err := registry.Load(*regPath)
	if err != nil {
		return err
	}
	a, ok := reg.ByID(id)
	if !ok {
		return fmt.Errorf("action edit: %w", registry.ErrNotFound)
	}
	if *name != "" {
		a.Name = *name
	}
	if *repo != "" {
		a.Repo = *repo
	}
	if *target != "" {
		a.Target = *target
	}
	if *mode != "" {
		if !registry.ValidMode(*mode) {
			return fmt.Errorf("action edit: invalid mode %q", *mode)
		}
		a.Mode = *mode
	}
	if *doFile != "" || *editDo {
		src := *doFile
		if *editDo {
			src = ""
		}
		do, err := promptBody(src)
		if err != nil {
			return err
		}
		a.Do = do
	}
	_ = reg.Remove(id)
	a.ID = id
	reg.Actions = append(reg.Actions, a)
	if err := reg.Save(); err != nil {
		return err
	}
	fmt.Printf("updated action %s\n", id)
	return nil
}

func actionRm(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("action rm: missing id")
	}
	id, rest := argv[0], argv[1:]
	fs := flag.NewFlagSet("action rm", flag.ExitOnError)
	regPath := fs.String("registry", registry.DefaultPath(), "path to the action registry")
	fs.Parse(rest)
	reg, err := registry.Load(*regPath)
	if err != nil {
		return err
	}
	if err := reg.Remove(id); err != nil {
		return err
	}
	if err := reg.Save(); err != nil {
		return err
	}
	fmt.Printf("removed action %s\n", id)
	return nil
}

// promptBody reads the `do` prompt from a file, or opens $EDITOR when file is "".
func promptBody(file string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		return string(b), err
	}
	ed := os.Getenv("EDITOR")
	if ed == "" {
		ed = "vi"
	}
	f, err := os.CreateTemp("", "drover-do-*.md")
	if err != nil {
		return "", err
	}
	name := f.Name()
	f.Close()
	defer os.Remove(name)
	cmd := osexec.Command(ed, name)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	b, err := os.ReadFile(name)
	return string(b), err
}

// argMap collects repeated --arg key=value flags.
type argMap map[string]string

func (m argMap) String() string { return "" }
func (m argMap) Set(kv string) error {
	k, v, ok := strings.Cut(kv, "=")
	if !ok {
		return fmt.Errorf("expected key=value, got %q", kv)
	}
	m[k] = v
	return nil
}

// runAction explicitly fires an allowlisted named action:
//
//	drover run fix-ci --arg repo=acme/api --arg task="fix the failing run" [--yes]
//
// The action name must be in the config allowlist; args fill the config command
// template. Nothing from a shepherd board is involved, so this can only run what
// trusted config permits.
func runAction(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("run: missing action name")
	}
	name, rest := argv[0], argv[1:]

	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", "config/config.toml", "path to drover's action allowlist")
	reason := fs.String("reason", "manual invocation", "why this action is firing (recorded)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt for confirm=true actions")
	provPath := fs.String("provenance", "", "append a JSON provenance record to this file")
	args := argMap{}
	fs.Var(args, "arg", "key=value substituted into the action command (repeatable)")
	fs.Parse(rest)

	allow, err := exec.LoadAllowlist(*configPath)
	if err != nil {
		return err
	}

	var prov *os.File
	if *provPath != "" {
		prov, err = os.OpenFile(*provPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer prov.Close()
	}

	x := exec.RunnerExecutor{
		Allow:      allow,
		Provenance: prov,
		Confirm: func(a loop.RunAction, s exec.ActionSpec) bool {
			if *yes {
				return true
			}
			return confirmTTY(a, s)
		},
	}
	return x.Apply(ctx, []loop.Action{loop.RunAction{Name: name, Args: args, Reason: *reason}})
}

// confirmTTY prompts on stderr and reads a y/N answer from stdin.
func confirmTTY(a loop.RunAction, s exec.ActionSpec) bool {
	fmt.Fprintf(os.Stderr, "run %q %v ? [y/N] ", a.Name, s.Cmd)
	var ans string
	fmt.Fscanln(os.Stdin, &ans)
	ans = strings.ToLower(strings.TrimSpace(ans))
	return ans == "y" || ans == "yes"
}

// doctor proves the boundary: read the real board, then add a marked throwaway.
func doctor(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	project := fs.String("project", "", "shepherd board to act on")
	fs.Parse(argv)

	st := store.ShepherdStore{Project: *project}
	board, err := st.List(ctx, loop.Filter{})
	if err != nil {
		return err
	}
	fmt.Printf("board has %d open item(s):\n", len(board))
	for _, it := range board {
		fmt.Printf("  [%s] %s\n", it.ID, it.Text)
	}

	// Leaves the probe item behind; remove it by hand with `shepherd rm`.
	added, err := st.Add(ctx, loop.Spec{Text: "drover doctor probe", Category: "drover", Priority: "L"})
	if err != nil {
		return err
	}
	fmt.Printf("added probe: [%s] %s\n", added.ID, added.Text)
	return nil
}

// repoWatch is one GitHub sense target derived from the registry (or --repo).
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
			base:     firstNonEmpty(a.Base, defBase),
			source:   firstNonEmpty(a.Source, defSource),
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

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
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

func boardName(p string) string {
	if p == "" {
		return "default"
	}
	return p
}
