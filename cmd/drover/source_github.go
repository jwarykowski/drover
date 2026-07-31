package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jwarykowski/drover/loop"
	"github.com/jwarykowski/drover/source"
)

// `drover source github` is a source plugin like any other: it writes NDJSON
// events to stdout and knows nothing about the rest of drover. It ships in this
// binary only for convenience — the daemon spawns it through the same
// ExecSource a third-party plugin gets, so GitHub has no privileged path in.
//
//	[[source]]
//	name = "acme-api"
//	cmd  = ["drover", "source", "github", "--repo", "acme/api"]
//
// Two modes. `poll` shells `gh pr list` on a ticker (no inbound port, no token
// handling — gh's own auth is reused). `forward` runs `gh webhook forward`,
// which relays deliveries over GitHub's websocket to a localhost receiver.
func sourceGitHub(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("source github", flag.ContinueOnError)
	repo := fs.String("repo", "", "owner/name (required)")
	base := fs.String("base", "master", "branch merges are sensed against (poll mode)")
	mode := fs.String("mode", "poll", "poll | forward")
	interval := fs.Duration("interval", time.Minute, "poll interval")
	addr := fs.String("addr", "127.0.0.1:9099", "local receiver bind (forward mode)")
	seed := fs.Bool("seed", true, "adopt already-merged PRs at startup instead of firing them")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *repo == "" {
		return fmt.Errorf("source github: --repo is required")
	}

	out := &lineWriter{w: os.Stdout}
	switch *mode {
	case "poll":
		return pollGitHub(ctx, out, *repo, *base, *interval, *seed)
	case "forward":
		return forwardGitHub(ctx, out, *repo, *addr)
	default:
		return fmt.Errorf("source github: unknown mode %q (want poll or forward)", *mode)
	}
}

// lineWriter serialises event writes; forward mode emits from HTTP handler
// goroutines, and a torn line is an unparsable event.
type lineWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lineWriter) emit(e loop.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := source.Write(l.w, e); err != nil {
		fmt.Fprintf(os.Stderr, "emit %s: %v\n", e.ID, err)
	}
}

// pollGitHub emits one event per merged PR each tick. It keeps no cursor —
// drover's Dedup drops repeats by event id — so the only state here is the
// startup adoption below.
//
// --seed suppresses the first poll so a fresh setup does not fire for every PR
// merged before drover existed. The ceiling: a PR merged while this process is
// restarting is adopted rather than fired. Persist a cursor here if that gap
// ever matters.
func pollGitHub(ctx context.Context, out *lineWriter, repo, base string, interval time.Duration, seed bool) error {
	adopted := map[string]bool{}
	first := true
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		evs, err := ghPoll(ctx, repo, base)
		if err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "github poll %s: %v\n", repo, err)
		}
		for _, e := range evs {
			if first && seed {
				adopted[e.ID] = true
				continue
			}
			if adopted[e.ID] {
				continue
			}
			out.emit(e)
		}
		if first && seed {
			fmt.Fprintf(os.Stderr, "adopted %d merged PR(s) at head for %s; not firing history\n", len(adopted), repo)
		}
		first = false
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// forwardGitHub serves a localhost receiver and runs `gh webhook forward` to
// relay deliveries into it. Localhost-only, so no TLS and no HMAC verification;
// add both before binding this anywhere reachable.
func forwardGitHub(ctx context.Context, out *lineWriter, repo, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read", http.StatusBadRequest)
			return
		}
		evs, err := decodeGitHubWebhook(r.Header.Get("X-GitHub-Event"), body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "webhook decode: %v\n", err)
		}
		for _, e := range evs {
			out.emit(e)
		}
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	go runGHForward(ctx, repo, addr)

	fmt.Fprintf(os.Stderr, "webhook listening on %s for %s\n", addr, repo)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// runGHForward spawns `gh webhook forward`, restarting it if it drops.
func runGHForward(ctx context.Context, repo, addr string) {
	url := "http://" + addr + "/hook"
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, "gh", "webhook", "forward",
			"--repo="+repo, "--events=pull_request,issues,issue_comment,check_suite", "--url="+url)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "gh webhook forward ended (%v); retrying in 2s\n", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
}

// putIf sets k only when v is non-empty, so the event CONTEXT block (and corral's
// field chips) stay free of blank keys.
func putIf(m map[string]string, k, v string) {
	if v != "" {
		m[k] = v
	}
}

// truncBody one-lines and caps a body so a long description doesn't dominate the
// prompt; the agent can always fetch the full text from the url.
func truncBody(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 500 {
		s = s[:500] + " …"
	}
	return s
}

// prEvent builds a pull_request event with the recommended field set, shared by
// the poll and webhook paths so both carry identical Data keys.
func prEvent(repo, verb string, number int, title, url, author, base, head, labels, body string) loop.Event {
	data := map[string]string{"repo": repo, "number": strconv.Itoa(number), "title": title, "url": url, "subject": url}
	putIf(data, "author", author)
	putIf(data, "base", base)
	putIf(data, "head", head)
	putIf(data, "labels", labels)
	putIf(data, "body", truncBody(body))
	return loop.Event{
		ID:     fmt.Sprintf("github/%s:pr:%d:%s", repo, number, verb),
		Type:   "github.pull_request." + verb,
		Source: "github/" + repo,
		Data:   data,
		At:     time.Now(),
	}
}

// issueEvent builds a github.issues.<action> event.
func issueEvent(repo, action string, number int, title, url, author, labels, body string) loop.Event {
	data := map[string]string{"repo": repo, "number": strconv.Itoa(number), "title": title, "url": url, "subject": url}
	putIf(data, "author", author)
	putIf(data, "labels", labels)
	putIf(data, "body", truncBody(body))
	return loop.Event{
		ID:     fmt.Sprintf("github/%s:issue:%d:%s", repo, number, action),
		Type:   "github.issues." + action,
		Source: "github/" + repo,
		Data:   data,
		At:     time.Now(),
	}
}

// ghPR is the slice of `gh pr list --json` this shim reads.
type ghPR struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	MergedAt    string `json:"mergedAt"`
	Body        string `json:"body"`
	BaseRefName string `json:"baseRefName"`
	HeadRefName string `json:"headRefName"`
	Author      struct {
		Login string `json:"login"`
	} `json:"author"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func ghPoll(ctx context.Context, repo, base string) ([]loop.Event, error) {
	raw, err := ghMergedPRs(ctx, repo, base)
	if err != nil {
		return nil, err
	}
	return decodeGitHubPRs(repo, raw)
}

// decodeGitHubPRs normalises `gh pr list --state merged` JSON into merged-PR
// events, ascending by number so ids advance in merge order.
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
		names := make([]string, len(pr.Labels))
		for i, l := range pr.Labels {
			names[i] = l.Name
		}
		evs = append(evs, prEvent(repo, "merged", pr.Number, pr.Title, pr.URL,
			pr.Author.Login, pr.BaseRefName, pr.HeadRefName, strings.Join(names, ","), pr.Body))
	}
	return evs, nil
}

// decodeGitHubWebhook normalises a forwarded delivery (identified by the
// X-GitHub-Event header) into events. A PR closed with merged=true is a merge;
// unhandled actions produce nothing.
func decodeGitHubWebhook(event string, raw []byte) ([]loop.Event, error) {
	switch event {
	case "pull_request":
		var p struct {
			Action string `json:"action"`
			PR     struct {
				Number  int    `json:"number"`
				Title   string `json:"title"`
				HTMLURL string `json:"html_url"`
				Merged  bool   `json:"merged"`
				Body    string `json:"body"`
				User    struct {
					Login string `json:"login"`
				} `json:"user"`
				Base struct {
					Ref string `json:"ref"`
				} `json:"base"`
				Head struct {
					Ref string `json:"ref"`
				} `json:"head"`
				Labels []struct {
					Name string `json:"name"`
				} `json:"labels"`
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
		if verb != "opened" && verb != "closed" && verb != "merged" && verb != "ready_for_review" {
			return nil, nil
		}
		names := make([]string, len(p.PR.Labels))
		for i, l := range p.PR.Labels {
			names[i] = l.Name
		}
		return []loop.Event{prEvent(p.Repo.FullName, verb, p.PR.Number, p.PR.Title, p.PR.HTMLURL,
			p.PR.User.Login, p.PR.Base.Ref, p.PR.Head.Ref, strings.Join(names, ","), p.PR.Body)}, nil

	case "issues":
		var p struct {
			Action string `json:"action"`
			Issue  struct {
				Number  int    `json:"number"`
				Title   string `json:"title"`
				HTMLURL string `json:"html_url"`
				Body    string `json:"body"`
				User    struct {
					Login string `json:"login"`
				} `json:"user"`
				Labels []struct {
					Name string `json:"name"`
				} `json:"labels"`
			} `json:"issue"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("webhook: parse issues: %w", err)
		}
		if p.Action != "opened" && p.Action != "labeled" {
			return nil, nil
		}
		names := make([]string, len(p.Issue.Labels))
		for i, l := range p.Issue.Labels {
			names[i] = l.Name
		}
		return []loop.Event{issueEvent(p.Repo.FullName, p.Action, p.Issue.Number, p.Issue.Title,
			p.Issue.HTMLURL, p.Issue.User.Login, strings.Join(names, ","), p.Issue.Body)}, nil

	case "issue_comment":
		var p struct {
			Action string `json:"action"`
			Issue  struct {
				Number      int       `json:"number"`
				HTMLURL     string    `json:"html_url"`
				PullRequest *struct{} `json:"pull_request"` // present iff the comment is on a PR
			} `json:"issue"`
			Comment struct {
				ID      int64  `json:"id"`
				Body    string `json:"body"`
				HTMLURL string `json:"html_url"`
				User    struct {
					Login string `json:"login"`
				} `json:"user"`
			} `json:"comment"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("webhook: parse issue_comment: %w", err)
		}
		if p.Action != "created" {
			return nil, nil
		}
		repo := p.Repo.FullName
		data := map[string]string{
			"repo":    repo,
			"number":  strconv.Itoa(p.Issue.Number),
			"url":     p.Comment.HTMLURL,
			"subject": p.Issue.HTMLURL, // one run per issue/PR thread
			"on_pr":   strconv.FormatBool(p.Issue.PullRequest != nil),
		}
		putIf(data, "author", p.Comment.User.Login)
		putIf(data, "body", truncBody(p.Comment.Body))
		return []loop.Event{{
			ID:     fmt.Sprintf("github/%s:comment:%d", repo, p.Comment.ID),
			Type:   "github.issue_comment.created",
			Source: "github/" + repo,
			Data:   data,
			At:     time.Now(),
		}}, nil

	case "check_suite":
		var p struct {
			Action     string `json:"action"`
			CheckSuite struct {
				Conclusion string `json:"conclusion"`
				HeadBranch string `json:"head_branch"`
				HeadSHA    string `json:"head_sha"`
			} `json:"check_suite"`
			Repo struct {
				FullName string `json:"full_name"`
				HTMLURL  string `json:"html_url"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("webhook: parse check_suite: %w", err)
		}
		if p.Action != "completed" {
			return nil, nil
		}
		repo := p.Repo.FullName
		cs := p.CheckSuite
		data := map[string]string{
			"repo":    repo,
			"subject": cs.HeadSHA, // one run per commit's checks
		}
		putIf(data, "conclusion", cs.Conclusion) // success | failure | cancelled | …
		putIf(data, "branch", cs.HeadBranch)
		putIf(data, "sha", cs.HeadSHA)
		putIf(data, "url", p.Repo.HTMLURL+"/commit/"+cs.HeadSHA)
		return []loop.Event{{
			// conclusion in the id so a re-run's differing verdict fires again
			ID:     fmt.Sprintf("github/%s:check_suite:%s:%s", repo, cs.HeadSHA, cs.Conclusion),
			Type:   "github.check_suite.completed",
			Source: "github/" + repo,
			Data:   data,
			At:     time.Now(),
		}}, nil
	}
	return nil, nil
}

// ghMergedPRs asks gh for merged PRs against base.
func ghMergedPRs(ctx context.Context, repo, base string) ([]byte, error) {
	args := []string{
		"pr", "list", "--repo", repo, "--base", base, "--state", "merged",
		"--json", "number,title,url,mergedAt,body,author,baseRefName,headRefName,labels", "--limit", "30",
	}
	out, err := exec.CommandContext(ctx, "gh", args...).Output()
	if err != nil {
		// gh puts the useful part ("could not resolve to a Repository") on
		// stderr, which Output captures but does not include in the error.
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("gh pr list: %w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("gh pr list: %w", err)
	}
	return out, nil
}
