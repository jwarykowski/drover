package source

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// freeAddr reserves a loopback port so parallel runs don't collide.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// The two transports must be indistinguishable downstream: a POSTed event
// arrives as the same loop.Event a spawned plugin would have produced.
func TestHTTPSourceAcceptsPostedEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr := freeAddr(t)
	src := HTTPSource{Name: "sentry", Addr: addr}
	ch := src.Events(ctx)

	url := "http://" + addr + "/events"
	body := strings.Join([]string{
		`{"id":"e1","type":"sentry.issue.opened","data":{"title":"boom","subject":"S-1"}}`,
		`{"id":"e2","type":"sentry.issue.opened","data":{"title":"bang","subject":"S-2"}}`,
	}, "\n")

	// The listener comes up asynchronously; retry briefly rather than sleeping.
	var resp *http.Response
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = http.Post(url, "application/x-ndjson", strings.NewReader(body))
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	for i, want := range []string{"e1", "e2"} {
		select {
		case e := <-ch:
			if e.ID != want {
				t.Fatalf("event %d = %q, want %q", i, e.ID, want)
			}
			if e.Source != "sentry" {
				t.Fatalf("source should default to the config name, got %q", e.Source)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("event %d (%s) never arrived", i, want)
		}
	}
}

// A GET (or any other verb) is not an event: the route is POST-only, so a
// stray probe can't inject anything.
func TestHTTPSourceRejectsNonPost(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr := freeAddr(t)
	src := HTTPSource{Name: "sentry", Addr: addr}
	ch := src.Events(ctx)

	url := "http://" + addr + "/events"
	var resp *http.Response
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = http.Get(url)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted {
		t.Fatal("GET must not be accepted as an event")
	}
	select {
	case e := <-ch:
		t.Fatalf("a GET produced an event: %+v", e)
	case <-time.After(100 * time.Millisecond):
	}
}
