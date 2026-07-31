package main

import (
	"testing"
)

func TestDecodeGitHubPRsEmitsMergedOnly(t *testing.T) {
	raw := []byte(`[
	  {"number":9,"title":"later","url":"https://github.com/acme/api/pull/9","mergedAt":"2026-07-30T10:00:00Z"},
	  {"number":7,"title":"earlier","url":"https://github.com/acme/api/pull/7","mergedAt":"2026-07-30T09:00:00Z"},
	  {"number":8,"title":"still open","url":"https://github.com/acme/api/pull/8","mergedAt":""}
	]`)
	evs, err := decodeGitHubPRs("acme/api", raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("unmerged PRs must not fire: got %d events", len(evs))
	}
	// Ascending by number, so ids advance in merge order.
	if evs[0].ID != "github/acme/api:pr:7:merged" || evs[1].ID != "github/acme/api:pr:9:merged" {
		t.Fatalf("not ordered by number: %s, %s", evs[0].ID, evs[1].ID)
	}
	d := evs[0].Data
	if d["repo"] != "acme/api" || d["title"] != "earlier" {
		t.Fatalf("data not carried: %v", d)
	}
	// The url is the stable handle for a PR, so it doubles as the subject: one
	// run per PR per action while that run is in flight.
	if d["subject"] != d["url"] {
		t.Fatalf("subject should be the PR url: %v", d)
	}
}

func TestDecodeGitHubWebhookMergeVsClose(t *testing.T) {
	body := func(action string, merged bool) []byte {
		m := "false"
		if merged {
			m = "true"
		}
		return []byte(`{"action":"` + action + `","pull_request":{"number":3,"title":"t","html_url":"https://github.com/acme/api/pull/3","merged":` + m + `},"repository":{"full_name":"acme/api"}}`)
	}

	evs, err := decodeGitHubWebhook("pull_request", body("closed", true))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != "github.pull_request.merged" {
		t.Fatalf("closed+merged must read as a merge: %+v", evs)
	}

	evs, _ = decodeGitHubWebhook("pull_request", body("closed", false))
	if len(evs) != 1 || evs[0].Type != "github.pull_request.closed" {
		t.Fatalf("closed without merge must stay closed: %+v", evs)
	}

	// An action drover has no event type for produces nothing, rather than an
	// event no action could ever match.
	evs, _ = decodeGitHubWebhook("pull_request", body("synchronize", false))
	if len(evs) != 0 {
		t.Fatalf("unhandled action must produce nothing: %+v", evs)
	}

	// An unknown delivery kind is ignored outright.
	evs, _ = decodeGitHubWebhook("deployment", []byte(`{}`))
	if len(evs) != 0 {
		t.Fatalf("unknown event kind must produce nothing: %+v", evs)
	}
}

func TestDecodeGitHubWebhookIssuesOpened(t *testing.T) {
	raw := []byte(`{"action":"opened","issue":{"number":4,"title":"boom","html_url":"https://github.com/acme/api/issues/4"},"repository":{"full_name":"acme/api"}}`)
	evs, err := decodeGitHubWebhook("issues", raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != "github.issues.opened" {
		t.Fatalf("issue open not decoded: %+v", evs)
	}
	if evs[0].Data["subject"] != "https://github.com/acme/api/issues/4" {
		t.Fatalf("subject should be the issue url: %v", evs[0].Data)
	}
}

func TestDecodeGitHubWebhookIssueComment(t *testing.T) {
	body := []byte(`{"action":"created",
	  "issue":{"number":42,"html_url":"https://github.com/acme/api/issues/42","pull_request":{}},
	  "comment":{"id":99,"body":"please fix","html_url":"https://github.com/acme/api/issues/42#c99","user":{"login":"alice"}},
	  "repository":{"full_name":"acme/api"}}`)
	evs, err := decodeGitHubWebhook("issue_comment", body)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != "github.issue_comment.created" {
		t.Fatalf("want one comment event: %+v", evs)
	}
	d := evs[0].Data
	if d["author"] != "alice" || d["body"] != "please fix" || d["on_pr"] != "true" {
		t.Fatalf("comment data wrong: %v", d)
	}
	if evs[0].ID != "github/acme/api:comment:99" {
		t.Fatalf("comment id keys dedup: %s", evs[0].ID)
	}
	// non-created comment actions produce nothing
	if evs, _ := decodeGitHubWebhook("issue_comment", []byte(`{"action":"edited","repository":{"full_name":"acme/api"}}`)); len(evs) != 0 {
		t.Fatalf("only created fires: %+v", evs)
	}
}

func TestDecodeGitHubWebhookCheckSuite(t *testing.T) {
	body := func(action, concl string) []byte {
		return []byte(`{"action":"` + action + `","check_suite":{"conclusion":"` + concl + `","head_branch":"main","head_sha":"abc123"},"repository":{"full_name":"acme/api","html_url":"https://github.com/acme/api"}}`)
	}
	evs, err := decodeGitHubWebhook("check_suite", body("completed", "failure"))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != "github.check_suite.completed" {
		t.Fatalf("want one check_suite event: %+v", evs)
	}
	d := evs[0].Data
	if d["conclusion"] != "failure" || d["branch"] != "main" || d["sha"] != "abc123" {
		t.Fatalf("check_suite data wrong: %v", d)
	}
	if d["url"] != "https://github.com/acme/api/commit/abc123" {
		t.Fatalf("commit url wrong: %v", d)
	}
	// conclusion is in the id so a re-run's different verdict is a new event
	if evs2, _ := decodeGitHubWebhook("check_suite", body("completed", "success")); evs2[0].ID == evs[0].ID {
		t.Fatal("differing conclusion must produce a distinct id")
	}
	// only completed fires
	if evs, _ := decodeGitHubWebhook("check_suite", body("requested", "")); len(evs) != 0 {
		t.Fatalf("only completed fires: %+v", evs)
	}
}
