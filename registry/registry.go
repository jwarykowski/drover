// Package registry is drover's trusted store of agent actions: the mapping from
// an event (type [+ repo]) to what an agent should do. It is drover-owned and
// authored through the `drover action` CLI — NEVER a shepherd board, whose file
// syncs and is hand-editable. The board only ever carries an action's stable id
// (a reference); the prompt, target and permission mode resolve here, so board
// content can at most select an allowlisted action, never introduce one.
package registry

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

// Action is one row: on this event, run an agent in target with this prompt.
type Action struct {
	ID     string `toml:"id"`             // stable key the board references
	Name   string `toml:"name"`           // friendly label, editable
	On     string `toml:"on"`             // event Type to match
	Repo   string `toml:"repo,omitempty"` // optional source filter (owner/name)
	Target string `toml:"target"`         // cwd the agent runs in
	Mode   string `toml:"mode"`           // claude permission mode
	Do     string `toml:"do"`             // the prompt body
}

// Registry is the loaded action set plus the path it came from. mu guards
// Actions so the watch daemon can Reload it (from the sensing goroutine) while
// agent workers resolve ids concurrently.
type Registry struct {
	Path    string
	mu      sync.RWMutex
	Actions []Action
}

// Reload swaps Actions for a fresh read of path, under the write lock, so
// `drover action add|edit|rm` take effect in a running daemon without a race.
func (r *Registry) Reload(path string) error {
	fresh, err := Load(path)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.Actions = fresh.Actions
	r.mu.Unlock()
	return nil
}

type file struct {
	Action []Action `toml:"action"`
}

// KnownEventTypes are the event types an action may listen on. Kept here so the
// CLI can reject typos at author time rather than silently never matching.
var KnownEventTypes = []string{
	"github.pull_request.opened",
	"github.pull_request.closed",
	"github.pull_request.merged",
	"github.issues.opened",
	"sentry.issue.opened",
}

// ValidModes are the claude permission modes an action may request.
var ValidModes = []string{"default", "acceptEdits", "plan", "bypassPermissions"}

var ErrNotFound = errors.New("registry: action not found")

// Load reads the registry from path. A missing file is an empty registry (so
// `action add` works on first run), not an error.
func Load(path string) (*Registry, error) {
	r := &Registry{Path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, fmt.Errorf("registry: read %s: %w", path, err)
	}
	var f file
	if err := toml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("registry: parse %s: %w", path, err)
	}
	r.Actions = f.Action
	return r, nil
}

// Save writes the registry back to its path (creating parent dirs).
func (r *Registry) Save() error {
	if err := os.MkdirAll(filepath.Dir(r.Path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(file{Action: r.Actions}); err != nil {
		return fmt.Errorf("registry: encode: %w", err)
	}
	return os.WriteFile(r.Path, buf.Bytes(), 0o644)
}

// Match returns the actions listening for evType whose repo filter is empty or
// equals repo.
func (r *Registry) Match(evType, repo string) []Action {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Action
	for _, a := range r.Actions {
		if a.On == evType && (a.Repo == "" || a.Repo == repo) {
			out = append(out, a)
		}
	}
	return out
}

// ByID resolves a fired task's action reference back to its row.
func (r *Registry) ByID(id string) (Action, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, a := range r.Actions {
		if a.ID == id {
			return a, true
		}
	}
	return Action{}, false
}

// Add assigns a fresh id and appends the action.
func (r *Registry) Add(a Action) (Action, error) {
	if a.ID == "" {
		a.ID = NewID()
	}
	r.Actions = append(r.Actions, a)
	return a, nil
}

// Remove drops the action with id; ErrNotFound if absent.
func (r *Registry) Remove(id string) error {
	for i, a := range r.Actions {
		if a.ID == id {
			r.Actions = append(r.Actions[:i], r.Actions[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrNotFound, id)
}

// NewID is a short, stable, lowercase-hex id so it round-trips through
// shepherd's `action:` token (which lowercases its value).
func NewID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ValidType reports whether t is a known event type.
func ValidType(t string) bool { return contains(KnownEventTypes, t) }

// ValidMode reports whether m is a permitted claude permission mode.
func ValidMode(m string) bool { return contains(ValidModes, m) }

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// DefaultPath is where the registry lives unless --registry overrides it.
func DefaultPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "drover", "actions.toml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "drover", "actions.toml")
}

// Summary is a one-line rendering for `action list`.
func (a Action) Summary() string {
	repo := a.Repo
	if repo == "" {
		repo = "*"
	}
	return strings.Join([]string{a.ID, a.Name, a.On, repo}, "  ")
}
