package source

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"time"

	"github.com/jwarykowski/drover/loop"
)

// GitHubSource senses merges to a branch by polling merged pull requests through
// the gh CLI (already authenticated — no token handling, no inbound webhook).
// It emits one github.pull_request.merged event per merged PR every poll; the
// Dedup decorator keyed on Event.ID drops repeats, so this source carries no
// cursor of its own. Cold-start "don't fire history" is a one-time Seen preload
// via SeedIDs in the wiring.
type GitHubSource struct {
	Repo     string        // owner/name
	Base     string        // target branch; defaults to "master"
	Interval time.Duration // poll interval; defaults to 60s
	Logf     func(string, ...any)
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

// Events polls until ctx is cancelled, emitting one event per merged PR seen.
func (s GitHubSource) Events(ctx context.Context) <-chan loop.Event {
	out := make(chan loop.Event)
	go func() {
		defer close(out)
		t := time.NewTicker(s.interval())
		defer t.Stop()
		for {
			evs, err := s.poll(ctx)
			if err != nil && ctx.Err() == nil {
				s.logf("github poll %s: %v", s.Repo, err)
			}
			for _, e := range evs {
				select {
				case <-ctx.Done():
					return
				case out <- e:
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

// SeedIDs returns the IDs of currently-merged PRs without emitting them, so a
// cold start can adopt the head into Seen instead of firing for history.
func (s GitHubSource) SeedIDs(ctx context.Context) ([]string, error) {
	evs, err := s.poll(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(evs))
	for i, e := range evs {
		ids[i] = e.ID
	}
	return ids, nil
}

func (s GitHubSource) poll(ctx context.Context) ([]loop.Event, error) {
	raw, err := s.runner()(ctx, s.Repo, s.base())
	if err != nil {
		return nil, err
	}
	return decodeGitHubPRs(s.Repo, raw)
}

// decodeGitHubPRs normalises `gh pr list --state merged` JSON into merged-PR
// events, ascending by number so IDs advance in merge order.
func decodeGitHubPRs(repo string, raw []byte) ([]loop.Event, error) {
	var prs []ghPR
	if err := json.Unmarshal(raw, &prs); err != nil {
		return nil, fmt.Errorf("github: parse gh output: %w", err)
	}
	sort.Slice(prs, func(i, j int) bool { return prs[i].Number < prs[j].Number })
	var evs []loop.Event
	for _, pr := range prs {
		if pr.MergedAt == "" {
			continue
		}
		evs = append(evs, loop.Event{
			ID:     fmt.Sprintf("github/%s:pr:%d:merged", repo, pr.Number),
			Type:   "github.pull_request.merged",
			Source: "github/" + repo,
			Data: loop.Signal{
				Repo:  repo,
				Title: pr.Title,
				URL:   pr.URL,
				Key:   fmt.Sprintf("pr:%d:merged", pr.Number),
				Extra: map[string]any{"pr": pr.Number, "sha": pr.MergeCommit.OID},
			},
			At: time.Now(),
		})
	}
	return evs, nil
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
