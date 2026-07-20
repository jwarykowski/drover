package registry

import (
	"path/filepath"
	"testing"
)

// TestReloadConcurrentWithMatch exercises the mutex under -race: the daemon
// reloads the registry from the sensing goroutine while workers resolve ids.
func TestReloadConcurrentWithMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.toml")
	reg := &Registry{Path: path}
	reg.Add(Action{Name: "x", On: "github.issues.opened", Target: "t"})
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			_ = reg.Reload(path)
		}
		close(done)
	}()
	for i := 0; i < 200; i++ {
		_ = reg.Match("github.issues.opened", "")
		_, _ = reg.ByID("nope")
	}
	<-done
}

func TestRoundTripAndMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "actions.toml")
	reg, err := Load(path) // missing file → empty registry, not an error
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Actions) != 0 {
		t.Fatalf("fresh registry not empty: %d", len(reg.Actions))
	}

	a1, _ := reg.Add(Action{Name: "sdk-bump", On: "github.pull_request.merged", Repo: "acme/api", Target: "~/x", Mode: "acceptEdits", Do: "do it"})
	a2, _ := reg.Add(Action{Name: "triage", On: "sentry.issue.opened", Target: "~/y", Mode: "default", Do: "triage"})
	if a1.ID == "" || a1.ID == a2.ID {
		t.Fatalf("ids not unique/stable: %q %q", a1.ID, a2.ID)
	}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}

	reg2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reg2.Actions) != 2 {
		t.Fatalf("want 2 after reload, got %d", len(reg2.Actions))
	}
	got, ok := reg2.ByID(a1.ID)
	if !ok || got.Do != "do it" || got.ID != a1.ID {
		t.Fatalf("ByID lost data: %+v", got)
	}

	// Match by type; repo filter narrows.
	if m := reg2.Match("github.pull_request.merged", "acme/api"); len(m) != 1 || m[0].ID != a1.ID {
		t.Fatalf("match wrong: %+v", m)
	}
	if m := reg2.Match("github.pull_request.merged", "other/repo"); len(m) != 0 {
		t.Fatalf("repo filter not applied: %+v", m)
	}
	// Empty repo filter matches any repo.
	if m := reg2.Match("sentry.issue.opened", "whatever"); len(m) != 1 {
		t.Fatalf("empty repo filter should match: %+v", m)
	}
}

func TestRemove(t *testing.T) {
	reg := &Registry{Path: filepath.Join(t.TempDir(), "a.toml")}
	a, _ := reg.Add(Action{Name: "x", On: "github.issues.opened", Target: "t"})
	if err := reg.Remove(a.ID); err != nil {
		t.Fatal(err)
	}
	if err := reg.Remove(a.ID); err == nil {
		t.Fatal("want ErrNotFound on second remove")
	}
}

func TestValidation(t *testing.T) {
	if !ValidType("github.pull_request.merged") || ValidType("nope.nope") {
		t.Fatal("ValidType wrong")
	}
	if !ValidMode("acceptEdits") || ValidMode("yolo") {
		t.Fatal("ValidMode wrong")
	}
}
