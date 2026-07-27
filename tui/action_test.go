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
	gotOns := subactionOns("github")
	wantOns := []string{
		"github.pull_request.opened",
		"github.pull_request.closed",
		"github.pull_request.merged",
		"github.issues.opened",
	}
	if !reflect.DeepEqual(gotOns, wantOns) {
		t.Fatalf("subactionOns(github) = %#v, want %#v", gotOns, wantOns)
	}
	gotLabels := subactionLabels("github")
	wantLabels := []string{"pull request opened", "pull request closed", "pull request merged", "issues opened"}
	if !reflect.DeepEqual(gotLabels, wantLabels) {
		t.Fatalf("subactionLabels(github) = %#v, want %#v", gotLabels, wantLabels)
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
