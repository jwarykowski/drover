package source

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jwarykowski/drover/loop"
)

// SentrySource senses new Sentry issues by polling the issues API. It is the
// second source, and deliberately mirrors GitHubSource: a transport (poll) plus
// a decoder (decodeSentry). Dedup keyed on Event.ID makes it at-most-once. Auth
// is a bearer token; nothing here knows about the rest of drover.
type SentrySource struct {
	Org      string        // Sentry org slug
	Project  string        // Sentry project slug
	Token    string        // auth token (from env, set by the wiring)
	BaseURL  string        // defaults to https://sentry.io
	Interval time.Duration // poll interval; defaults to 60s
	Logf     func(string, ...any)
	// run fetches the issues JSON; injectable so tests don't hit the network.
	run func(ctx context.Context) ([]byte, error)
}

// sentryIssue is the slice of the issues API drover reads.
type sentryIssue struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Culprit   string `json:"culprit"`
	Permalink string `json:"permalink"`
	Level     string `json:"level"`
}

func (s SentrySource) baseURL() string {
	if s.BaseURL == "" {
		return "https://sentry.io"
	}
	return s.BaseURL
}

func (s SentrySource) interval() time.Duration {
	if s.Interval <= 0 {
		return 60 * time.Second
	}
	return s.Interval
}

func (s SentrySource) logf(format string, a ...any) {
	if s.Logf != nil {
		s.Logf(format, a...)
	}
}

func (s SentrySource) runner() func(context.Context) ([]byte, error) {
	if s.run != nil {
		return s.run
	}
	return s.fetch
}

// Events polls until ctx is cancelled, emitting one event per issue seen.
func (s SentrySource) Events(ctx context.Context) <-chan loop.Event {
	out := make(chan loop.Event)
	go func() {
		defer close(out)
		t := time.NewTicker(s.interval())
		defer t.Stop()
		for {
			raw, err := s.runner()(ctx)
			if err != nil && ctx.Err() == nil {
				s.logf("sentry poll %s/%s: %v", s.Org, s.Project, err)
			} else {
				evs, derr := decodeSentry(s.Project, raw)
				if derr != nil {
					s.logf("sentry decode: %v", derr)
				}
				for _, e := range evs {
					select {
					case <-ctx.Done():
						return
					case out <- e:
					}
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return out
}

func (s SentrySource) fetch(ctx context.Context) ([]byte, error) {
	url := fmt.Sprintf("%s/api/0/projects/%s/%s/issues/?query=is:unresolved",
		s.baseURL(), s.Org, s.Project)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sentry: %s", resp.Status)
	}
	return body, nil
}

// decodeSentry normalises the issues API into sentry.issue.opened events.
func decodeSentry(project string, raw []byte) ([]loop.Event, error) {
	var issues []sentryIssue
	if err := json.Unmarshal(raw, &issues); err != nil {
		return nil, fmt.Errorf("sentry: parse issues: %w", err)
	}
	var evs []loop.Event
	for _, is := range issues {
		evs = append(evs, loop.Event{
			ID:     fmt.Sprintf("sentry/%s:issue:%s", project, is.ID),
			Type:   "sentry.issue.opened",
			Source: "sentry/" + project,
			Data: loop.Signal{
				Repo: project, Title: is.Title, URL: is.Permalink,
				Key:   "issue:" + is.ID,
				Extra: map[string]any{"culprit": is.Culprit, "level": is.Level},
			},
			At: time.Now(),
		})
	}
	return evs, nil
}
