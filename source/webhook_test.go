package source

import (
	"testing"

	"github.com/jwarykowski/drover/loop"
)

func TestDecodeGitHubWebhookMerged(t *testing.T) {
	body := []byte(`{"action":"closed","pull_request":{"number":42,"title":"bump","html_url":"https://x/pr/42","merged":true,"merge_commit_sha":"abc"},"repository":{"full_name":"acme/api"}}`)
	evs, err := decodeGitHubWebhook("pull_request", body)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	if evs[0].Type != "github.pull_request.merged" || evs[0].ID != "github/acme/api:pr:42:merged" {
		t.Fatalf("wrong envelope: %+v", evs[0])
	}
	sig, ok := evs[0].Data.(loop.Signal)
	if !ok || sig.Title != "bump" || sig.URL != "https://x/pr/42" {
		t.Fatalf("signal not populated: %#v", evs[0].Data)
	}
}

func TestDecodeGitHubWebhookClosedNotMerged(t *testing.T) {
	body := []byte(`{"action":"closed","pull_request":{"number":42,"merged":false},"repository":{"full_name":"acme/api"}}`)
	evs, _ := decodeGitHubWebhook("pull_request", body)
	if len(evs) != 1 || evs[0].Type != "github.pull_request.closed" {
		t.Fatalf("want a closed (not merged) event, got %+v", evs)
	}
}

func TestDecodeGitHubWebhookIssues(t *testing.T) {
	body := []byte(`{"action":"opened","issue":{"number":7,"title":"bug","html_url":"u"},"repository":{"full_name":"acme/api"}}`)
	evs, _ := decodeGitHubWebhook("issues", body)
	if len(evs) != 1 || evs[0].Type != "github.issues.opened" {
		t.Fatalf("want issues.opened, got %+v", evs)
	}
	closed := []byte(`{"action":"closed","issue":{"number":7},"repository":{"full_name":"acme/api"}}`)
	if evs2, _ := decodeGitHubWebhook("issues", closed); len(evs2) != 0 {
		t.Fatalf("a closed issue should be ignored, got %+v", evs2)
	}
}
