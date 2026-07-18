package source

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jwarykowski/drover/loop"
)

// GitHubSource senses merges to a branch by polling merged pull requests through
// the gh CLI (already authenticated — no token handling, no inbound webhook).
// Each newly-merged PR becomes one github.merged event. A cursor (highest PR
// number already emitted) makes it at-most-once across polls and restarts; on
// first run with no cursor it seeds to the current head WITHOUT firing, so
// history doesn't stampede the board.
type GitHubSource struct {
	Repo       string        // owner/name
	Base       string        // target branch; defaults to "master"
	Interval   time.Duration // poll interval; defaults to 60s
	CursorPath string        // file holding the last-emitted PR number; empty = memory only
	Logf       func(string, ...any)
	// run fetches merged PRs as gh JSON; injectable so tests don't shell out.
	run func(ctx context.Context, repo, base string) ([]byte, error)
}

// ghPR is the slice of `gh pr list --json` drover reads.
type ghPR struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	MergedAt    string `json:"mergedAt"`
	MergeCommit struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
}

func (s GitHubSource) base() string {
	if s.Base == "" {
		return "master"
	}
	return s.Base
}

func (s GitHubSource) interval() time.Duration {
	if s.Interval <= 0 {
		return 60 * time.Second
	}
	return s.Interval
}

func (s GitHubSource) logf(format string, a ...any) {
	if s.Logf != nil {
		s.Logf(format, a...)
	}
}

func (s GitHubSource) runner() func(context.Context, string, string) ([]byte, error) {
	if s.run != nil {
		return s.run
	}
	return ghMergedPRs
}

// Events polls until ctx is cancelled, emitting one event per newly-merged PR.
func (s GitHubSource) Events(ctx context.Context) <-chan loop.Event {
	out := make(chan loop.Event)
	go func() {
		defer close(out)
		cursor := s.loadCursor()
		seeded := cursor > 0
		t := time.NewTicker(s.interval())
		defer t.Stop()
		for {
			prs, err := s.poll(ctx)
			if err != nil && ctx.Err() == nil {
				s.logf("github poll %s: %v", s.Repo, err)
			}
			// Ascending by number so the cursor advances monotonically and events
			// arrive in merge order.
			sort.Slice(prs, func(i, j int) bool { return prs[i].Number < prs[j].Number })
			for _, pr := range prs {
				if pr.Number <= cursor {
					continue
				}
				cursor = pr.Number
				if !seeded {
					continue // first run: adopt the head, don't fire for history
				}
				select {
				case <-ctx.Done():
					return
				case out <- s.event(pr):
				}
			}
			if !seeded {
				s.logf("github seeded %s@%s at PR #%d; watching for newer merges", s.Repo, s.base(), cursor)
				seeded = true
			}
			s.saveCursor(cursor)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return out
}

func (s GitHubSource) poll(ctx context.Context) ([]ghPR, error) {
	out, err := s.runner()(ctx, s.Repo, s.base())
	if err != nil {
		return nil, err
	}
	var prs []ghPR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("github: parse gh output: %w", err)
	}
	return prs, nil
}

func (s GitHubSource) event(pr ghPR) loop.Event {
	return loop.Event{
		Kind:   "github.merged",
		Source: "github",
		Payload: map[string]any{
			"repo":  s.Repo,
			"pr":    pr.Number,
			"title": pr.Title,
			"url":   pr.URL,
			"sha":   pr.MergeCommit.OID,
		},
		At: time.Now(),
	}
}

// loadCursor reads the last-emitted PR number; 0 (no cursor) means seed on first
// poll. A malformed file is treated as absent — the seed path is safe.
func (s GitHubSource) loadCursor() int {
	if s.CursorPath == "" {
		return 0
	}
	b, err := os.ReadFile(s.CursorPath)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func (s GitHubSource) saveCursor(n int) {
	if s.CursorPath == "" || n <= 0 {
		return
	}
	if err := os.WriteFile(s.CursorPath, []byte(strconv.Itoa(n)), 0o644); err != nil {
		s.logf("github: save cursor: %v", err)
	}
}

// ghMergedPRs asks gh for merged PRs against base, newest first.
func ghMergedPRs(ctx context.Context, repo, base string) ([]byte, error) {
	args := []string{
		"pr", "list", "--repo", repo, "--base", base, "--state", "merged",
		"--json", "number,title,url,mergedAt,mergeCommit", "--limit", "30",
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %w", err)
	}
	return out, nil
}
