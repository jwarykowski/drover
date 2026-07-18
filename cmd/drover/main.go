// Command drover runs the loop around a shepherd board. Subcommands: doctor
// (prove the boundary), ingest (one signal, one task), daemon (react live).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	dctx "github.com/jwarykowski/drover/context"
	"github.com/jwarykowski/drover/exec"
	"github.com/jwarykowski/drover/loop"
	"github.com/jwarykowski/drover/policy"
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
	fmt.Fprintln(os.Stderr, "usage: drover <doctor|ingest|daemon> [flags]")
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
		Kind:    *kind,
		Source:  "git",
		Payload: map[string]any{"title": *title, "link": *link},
		At:      time.Now(),
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
			logger.Printf("event %s", e.Kind)
		}
		if err := l.Run(ctx, e); err != nil && ctx.Err() == nil {
			// Keep the daemon alive; one bad event shouldn't take it down.
			logger.Printf("processing %s: %v", e.Kind, err)
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
