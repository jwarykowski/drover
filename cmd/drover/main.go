// Command drover runs the loop around a shepherd board. Phase 0+1 ships two
// subcommands: doctor (prove the boundary) and ingest (one signal, one task).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
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
	fmt.Fprintln(os.Stderr, "usage: drover <doctor|ingest> [flags]")
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

// ingest turns one CLI-supplied signal into one loop run.
func ingest(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	kind := fs.String("kind", "ci.failed", "event kind")
	link := fs.String("link", "", "link to attach (e.g. CI run URL)")
	title := fs.String("title", "", "short description of what happened")
	project := fs.String("project", "", "shepherd board to act on")
	fs.Parse(argv)

	st := store.ShepherdStore{Project: *project}
	l := loop.Loop{
		Assembler: dctx.WorkingContext{Store: st},
		Policy:    policy.RulesPolicy{},
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
