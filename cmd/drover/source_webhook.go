package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	osexec "os/exec"
	"sync"
	"time"
)

// `drover source webhook` is the generic remote intake: it accepts whatever JSON
// a third party POSTs and maps it to drover's envelope with a jq program from
// config. HTTPSource already accepts the envelope, but nothing outside drover
// speaks it, so without this every provider needs its own shim; with it a new
// SaaS source is a [[source]] row.
//
//	[[source]]
//	name  = "datadog"
//	cmd   = ["drover", "source", "webhook", "--addr", "127.0.0.1:9110", "--map",
//	         '{id: "dd:\(.id)", type: "datadog.alert", data: {title: .title, subject: .monitor_id, url: .link}}']
//	types = ["datadog.alert"]
//
// The mapping lives in config, not in Go, so the shim never models a provider's
// payload and adding one costs no build. jq's output is copied to stdout
// unparsed: drover's own decode is the validator and there is exactly one of
// those. Emitting an id unique per logical event is the mapping's job — a
// mapping that hardcodes one id fires once, ever, because Dedup remembers it.
//
// The bind is localhost by default and unauthenticated, same posture as
// source.HTTPSource: anything that can reach the port can raise an event, and an
// event can select any action. Put an authenticating proxy in front before
// exposing it.
func sourceWebhook(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("source webhook", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:9099", "bind for POST /events")
	prog := fs.String("map", ".", "jq program mapping the payload to drover's envelope")
	limit := fs.Int64("max-body", 1<<20, "reject request bodies larger than this")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := osexec.LookPath("jq"); err != nil {
		return fmt.Errorf("source webhook: jq not found in PATH (the mapping runs in jq)")
	}

	// Handlers are concurrent; a torn line is an unparsable event.
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("POST /events", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, *limit))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		out, err := mapPayload(r.Context(), *prog, body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "webhook map: %v\n", err)
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if _, err := os.Stdout.Write(out); err != nil {
			fmt.Fprintf(os.Stderr, "webhook emit: %v\n", err)
		}
		w.WriteHeader(http.StatusAccepted)
	})

	srv := &http.Server{Addr: *addr, Handler: mux}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	fmt.Fprintf(os.Stderr, "webhook listening on http://%s/events\n", *addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

// mapPayload runs one delivery through the jq program and returns the NDJSON to
// emit. -c keeps one output object on one line; a mapping that emits several
// objects (a batched payload) is several events, which is the useful behaviour.
func mapPayload(ctx context.Context, prog string, body []byte) ([]byte, error) {
	cmd := osexec.CommandContext(ctx, "jq", "-c", prog)
	cmd.Stdin = bytes.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("jq: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, fmt.Errorf("mapping produced no output (a filter that selects nothing drops the delivery)")
	}
	return out, nil
}
