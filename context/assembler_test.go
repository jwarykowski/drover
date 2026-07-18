package context

import (
	"context"
	"testing"

	"github.com/jwarykowski/drover/loop"
	"github.com/jwarykowski/drover/store"
)

func TestWorkingContextAttendsToCategory(t *testing.T) {
	fs := &store.FakeStore{}
	fs.Seed(
		loop.Item{Text: "old ci break", Category: "ci"},
		loop.Item{Text: "buy milk", Category: "home"},
	)
	w := WorkingContext{Store: fs}

	c, err := w.Assemble(context.Background(), loop.Event{Kind: "ci.failed"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Event.Kind != "ci.failed" {
		t.Errorf("event not carried through: %+v", c.Event)
	}
	// ci.failed attends to the ci slice only.
	if len(c.Board) != 1 || c.Board[0].Category != "ci" {
		t.Errorf("want 1 ci item, got %+v", c.Board)
	}
}

func TestWorkingContextUnknownKindSeesWholeBoard(t *testing.T) {
	fs := &store.FakeStore{}
	fs.Seed(loop.Item{Text: "a", Category: "ci"}, loop.Item{Text: "b", Category: "home"})
	w := WorkingContext{Store: fs}

	c, err := w.Assemble(context.Background(), loop.Event{Kind: "note.added"})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Board) != 2 {
		t.Errorf("unknown kind: want whole board (2), got %d", len(c.Board))
	}
}
