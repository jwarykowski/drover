// Command drover runs the loop around a shepherd board. Subcommands: doctor
// (prove the boundary), ingest (one signal, one task), daemon (react live).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	osexec "os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	dctx "github.com/jwarykowski/drover/context"
	"github.com/jwarykowski/drover/exec"
	"github.com/jwarykowski/drover/loop"
	"github.com/jwarykowski/drover/policy"
	"github.com/jwarykowski/drover/registry"
	"github.com/jwarykowski/drover/source"
	"github.com/jwarykowski/drover/store"
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
	case "ingest":
		err = ingest(ctx, os.Args[2:])
	case "daemon":
		err = daemon(ctx, os.Args[2:])
	case "run":
		err = runAction(ctx, os.Args[2:])
	case "watch":
		err = watch(ctx, os.Args[2:])
	case "action":
		err = actionCmd(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "drover:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: drover <doctor|ingest|daemon|run|watch|action> [flags]")
}

// watch runs the closed loop over all configured sources: sense GitHub (+ the
// board), match each event against the trusted registry to park a held agentic
// task, and when a human releases one fire its registered agent action and
// reconcile the task from the agent's verdict. The GitHub sense is push (`gh
// webhook forward`) by default, poll as a fallback; both dedup on event id.
func watch(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	repo := fs.String("repo", "", "GitHub repo to watch, owner/name (required)")
	base := fs.String("base", "master", "branch whose merges are sensed (poll mode)")
	project := fs.String("project", "", "shepherd board to park tasks on")
	sourceMode := fs.String("source", "forward", "GitHub sense: forward (gh webhook forward) | poll")
	addr := fs.String("addr", "127.0.0.1:9099", "local bind for the webhook receiver (forward mode)")
	interval := fs.Duration("interval", time.Minute, "GitHub poll interval (poll mode)")
	seenPath := fs.String("seen", "", "file recording handled event ids (survives restarts)")
	regPath := fs.String("registry", registry.DefaultPath(), "path to the action registry")
	provPath := fs.String("provenance", "", "append a JSON provenance record per agent run to this file")
	fs.Parse(argv)

	if *repo == "" {
		return fmt.Errorf("watch: --repo owner/name is required")
	}

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

	var ghSrc loop.Source
	switch *sourceMode {
	case "poll":
		gh := source.GitHubSource{Repo: *repo, Base: *base, Interval: *interval, Logf: logger.Printf}
		if fseen, ok := seen.(*source.FileSeen); ok && fseen.Empty() {
			if ids, err := gh.SeedIDs(ctx); err == nil {
				for _, id := range ids {
					_ = seen.Add(id)
				}
				logger.Printf("seeded %d merged PR(s) at head; not firing history", len(ids))
			} else {
				logger.Printf("seed: %v", err)
			}
		}
		ghSrc = source.Dedup{Src: gh, Seen: seen, Logf: logger.Printf}
	default: // forward
		wh := source.WebhookSource{Repo: *repo, Addr: *addr, Forward: true, Logf: logger.Printf}
		ghSrc = source.Dedup{Src: wh, Seen: seen, Logf: logger.Printf}
	}

	st := store.ShepherdStore{Project: *project}
	src := source.Merge(ghSrc, source.WatchSource{Project: *project, Logf: logger.Printf})
	l := loop.Loop{
		Assembler: dctx.WorkingContext{Store: st},
		Policy: policy.PolicyRouter{
			{Prefix: "board.", Policy: policy.Dispatcher{}},
			{Prefix: "", Policy: policy.Ingress{Registry: reg}},
		},
		Executor: exec.RouterExecutor{
			Store: exec.StoreExecutor{Store: st},
			Agent: exec.AgentExecutor{Registry: reg, Store: st, Provenance: prov, Timeout: 20 * time.Minute},
		},
	}

	logger.Printf("watching %s via %s; parking on board %q, %d action(s) registered", *repo, *sourceMode, boardName(*project), len(reg.Actions))
	for e := range src.Events(ctx) {
		// Reload the registry each event so `drover action add|edit|rm` take
		// effect without restarting the daemon. reg is shared by Ingress and
		// AgentExecutor; mutating it in place updates both. The loop is
		// single-goroutine, so no lock is needed.
		// ponytail: unconditional reload of a small TOML; gate on mtime if it grows hot.
		if fresh, err := registry.Load(*regPath); err == nil {
			reg.Actions = fresh.Actions
		} else {
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
	if len(argv) == 0 {
		return fmt.Errorf("action: missing subcommand (add|list|edit|rm)")
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

// buildLoop wires the seams over a store with the deterministic rules policy —
// used by the daemon, whose live board events are handled by rules.
func buildLoop(st loop.Store) loop.Loop {
	return loop.Loop{
		Assembler: dctx.WorkingContext{Store: st},
		Policy:    policy.RulesPolicy{},
		Executor:  exec.StoreExecutor{Store: st},
	}
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

// ingest turns one CLI-supplied signal into one loop run, under the chosen policy.
func ingest(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	kind := fs.String("kind", "ci.failed", "event kind")
	link := fs.String("link", "", "link to attach (e.g. CI run URL)")
	title := fs.String("title", "", "short description of what happened")
	project := fs.String("project", "", "shepherd board to act on")
	policyName := fs.String("policy", "rules", "decision policy: rules | llm | shadow")
	fs.Parse(argv)

	st := store.ShepherdStore{Project: *project}
	l := loop.Loop{
		Assembler: dctx.WorkingContext{Store: st},
		Policy:    buildPolicy(ctx, *policyName, st),
		Executor:  exec.StoreExecutor{Store: st},
	}
	src := source.GitSource{Event: loop.Event{
		Type:   *kind,
		Source: "git",
		Data:   loop.Generic{"title": *title, "link": *link},
		At:     time.Now(),
	}}
	for e := range src.Events(ctx) {
		if err := l.Run(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// buildPolicy wires the decision policy named on the CLI. rules is deterministic;
// llm reasons with Claude and falls back to rules; shadow runs both and acts only
// on rules while logging the diff. The LLM policies read shepherd's schema
// (best-effort) and the ANTHROPIC_API_KEY / ant-login profile from the env.
func buildPolicy(ctx context.Context, name string, st store.ShepherdStore) loop.Policy {
	rules := policy.RulesPolicy{}
	if name == "rules" {
		return rules
	}

	schema, err := st.Schema(ctx)
	if err != nil {
		log.Printf("drover: no shepherd schema (%v); reasoning unconstrained by it", err)
		schema = nil
	}
	llm := policy.LLMReasoner{
		Reasoner: policy.NewAnthropicReasoner(),
		Schema:   schema,
		Timeout:  30 * time.Second,
		Fallback: rules,
		Logf:     log.Printf,
	}
	switch name {
	case "shadow":
		return policy.ShadowPolicy{Trusted: rules, Shadow: llm, Logf: log.Printf}
	case "llm":
		return llm
	default:
		log.Printf("drover: unknown policy %q; using rules", name)
		return rules
	}
}

// daemon reacts to board changes live: it streams shepherd watch through the
// loop until interrupted, reconnecting if shepherd restarts.
func daemon(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	project := fs.String("project", "", "shepherd board to watch")
	interval := fs.Duration("interval", 0, "watch poll interval (0 = shepherd default)")
	verbose := fs.Bool("verbose", false, "log every event processed")
	fs.Parse(argv)

	// Cancel on SIGINT/SIGTERM so the watch stream closes and we drain cleanly.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger := log.New(os.Stderr, "drover: ", 0)
	st := store.ShepherdStore{Project: *project}
	l := buildLoop(st)
	src := source.WatchSource{
		Project:  *project,
		Interval: *interval,
		Logf:     logger.Printf,
	}

	logger.Printf("daemon watching board %q", boardName(*project))
	for e := range src.Events(ctx) {
		if *verbose {
			logger.Printf("event %s", e.Type)
		}
		if err := l.Run(ctx, e); err != nil && ctx.Err() == nil {
			// Keep the daemon alive; one bad event shouldn't take it down.
			logger.Printf("processing %s: %v", e.Type, err)
		}
	}
	logger.Printf("daemon stopped")
	return nil
}

func boardName(p string) string {
	if p == "" {
		return "default"
	}
	return p
}
