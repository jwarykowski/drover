package tui

import (
	"reflect"
	"testing"

	"github.com/jwarykowski/drover/registry"
)

func TestKinds(t *testing.T) {
	got := kinds()
	want := []string{"github", "sentry", "board"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds() = %v, want %v", got, want)
	}
}

func TestSubactionsGithub(t *testing.T) {
	got := subactions("github")
	want := []subaction{
		{label: "pull request opened", on: "github.pull_request.opened"},
		{label: "pull request closed", on: "github.pull_request.closed"},
		{label: "pull request merged", on: "github.pull_request.merged"},
		{label: "issues opened", on: "github.issues.opened"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("subactions(github) = %#v, want %#v", got, want)
	}
}

func TestLabel(t *testing.T) {
	cases := map[string]string{
		"github.pull_request.merged": "pull request merged",
		"sentry.issue.opened":        "issue opened",
		"board.archived":             "archived",
	}
	for on, want := range cases {
		if got := label(on); got != want {
			t.Errorf("label(%q) = %q, want %q", on, got, want)
		}
	}
}

func TestDefaultPromptCoversEveryKnownType(t *testing.T) {
	for _, on := range registry.KnownEventTypes {
		if defaultPrompt(on) == "" {
			t.Errorf("defaultPrompt(%q) is empty; every known event type should seed a prompt", on)
		}
	}
}

func TestToFromActionRoundTrip(t *testing.T) {
	a := registry.Action{
		Name:     "fix-ci",
		On:       "github.pull_request.merged",
		Repo:     "acme/api",
		Base:     "main",
		Source:   "poll",
		Interval: "30s",
		Target:   "~/src/acme-api",
		Mode:     "acceptEdits",
		Do:       "A PR merged. If CI is red, open a fix PR.",
	}
	if got := toAction(fromAction(a)); !reflect.DeepEqual(got, a) {
		t.Fatalf("round-trip = %#v, want %#v", got, a)
	}
}
