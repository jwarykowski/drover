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
	if got[0].Kind != "board.added" || got[1].Kind != "board.removed" {
		t.Errorf("wrong kinds: %q, %q", got[0].Kind, got[1].Kind)
	}
	it, ok := got[0].Payload["item"].(loop.Item)
	if !ok || it.ID != "2" || it.Category != "ci" {
		t.Errorf("added payload lost the item: %+v", got[0].Payload)
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
