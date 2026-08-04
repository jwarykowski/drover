package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// A shim must die when its spawner does, or it orphans holding a port. Guarding
// the whole condition: alive parent keeps running, reparented parent cancels.
func TestWatchParentCancelsOnReparent(t *testing.T) {
	var ppid atomic.Int64
	ppid.Store(4242)
	ctx, cancel := watchParent(context.Background(), func() int { return int(ppid.Load()) }, time.Millisecond)
	defer cancel()

	select {
	case <-ctx.Done():
		t.Fatal("cancelled while the parent was still alive")
	case <-time.After(20 * time.Millisecond):
	}

	ppid.Store(1)
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("still running after the parent died")
	}
}

// A launchd job is parented to init from its first instruction; treating that as
// orphaned would make it exit one tick after starting.
func TestWatchParentIgnoresInitLaunch(t *testing.T) {
	ctx, cancel := watchParent(context.Background(), func() int { return 1 }, time.Millisecond)
	defer cancel()
	select {
	case <-ctx.Done():
		t.Fatal("cancelled a process init launched directly")
	case <-time.After(20 * time.Millisecond):
	}
}
