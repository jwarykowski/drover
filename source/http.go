package source

import (
	"context"
	"net/http"
	"time"

	"github.com/jwarykowski/drover/loop"
)

// HTTPSource is the remote transport: a source drover does not spawn POSTs
// NDJSON to it instead. Same envelope as ExecSource, so an action cannot tell
// which transport raised an event.
//
// The bind is localhost-only by default and there is no authentication: anything
// that can reach the port can raise an event, and an event can select any
// action. Do not bind it to a public interface — put a reverse proxy that
// authenticates in front of it first.
type HTTPSource struct {
	Name string // config source name; the default Event.Source
	Addr string // host:port; defaults to 127.0.0.1:9099
	Logf func(string, ...any)
}

func (s HTTPSource) addr() string {
	if s.Addr == "" {
		return "127.0.0.1:9099"
	}
	return s.Addr
}

func (s HTTPSource) logf(format string, a ...any) {
	if s.Logf != nil {
		s.Logf(format, a...)
	}
}

// Events serves POST /events until ctx is cancelled. The channel is buffered so
// a slow loop does not stall the sender's request; a full buffer applies
// backpressure by holding the request open.
func (s HTTPSource) Events(ctx context.Context) <-chan loop.Event {
	out := make(chan loop.Event, 64)
	go func() {
		defer close(out)

		mux := http.NewServeMux()
		mux.HandleFunc("POST /events", func(w http.ResponseWriter, r *http.Request) {
			if err := Scan(r.Context(), r.Body, out, s.Name, s.logf); err != nil && r.Context().Err() == nil {
				s.logf("source %q: read body: %v", s.Name, err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		})

		srv := &http.Server{Addr: s.addr(), Handler: mux}
		go func() {
			<-ctx.Done()
			sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
		}()

		s.logf("source %q listening on http://%s/events", s.Name, s.addr())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logf("source %q: %v", s.Name, err)
		}
	}()
	return out
}
