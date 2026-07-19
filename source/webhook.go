package source

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"time"

	"github.com/jwarykowski/drover/loop"
)

// WebhookSource senses GitHub events by push instead of poll. In Forward mode it
// runs `gh webhook forward`, which registers a repo webhook and relays each
// delivery to a local URL over GitHub's websocket — so there is no public
// endpoint, no inbound port, and gh's existing auth is reused. It binds an
// http.Server to 127.0.0.1 (localhost-only, so no TLS/HMAC) and turns each
// forwarded delivery into a typed event.
//
// ponytail: localhost only; add HMAC verification + a public bind when a remote
// receiver deployment is real.
type WebhookSource struct {
	Repo    string   // owner/name, for `gh webhook forward --repo` and logging
	Kinds   []string // gh event names; defaults to ["pull_request","issues"]
	Addr    string   // local bind; defaults to "127.0.0.1:9099"
	Forward bool     // spawn `gh webhook forward` to relay deliveries here
	Bin     string   // gh binary; defaults to "gh"
	Logf    func(string, ...any)
}

func (s WebhookSource) addr() string {
	if s.Addr == "" {
		return "127.0.0.1:9099"
	}
	return s.Addr
}

func (s WebhookSource) events() []string {
	if len(s.Kinds) == 0 {
		return []string{"pull_request", "issues"}
	}
	return s.Kinds
}

func (s WebhookSource) bin() string {
	if s.Bin == "" {
		return "gh"
	}
	return s.Bin
}

func (s WebhookSource) logf(format string, a ...any) {
	if s.Logf != nil {
		s.Logf(format, a...)
	}
}

// Events serves the local receiver (and, in Forward mode, the gh relay) until
// ctx is cancelled. The channel is modestly buffered so a slow loop doesn't
// stall gh's delivery POSTs.
func (s WebhookSource) Events(ctx context.Context) <-chan loop.Event {
	out := make(chan loop.Event, 64)
	go func() {
		defer close(out)

		mux := http.NewServeMux()
		mux.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read", http.StatusBadRequest)
				return
			}
			evs, err := decodeGitHubWebhook(r.Header.Get("X-GitHub-Event"), body)
			if err != nil {
				s.logf("webhook decode: %v", err)
			}
			for _, e := range evs {
				select {
				case <-ctx.Done():
				case out <- e:
				}
			}
			w.WriteHeader(http.StatusOK)
		})
		srv := &http.Server{Addr: s.addr(), Handler: mux}

		go func() {
			<-ctx.Done()
			sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
		}()

		if s.Forward {
			go s.runForward(ctx)
		}

		s.logf("webhook listening on %s for %s", s.addr(), s.Repo)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logf("webhook server: %v", err)
		}
	}()
	return out
}

// runForward spawns `gh webhook forward`, restarting it if it drops while ctx is
// live.
func (s WebhookSource) runForward(ctx context.Context) {
	url := "http://" + s.addr() + "/hook"
	events := ""
	for i, e := range s.events() {
		if i > 0 {
			events += ","
		}
		events += e
	}
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, s.bin(), "webhook", "forward",
			"--repo="+s.Repo, "--events="+events, "--url="+url)
		if err := cmd.Run(); err != nil && ctx.Err() == nil {
			s.logf("gh webhook forward ended (%v); retrying in 2s", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
}

// decodeGitHubWebhook normalises a forwarded GitHub delivery (identified by the
// X-GitHub-Event header) into typed events. A PR closed with merged=true is a
// merge; unhandled actions produce nothing.
func decodeGitHubWebhook(event string, raw []byte) ([]loop.Event, error) {
	switch event {
	case "pull_request":
		var p struct {
			Action string `json:"action"`
			PR     struct {
				Number   int    `json:"number"`
				Title    string `json:"title"`
				HTMLURL  string `json:"html_url"`
				Merged   bool   `json:"merged"`
				MergeSHA string `json:"merge_commit_sha"`
			} `json:"pull_request"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("webhook: parse pull_request: %w", err)
		}
		verb := p.Action
		if p.Action == "closed" && p.PR.Merged {
			verb = "merged"
		}
		if verb != "opened" && verb != "closed" && verb != "merged" {
			return nil, nil
		}
		repo := p.Repo.FullName
		return []loop.Event{{
			ID:     fmt.Sprintf("github/%s:pr:%d:%s", repo, p.PR.Number, verb),
			Type:   "github.pull_request." + verb,
			Source: "github/" + repo,
			Data: loop.Signal{
				Repo: repo, Title: p.PR.Title, URL: p.PR.HTMLURL,
				Key:   fmt.Sprintf("pr:%d:%s", p.PR.Number, verb),
				Extra: map[string]any{"pr": p.PR.Number, "sha": p.PR.MergeSHA},
			},
			At: time.Now(),
		}}, nil

	case "issues":
		var p struct {
			Action string `json:"action"`
			Issue  struct {
				Number  int    `json:"number"`
				Title   string `json:"title"`
				HTMLURL string `json:"html_url"`
			} `json:"issue"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("webhook: parse issues: %w", err)
		}
		if p.Action != "opened" {
			return nil, nil
		}
		repo := p.Repo.FullName
		return []loop.Event{{
			ID:     fmt.Sprintf("github/%s:issue:%d:opened", repo, p.Issue.Number),
			Type:   "github.issues.opened",
			Source: "github/" + repo,
			Data: loop.Signal{
				Repo: repo, Title: p.Issue.Title, URL: p.Issue.HTMLURL,
				Key:   fmt.Sprintf("issue:%d:opened", p.Issue.Number),
				Extra: map[string]any{"issue": p.Issue.Number},
			},
			At: time.Now(),
		}}, nil
	}
	return nil, nil
}
