package source

import (
	"context"
	"strings"
	"testing"

	"github.com/jwarykowski/drover/loop"
)

func TestScanEmitsChangesSkipsSnapshot(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"snapshot","items":[{"id":"1","text":"existing"}]}`,
		`{"type":"added","item":{"id":"2","text":"new ci break","category":"ci"}}`,
		`not json — should be skipped`,
		`{"type":"removed","item":{"id":"1","text":"existing"}}`,
	}, "\n")

	out := make(chan loop.Event, 8)
	if err := scan(context.Background(), strings.NewReader(stream), out, nil); err != nil {
		t.Fatal(err)
	}
	close(out)

	var got []loop.Event
	for e := range out {
		got = append(got, e)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 change events (snapshot + garbage skipped), got %d", len(got))
	}
	if got[0].Type != "board.added" || got[1].Type != "board.removed" {
		t.Errorf("wrong types: %q, %q", got[0].Type, got[1].Type)
	}
	bc, ok := got[0].Data.(loop.BoardChange)
	if !ok || bc.Item.ID != "2" || bc.Item.Category != "ci" {
		t.Errorf("added payload lost the item: %+v", got[0].Data)
	}
}

func TestScanStopsOnContextCancel(t *testing.T) {
	// Unbuffered channel with no consumer: the first send blocks, so a cancelled
	// context must unblock scan rather than hang.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := make(chan loop.Event) // no reader
	err := scan(ctx, strings.NewReader(`{"type":"added","item":{"id":"1"}}`), out, nil)
	if err == nil {
		t.Error("want context error on cancelled send, got nil")
	}
}
