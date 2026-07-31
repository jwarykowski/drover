// Command drover runs the sense → match → act loop. Subcommands: watch (the
// closed loop), action (author drover.toml), source (the built-in source
// plugins), doctor (check the wiring).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	osexec "os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/charmbracelet/x/term"
	"github.com/jwarykowski/drover/config"
	"github.com/jwarykowski/drover/daemon"
	"github.com/jwarykowski/drover/loop"
	"github.com/jwarykowski/drover/store"
	"github.com/jwarykowski/drover/tui"
)

func main() {
	// Bare `drover` on a terminal opens the interactive dashboard; piped/CI it
	// prints usage instead of trying to draw a TUI.
	if len(os.Args) < 2 {
		if term.IsTerminal(os.Stdin.Fd()) {
			if err := tui.Dashboard(config.DefaultPath()); err != nil {
				fmt.Fprintln(os.Stderr, "drover:", err)
				os.Exit(1)
			}
			return
		}
		usage()
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "doctor":
		err = doctor(ctx, os.Args[2:])
	case "watch":
		err = watch(ctx, os.Args[2:])
	case "action":
		err = actionCmd(os.Args[2:])
	case "source":
		err = sourceCmd(ctx, os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("drover", tui.Version) // tui.Version is the single source of truth
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

const usageText = `drover — the sense→match→act loop

Usage:
  drover                  open the interactive dashboard (lanes + watch control)
  drover <command> [flags]

Commands:
  watch      run every configured source and drive the loop
  action     author the actions in drover.toml (opens a TUI)
  source     run a built-in source plugin (github | shepherd)
  doctor     check the config: sources, agents and their binaries
  version    print the version and exit
  help       print this help

watch:  (needs no flags — everything comes from drover.toml)
  --config <path>         config file (default ~/.config/drover/drover.toml)
  --agents <n>            agent runs allowed in parallel (default 1)
  --seen <file>           persist handled event ids across restarts
  --provenance <file>     also tee the per-agent-run JSON trace to a file

stdout carries the structured per-agent-run JSON trace; stderr the operational log.

action:
  (bare)                  interactive TUI: create/view/edit/delete actions
  add|list|edit|rm        scriptable action management

source: run a source plugin, writing NDJSON events to stdout. Normally spawned
by watch through a [[source]] row rather than by hand.
  drover source github --repo owner/name [--mode poll|forward] [--base branch]
  drover source shepherd [--board name] [--all]

A source is any process that writes drover's event envelope to stdout, or any
service that POSTs it to a [[source]] http address — these two ship in the box.`

// watch runs the closed loop: every configured source streams events, each
// event is matched against the actions in drover.toml, a match parks a run, and
// once released (by a human, or immediately for an action marked auto) the
// action's agentic tool runs and its verdict reconciles the task.
func watch(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath(), "path to drover.toml")
	seenPath := fs.String("seen", "", "file recording handled event ids (survives restarts)")
	provPath := fs.String("provenance", "", "also append the per-agent-run JSON records to this file (they always stream to stdout)")
	agents := fs.Int("agents", 1, "number of agent runs to allow in parallel")
	fs.Parse(argv)

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger := log.New(os.Stderr, "drover: ", 0)

	// Provenance streams to stdout (stderr carries the operational log); tee to
	// the --provenance file when given.
	var provW io.Writer = os.Stdout
	if *provPath != "" {
		f, err := os.OpenFile(*provPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		provW = io.MultiWriter(os.Stdout, f)
	}

	return daemon.Run(ctx, daemon.Config{
		ConfigPath: *cfgPath,
		SeenPath:   *seenPath,
		Agents:     *agents,
	}, provW, logger.Printf)
}

// sourceCmd runs one of the source plugins that ship in this binary. They have
// no privileged path into drover — watch spawns them through the same
// ExecSource any third-party plugin gets.
func sourceCmd(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("source: missing plugin name (github | shepherd)")
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	switch argv[0] {
	case "github":
		return sourceGitHub(ctx, argv[1:])
	case "shepherd":
		return sourceShepherd(ctx, argv[1:])
	default:
		return fmt.Errorf("source: unknown plugin %q (want github or shepherd)", argv[0])
	}
}

// actionCmd is the CRUD UI over the actions in drover.toml. This is the only
// writer of the actions an event can select, so it is where events bind to what
// an agent does.
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

// actionTUI opens the interactive action manager. It needs a terminal; when
// stdin isn't one (piped/CI), it prints the scriptable usage instead of crashing.
func actionTUI(argv []string) error {
	fs := flag.NewFlagSet("action", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath(), "path to drover.toml")
	fs.Parse(argv)
	if !term.IsTerminal(os.Stdin.Fd()) {
		fmt.Fprintln(os.Stderr, "usage: drover action <add|list|edit|rm> [flags]  (bare `drover action` opens the TUI on a terminal)")
		return fmt.Errorf("action: not a terminal")
	}
	return tui.Run(*cfgPath)
}

// whereFlag collects repeated --where key=value filters.
type whereFlag map[string]string

func (m whereFlag) String() string { return "" }
func (m whereFlag) Set(kv string) error {
	k, v, ok := strings.Cut(kv, "=")
	if !ok {
		return fmt.Errorf("expected key=value, got %q", kv)
	}
	m[k] = v
	return nil
}

func actionAdd(argv []string) error {
	fs := flag.NewFlagSet("action add", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath(), "path to drover.toml")
	name := fs.String("name", "", "friendly label (required)")
	on := fs.String("on", "", "event type to match (required)")
	runner := fs.String("runner", "", "runner to invoke; empty uses the first [[runner]]")
	target := fs.String("target", "", "directory the runner runs in; may template {{key}} from event data")
	mode := fs.String("mode", "", "permission mode passed to the runner")
	auto := fs.Bool("auto", false, "fire without waiting for a human release")
	doFile := fs.String("do-file", "", "read the prompt body from this file instead of $EDITOR")
	where := whereFlag{}
	fs.Var(where, "where", "event data filter, key=value (repeatable)")
	fs.Parse(argv)

	if *name == "" || *on == "" {
		return fmt.Errorf("action add: --name and --on are required")
	}
	cf, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if _, err := cf.RunnerByName(*runner); err != nil {
		return fmt.Errorf("action add: %w", err)
	}
	if !cf.ValidMode(*runner, *mode) {
		return fmt.Errorf("action add: mode %q is not one this runner accepts", *mode)
	}
	if known := cf.Types(); len(known) > 0 && !slices.Contains(known, *on) {
		fmt.Fprintf(os.Stderr, "note: no configured source declares %q (declared: %s)\n", *on, strings.Join(known, ", "))
	}
	do, err := promptBody(*doFile)
	if err != nil {
		return err
	}
	if strings.TrimSpace(do) == "" {
		return fmt.Errorf("action add: empty prompt body")
	}

	a := config.Action{Name: *name, On: *on, Where: where, Runner: *runner, Target: *target, Mode: *mode, Auto: *auto, Do: do}
	warnRisky(a)
	saved, _ := cf.Add(a)
	if err := cf.Save(); err != nil {
		return err
	}
	fmt.Printf("added action %s (%s)\n", saved.ID, saved.Name)
	return nil
}

// warnRisky says plainly what an unattended, permission-waiving action means.
// It warns rather than refuses: on a source whose content nobody outside the
// operator can write, it is a reasonable thing to want.
func warnRisky(a config.Action) {
	if !a.Risky() {
		return
	}
	fmt.Fprintf(os.Stderr,
		"warning: action %q runs unattended (auto) in mode %q, so an agent with file system access\n"+
			"         acts on event text drover did not author. Only do this for a source whose\n"+
			"         content you control end to end.\n", a.Name, a.Mode)
}

func actionList(argv []string) error {
	fs := flag.NewFlagSet("action list", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath(), "path to drover.toml")
	fs.Parse(argv)
	cf, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	actions := cf.Actions()
	if len(actions) == 0 {
		fmt.Println("no actions configured")
		return nil
	}
	fmt.Println("id        name  on  where  agent")
	for _, a := range actions {
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
	cfgPath := fs.String("config", config.DefaultPath(), "path to drover.toml")
	name := fs.String("name", "", "new label")
	runner := fs.String("runner", "", "new runner")
	target := fs.String("target", "", "new target directory")
	mode := fs.String("mode", "", "new permission mode")
	doFile := fs.String("do-file", "", "replace the prompt body from this file")
	editDo := fs.Bool("do", false, "replace the prompt body in $EDITOR")
	auto := fs.String("auto", "", "true|false — fire without a human release")
	where := whereFlag{}
	fs.Var(where, "where", "replace the event data filter, key=value (repeatable)")
	fs.Parse(rest)

	cf, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	a, ok := cf.ByID(id)
	if !ok {
		return fmt.Errorf("action edit: %w: %s", config.ErrNotFound, id)
	}
	if *name != "" {
		a.Name = *name
	}
	if *runner != "" {
		if _, err := cf.RunnerByName(*runner); err != nil {
			return fmt.Errorf("action edit: %w", err)
		}
		a.Runner = *runner
	}
	if *target != "" {
		a.Target = *target
	}
	if *mode != "" {
		if !cf.ValidMode(a.Runner, *mode) {
			return fmt.Errorf("action edit: mode %q is not one runner %q accepts", *mode, a.Runner)
		}
		a.Mode = *mode
	}
	if len(where) > 0 {
		a.Where = where
	}
	switch *auto {
	case "true":
		a.Auto = true
	case "false":
		a.Auto = false
	case "":
	default:
		return fmt.Errorf("action edit: --auto wants true or false, got %q", *auto)
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
	warnRisky(a)
	if err := cf.Replace(a); err != nil {
		return err
	}
	if err := cf.Save(); err != nil {
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
	cfgPath := fs.String("config", config.DefaultPath(), "path to drover.toml")
	fs.Parse(rest)
	cf, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if err := cf.Remove(id); err != nil {
		return err
	}
	if err := cf.Save(); err != nil {
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

// doctor reports what drover is wired to and whether it can actually run it:
// every source's transport and binary, every agent's binary, every action's
// bindings, and the task store. It changes nothing.
func doctor(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath(), "path to drover.toml")
	fs.Parse(argv)

	cf, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	fmt.Printf("config: %s\n", *cfgPath)

	problems := 0
	sources := cf.Sources()
	fmt.Printf("\nsources (%d):\n", len(sources))
	if len(sources) == 0 {
		fmt.Println("  none — drover will only react to its own tasks")
	}
	for _, s := range sources {
		switch {
		case len(s.Cmd) > 0 && s.HTTP != "":
			fmt.Printf("  ✗ %-14s sets both cmd and http; it will be skipped\n", s.Name)
			problems++
		case len(s.Cmd) > 0:
			mark, bad := lookMark(s.Cmd[0])
			problems += bad
			fmt.Printf("  %s %-14s exec  %s\n", mark, s.Name, strings.Join(s.Cmd, " "))
		case s.HTTP != "":
			fmt.Printf("  ✓ %-14s http  %s/events\n", s.Name, s.HTTP)
		default:
			fmt.Printf("  ✗ %-14s sets neither cmd nor http; it will be skipped\n", s.Name)
			problems++
		}
		if len(s.Types) > 0 {
			fmt.Printf("    emits: %s\n", strings.Join(s.Types, ", "))
		}
	}

	runners := cf.RunnerNames()
	fmt.Printf("\nrunners (%d):\n", len(runners))
	if len(runners) == 0 {
		fmt.Println("  ✗ none — every action will fail to run")
		problems++
	}
	for _, name := range runners {
		g, _ := cf.RunnerByName(name)
		if len(g.Cmd) == 0 {
			fmt.Printf("  ✗ %-14s has an empty cmd\n", name)
			problems++
			continue
		}
		mark, bad := lookMark(g.Cmd[0])
		problems += bad
		fmt.Printf("  %s %-14s %s\n", mark, name, strings.Join(g.Cmd, " "))
	}

	actions := cf.Actions()
	fmt.Printf("\nactions (%d):\n", len(actions))
	declared := cf.Types()
	for _, a := range actions {
		flags := ""
		if a.Auto {
			flags = "  [auto]"
		}
		fmt.Printf("  %-8s %-14s on %s%s\n", a.ID, a.Name, a.On, flags)
		if _, err := cf.RunnerByName(a.Runner); err != nil {
			fmt.Printf("    ✗ %v\n", err)
			problems++
		}
		if len(declared) > 0 && !slices.Contains(declared, a.On) {
			fmt.Printf("    ! no configured source declares %q\n", a.On)
		}
		if a.Risky() {
			fmt.Printf("    ! runs unattended in %s — an agent acts on event text with no human gate\n", a.Mode)
		}
	}

	st, err := store.OpenFileStore(store.DefaultTasksPath())
	if err != nil {
		return err
	}
	tasks, err := st.List(ctx, loop.Filter{})
	if err != nil {
		return err
	}
	fmt.Printf("\ntasks: %d open in %s\n", len(tasks), store.DefaultTasksPath())

	if problems > 0 {
		return fmt.Errorf("doctor: %d problem(s) found", problems)
	}
	fmt.Println("\nall good")
	return nil
}

// lookMark reports whether a command resolves on PATH, returning the tick and
// how much it adds to the problem count.
func lookMark(bin string) (string, int) {
	if _, err := osexec.LookPath(bin); err != nil {
		return "✗", 1
	}
	return "✓", 0
}
