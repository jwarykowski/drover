// Package config is drover's single trusted store: the sources it ingests from,
// the runners it may invoke, and the actions binding one to the other.
//
// It is drover-owned and authored through the `drover action` CLI. Nothing in
// an event can name a command body — an event can at most match an action that
// is already here, and every command (a source's argv, a runner's argv) lives
// in this file alone.
package config

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

// Source is one event feed. Exactly one transport is set: Cmd spawns a process
// and reads NDJSON from its stdout; HTTP binds a listener that accepts POSTed
// NDJSON. Both deliver the same envelope, so a source is free to be a local
// shim, a third-party plugin in any language, or a remote service.
type Source struct {
	Name  string   `toml:"name"`
	Cmd   []string `toml:"cmd,omitempty"`   // local: argv to spawn, NDJSON on stdout
	HTTP  string   `toml:"http,omitempty"`  // remote: host:port to accept POST /events on
	Types []string `toml:"types,omitempty"` // event types this source emits, for the action editor
}

// Runner is an executable drover can invoke to act on an event — an agentic
// coding tool, a script, any program. Cmd is an argv template: {{prompt}}
// receives the built prompt and {{mode}} the action's permission mode. A tool
// without a permission concept simply omits {{mode}} from its template.
type Runner struct {
	Name  string   `toml:"name"`
	Cmd   []string `toml:"cmd"`
	Modes []string `toml:"modes,omitempty"` // permitted mode values; empty = unchecked
}

// Action binds an event to a runner. Where narrows the match to events whose
// data carries those exact values, so a filter is not limited to any one
// source's vocabulary.
type Action struct {
	ID     string            `toml:"id"`               // stable key a task references
	Name   string            `toml:"name"`             // friendly label, editable
	On     string            `toml:"on"`               // event Type to match
	Where  map[string]string `toml:"where,omitempty"`  // event data filter; empty = match any
	Runner string            `toml:"runner,omitempty"` // runner name; empty = the first [[runner]]
	Mode   string            `toml:"mode,omitempty"`   // permission mode passed as {{mode}}
	Target string            `toml:"target,omitempty"` // runner cwd; may template {{key}} from event data
	Auto   bool              `toml:"auto,omitempty"`   // skip the human hold gate — see Risky
	Do     string            `toml:"do"`               // the prompt body
}

// file is the on-disk shape. Kept separate from Config so the mutex and path
// never become TOML keys.
type file struct {
	Source []Source `toml:"source"`
	Runner []Runner `toml:"runner"`
	Action []Action `toml:"action"`
}

// Config is the loaded file plus the path it came from. mu guards the tables so
// the daemon can Reload from the sensing goroutine while agent workers resolve
// ids concurrently.
type Config struct {
	Path string
	mu   sync.RWMutex
	file
}

var ErrNotFound = errors.New("config: action not found")

// Load reads the config from path. A missing file is an empty config (so
// `drover action add` works on first run), not an error.
func Load(path string) (*Config, error) {
	c := &Config{Path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := toml.Unmarshal(b, &c.file); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return c, nil
}

// Reload swaps the tables for a fresh read of path, under the write lock, so
// `drover action add|edit|rm` take effect in a running daemon without a race.
func (c *Config) Reload(path string) error {
	fresh, err := Load(path)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.file = fresh.file
	c.mu.Unlock()
	return nil
}

// Save writes the config back to its path (creating parent dirs).
func (c *Config) Save() error {
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o755); err != nil {
		return err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(c.file); err != nil {
		return fmt.Errorf("config: encode: %w", err)
	}
	return os.WriteFile(c.Path, buf.Bytes(), 0o644)
}

// Match returns the actions listening for evType whose Where filter is satisfied
// by the event's data. An absent key never matches a non-empty filter value, so
// a filter can only narrow.
func (c *Config) Match(evType string, data map[string]string) []Action {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []Action
	for _, a := range c.Action {
		if a.On != evType {
			continue
		}
		if matches(a.Where, data) {
			out = append(out, a)
		}
	}
	return out
}

func matches(where, data map[string]string) bool {
	for k, v := range where {
		if data[k] != v {
			return false
		}
	}
	return true
}

// ByID resolves a fired task's action reference back to its row.
func (c *Config) ByID(id string) (Action, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, a := range c.Action {
		if a.ID == id {
			return a, true
		}
	}
	return Action{}, false
}

// RunnerByName resolves an action's runner. An empty name takes the first
// configured runner, so a single-tool setup needs no `runner =` on every action.
func (c *Config) RunnerByName(name string) (Runner, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.Runner) == 0 {
		return Runner{}, errors.New("config: no [[runner]] configured")
	}
	if name == "" {
		return c.Runner[0], nil
	}
	for _, g := range c.Runner {
		if g.Name == name {
			return g, nil
		}
	}
	return Runner{}, fmt.Errorf("config: no runner named %q", name)
}

// RunnerNames lists the configured runners, for the action editor's picker.
func (c *Config) RunnerNames() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, len(c.Runner))
	for i, g := range c.Runner {
		out[i] = g.Name
	}
	return out
}

// Runners returns a copy of the runner table, for listing/editing.
func (c *Config) Runners() []Runner {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Runner, len(c.Runner))
	copy(out, c.Runner)
	return out
}

// AddRunner appends a new runner; runners are keyed by Name, so a duplicate name
// is an error rather than a silent shadow (RunnerByName resolves the first match).
func (c *Config) AddRunner(g Runner) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.Runner {
		if e.Name == g.Name {
			return fmt.Errorf("config: runner %q already exists", g.Name)
		}
	}
	c.Runner = append(c.Runner, g)
	return nil
}

// ReplaceRunner overwrites the runner with g.Name; ErrNotFound if absent.
func (c *Config) ReplaceRunner(g Runner) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.Runner {
		if c.Runner[i].Name == g.Name {
			c.Runner[i] = g
			return nil
		}
	}
	return fmt.Errorf("%w: runner %s", ErrNotFound, g.Name)
}

// RemoveRunner drops the runner with name; ErrNotFound if absent.
func (c *Config) RemoveRunner(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, g := range c.Runner {
		if g.Name == name {
			c.Runner = append(c.Runner[:i], c.Runner[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("%w: runner %s", ErrNotFound, name)
}

// Types is the union of every event type the configured sources declare, sorted.
// It replaces a compiled-in list: the types offered by the action editor now
// come from the sources actually installed, so a plugin's own types show up
// without drover knowing anything about it. Sources that declare none simply
// contribute none — a type can always be typed in by hand.
func (c *Config) Types() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	seen := map[string]bool{}
	var out []string
	for _, s := range c.Source {
		for _, t := range s.Types {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Add assigns a fresh id and appends the action.
func (c *Config) Add(a Action) (Action, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a.ID == "" {
		a.ID = NewID()
	}
	c.Action = append(c.Action, a)
	return a, nil
}

// Replace overwrites the action with a.ID; ErrNotFound if absent.
func (c *Config) Replace(a Action) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.Action {
		if c.Action[i].ID == a.ID {
			c.Action[i] = a
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrNotFound, a.ID)
}

// Remove drops the action with id; ErrNotFound if absent.
func (c *Config) Remove(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, a := range c.Action {
		if a.ID == id {
			c.Action = append(c.Action[:i], c.Action[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrNotFound, id)
}

// Actions returns a copy of the action table, for listing.
func (c *Config) Actions() []Action {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Action, len(c.Action))
	copy(out, c.Action)
	return out
}

// Sources returns a copy of the source table.
func (c *Config) Sources() []Source {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Source, len(c.Source))
	copy(out, c.Source)
	return out
}

// UpsertSource adds s, or overwrites the row with the same Name. Unlike
// AddRunner a duplicate is not an error: a source row is fully described by its
// own fields, so installing the same one twice must be idempotent rather than
// something the caller has to check for first.
//
// Exactly one transport must be set. build skips a row with both or neither
// (daemon/daemon.go), and a source that is silently never spawned is a far worse
// failure than a rejected write.
func (c *Config) UpsertSource(s Source) error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("config: source name is required")
	}
	switch {
	case len(s.Cmd) > 0 && s.HTTP != "":
		return fmt.Errorf("config: source %q sets both cmd and http", s.Name)
	case len(s.Cmd) == 0 && s.HTTP == "":
		return fmt.Errorf("config: source %q sets neither cmd nor http", s.Name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.Source {
		if c.Source[i].Name == s.Name {
			c.Source[i] = s
			return nil
		}
	}
	c.Source = append(c.Source, s)
	return nil
}

// RemoveSource drops the source row with name; ErrNotFound if absent. The process
// it names keeps running until the daemon restarts, same as adding one.
func (c *Config) RemoveSource(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, s := range c.Source {
		if s.Name == name {
			c.Source = append(c.Source[:i], c.Source[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("%w: source %s", ErrNotFound, name)
}

// ValidMode reports whether m is permitted for the named runner. A runner that
// declares no modes accepts anything, since drover cannot know a third-party
// tool's vocabulary.
func (c *Config) ValidMode(runner, m string) bool {
	g, err := c.RunnerByName(runner)
	if err != nil || len(g.Modes) == 0 {
		return true
	}
	return slices.Contains(g.Modes, m)
}

// Risky reports whether an action fires an agent unattended in a mode that
// waives per-tool permission prompts. Such an action runs a tool with file
// system access on event text drover did not author, so the author is warned
// before it is saved. Kept here rather than in the TUI so every authoring path
// asks the same question.
func (a Action) Risky() bool {
	return a.Auto && (a.Mode == "bypassPermissions" || a.Mode == "full-auto" || a.Mode == "yolo")
}

// NewID is a short, stable, lowercase-hex id.
func NewID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// DefaultPath is where the config lives unless --config overrides it.
func DefaultPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "drover", "drover.toml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "drover", "drover.toml")
}

// Summary is a one-line rendering for `action list`.
func (a Action) Summary() string {
	where := "*"
	if len(a.Where) > 0 {
		parts := make([]string, 0, len(a.Where))
		for k, v := range a.Where {
			parts = append(parts, k+"="+v)
		}
		sort.Strings(parts)
		where = strings.Join(parts, ",")
	}
	runner := a.Runner
	if runner == "" {
		runner = "(default)"
	}
	return strings.Join([]string{a.ID, a.Name, a.On, where, runner}, "  ")
}
