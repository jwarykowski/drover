//go:build integration

// Integration test against a real shepherd binary. Run with:
//
//	go test -tags integration ./store/
//
// Requires `shepherd` on PATH. Uses a throwaway board and deletes it
// afterwards, so it never touches a real one.
package store

import (
	"context"
	"os/exec"
	"testing"

	"github.com/jwarykowski/drover/loop"
)

func TestShepherdStoreRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("shepherd"); err != nil {
		t.Skip("shepherd not on PATH")
	}
	const board = "drover-integration"
	ctx := context.Background()
	st := ShepherdStore{Project: board}
	t.Cleanup(func() { _ = exec.Command("shepherd", "project", "delete", board, "--force").Run() })

	added, err := st.Add(ctx, loop.Spec{
		Text: "integration probe", Category: "ci", Priority: "H",
		Link: "https://ci.example/run/1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if added.ID == "" {
		t.Fatal("add returned no id")
	}
	if added.Priority != "H" || added.Category != "ci" || added.Link != "https://ci.example/run/1" {
		t.Errorf("round-trip lost fields: %+v", added)
	}

	got, err := st.List(ctx, loop.Filter{Category: "ci"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != added.ID {
		t.Fatalf("list: want the added item, got %+v", got)
	}

	if err := st.SetStatus(ctx, added.ID, "done"); err != nil {
		t.Fatal(err)
	}
	open, err := st.List(ctx, loop.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Errorf("after done: want 0 open, got %d", len(open))
	}

	// Typed error on a bad id.
	if err := st.SetStatus(ctx, "nope", "done"); err == nil {
		t.Error("SetStatus on bad id: want error, got nil")
	}
}
