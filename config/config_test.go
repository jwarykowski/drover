package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const sample = `
[[source]]
name  = "shepherd"
cmd   = ["drover", "source", "shepherd"]
types = ["shepherd.added", "shepherd.updated"]

[[source]]
name  = "remote"
http  = "127.0.0.1:9100"
types = ["remote.ping"]

[[runner]]
name  = "claude"
cmd   = ["claude", "-p", "{{prompt}}", "--permission-mode", "{{mode}}"]
modes = ["acceptEdits", "bypassPermissions"]

[[runner]]
name = "codex"
cmd  = ["codex", "exec", "--full-auto", "{{prompt}}"]

[[action]]
id     = "a1"
name   = "fix-ci"
on     = "github.pull_request.merged"
where  = { repo = "acme/api" }
runner = "claude"
mode   = "acceptEdits"
target = "~/src/acme-api"
do     = "fix it"
`

func load(t *testing.T, body string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "drover.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLoadReadsAllThreeTables(t *testing.T) {
	c := load(t, sample)
	if len(c.Sources()) != 2 || len(c.RunnerNames()) != 2 || len(c.Actions()) != 1 {
		t.Fatalf("tables not loaded: %d sources, %d runners, %d actions",
			len(c.Sources()), len(c.RunnerNames()), len(c.Actions()))
	}
	src := c.Sources()[0]
	if src.Name != "shepherd" || len(src.Cmd) != 3 || src.Cmd[0] != "drover" {
		t.Fatalf("exec source not loaded: %+v", src)
	}
	if c.Sources()[1].HTTP != "127.0.0.1:9100" {
		t.Fatalf("http source not loaded: %+v", c.Sources()[1])
	}
}

// A missing file is an empty config, not an error, so `drover action add` works
// on a machine that has never run drover.
func TestLoadMissingFileIsEmpty(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatalf("a missing config must not error: %v", err)
	}
	if len(c.Actions()) != 0 || len(c.Sources()) != 0 {
		t.Fatal("a missing config should load empty")
	}
}

func TestMatchFiltersOnTypeAndData(t *testing.T) {
	c := load(t, sample)
	data := map[string]string{"repo": "acme/api", "title": "x"}
	if got := c.Match("github.pull_request.merged", data); len(got) != 1 || got[0].ID != "a1" {
		t.Fatalf("want a1, got %+v", got)
	}
	if got := c.Match("github.pull_request.merged", map[string]string{"repo": "other/repo"}); len(got) != 0 {
		t.Fatalf("a mismatched filter must not match: %+v", got)
	}
	if got := c.Match("github.issues.opened", data); len(got) != 0 {
		t.Fatalf("a mismatched type must not match: %+v", got)
	}
}

func TestRunnerByName(t *testing.T) {
	c := load(t, sample)
	// An empty name takes the first runner, so a single-tool setup needs no
	// `runner =` on every action.
	g, err := c.RunnerByName("")
	if err != nil || g.Name != "claude" {
		t.Fatalf("empty name should take the first runner, got %q (%v)", g.Name, err)
	}
	if g, err := c.RunnerByName("codex"); err != nil || g.Cmd[0] != "codex" {
		t.Fatalf("codex not resolved: %+v (%v)", g, err)
	}
	if _, err := c.RunnerByName("ghost"); err == nil {
		t.Fatal("an unknown runner must error rather than fall back")
	}
	// No runners at all is an error, not a silent no-op run.
	if _, err := (&Config{}).RunnerByName(""); err == nil {
		t.Fatal("a config with no runners must error")
	}
}

// The action editor offers the types the installed sources declare, so a
// plugin's own event types appear without drover being rebuilt.
func TestTypesUnionsWhatSourcesDeclare(t *testing.T) {
	c := load(t, sample)
	want := []string{"remote.ping", "shepherd.added", "shepherd.updated"}
	if got := c.Types(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Types() = %v, want %v", got, want)
	}
}

// drover cannot know a third-party tool's permission vocabulary, so an agent
// that declares none accepts anything.
func TestValidModeOnlyChecksDeclaredModes(t *testing.T) {
	c := load(t, sample)
	if !c.ValidMode("claude", "acceptEdits") {
		t.Error("a declared mode must be accepted")
	}
	if c.ValidMode("claude", "yolo") {
		t.Error("an undeclared mode must be rejected when the agent declares a list")
	}
	if !c.ValidMode("codex", "anything") {
		t.Error("an agent declaring no modes must accept anything")
	}
}

func TestAddReplaceRemoveRoundTripThroughTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drover.toml")
	c, _ := Load(path)
	a, err := c.Add(Action{Name: "one", On: "demo.ping", Do: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == "" {
		t.Fatal("Add must assign an id")
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.ByID(a.ID)
	if !ok || got.Name != "one" {
		t.Fatalf("action did not survive a save/load: %+v", reloaded.Actions())
	}

	got.Name = "two"
	if err := reloaded.Replace(got); err != nil {
		t.Fatal(err)
	}
	if again, _ := reloaded.ByID(a.ID); again.Name != "two" {
		t.Fatalf("Replace did not update in place: %+v", again)
	}
	if len(reloaded.Actions()) != 1 {
		t.Fatalf("Replace must not append a duplicate: %+v", reloaded.Actions())
	}

	if err := reloaded.Remove(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.ByID(a.ID); ok {
		t.Fatal("Remove did not drop the action")
	}
	if err := reloaded.Remove("ghost"); err == nil {
		t.Fatal("removing an unknown id must error")
	}
}

// Reload is what makes `drover action add` take effect in a running daemon.
func TestReloadPicksUpFileChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drover.toml")
	if err := os.WriteFile(path, []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Actions()) != 1 {
		t.Fatalf("want 1 action, got %d", len(c.Actions()))
	}
	extra := sample + "\n[[action]]\nid = \"a2\"\nname = \"triage\"\non = \"demo.ping\"\ndo = \"y\"\n"
	if err := os.WriteFile(path, []byte(extra), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Reload(path); err != nil {
		t.Fatal(err)
	}
	if len(c.Actions()) != 2 {
		t.Fatalf("reload did not pick up the new action: %+v", c.Actions())
	}
}

// Risky is what the CLI and the TUI both warn on, so the rule lives here rather
// than being restated per authoring path.
func TestRiskyIsAutoPlusAPermissionWaivingMode(t *testing.T) {
	if !(Action{Auto: true, Mode: "bypassPermissions"}).Risky() {
		t.Error("auto + bypassPermissions must be flagged")
	}
	if (Action{Auto: false, Mode: "bypassPermissions"}).Risky() {
		t.Error("a gated action is not risky — a human sees it first")
	}
	if (Action{Auto: true, Mode: "acceptEdits"}).Risky() {
		t.Error("auto alone with a prompting mode is not the flagged case")
	}
}

// A source row is fully described by its own fields, so writing the same name
// twice replaces rather than appending — otherwise installing a preset twice
// would leave two rows racing for the same port. The new types must also reach
// Types(), since that is what the action editor offers.
func TestUpsertSourceIsIdempotent(t *testing.T) {
	c := &Config{}
	row := Source{Name: "sentry", Cmd: []string{"drover", "source", "webhook"}, Types: []string{"sentry.issue.created"}}
	if err := c.UpsertSource(row); err != nil {
		t.Fatal(err)
	}
	row.Types = append(row.Types, "sentry.issue.resolved")
	if err := c.UpsertSource(row); err != nil {
		t.Fatal(err)
	}
	if got := c.Sources(); len(got) != 1 {
		t.Fatalf("upsert appended instead of replacing: %+v", got)
	}
	if got := c.Types(); !reflect.DeepEqual(got, []string{"sentry.issue.created", "sentry.issue.resolved"}) {
		t.Fatalf("Types() did not pick up the replacement: %v", got)
	}
}

// Removing a source has to drop that row and leave the rest: a feed that installs
// one row per repo is the reason it exists.
func TestRemoveSource(t *testing.T) {
	c := &Config{}
	for _, n := range []string{"github/acme/api", "github/acme/web"} {
		if err := c.UpsertSource(Source{Name: n, Cmd: []string{"drover", "source", "github"}, Types: []string{"github.issues.opened"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.RemoveSource("github/acme/api"); err != nil {
		t.Fatal(err)
	}
	if got := c.Sources(); len(got) != 1 || got[0].Name != "github/acme/web" {
		t.Fatalf("RemoveSource left %+v", got)
	}
	if err := c.RemoveSource("github/acme/api"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removing an absent row: want ErrNotFound, got %v", err)
	}
}

// A row with both transports or neither is skipped by daemon.build, so it is
// rejected at the write instead of silently never being spawned.
func TestUpsertSourceRequiresExactlyOneTransport(t *testing.T) {
	c := &Config{}
	for _, s := range []Source{
		{Name: "", Cmd: []string{"x"}},
		{Name: "both", Cmd: []string{"x"}, HTTP: "127.0.0.1:9100"},
		{Name: "neither"},
	} {
		if err := c.UpsertSource(s); err == nil {
			t.Fatalf("want an error for %+v", s)
		}
	}
	if len(c.Sources()) != 0 {
		t.Fatalf("a rejected row must not be written: %+v", c.Sources())
	}
}

// Runner CRUD mirrors action CRUD: add (unique by name), replace, remove.
func TestRunnerCRUD(t *testing.T) {
	c := &Config{}
	if err := c.AddRunner(Runner{Name: "claude", Cmd: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddRunner(Runner{Name: "claude"}); err == nil {
		t.Fatal("duplicate runner name must error")
	}
	if err := c.ReplaceRunner(Runner{Name: "claude", Modes: []string{"acceptEdits"}}); err != nil {
		t.Fatal(err)
	}
	if g, _ := c.RunnerByName("claude"); len(g.Modes) != 1 {
		t.Fatalf("replace did not take: %+v", g)
	}
	if err := c.RemoveRunner("claude"); err != nil {
		t.Fatal(err)
	}
	if len(c.Runners()) != 0 {
		t.Fatalf("remove left runners: %+v", c.Runners())
	}
	if err := c.RemoveRunner("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
