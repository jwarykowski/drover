// Package store adapts shepherd's CLI to the loop.Store seam. This is the only
// file in drover that knows shepherd exists.
package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/jwarykowski/drover/loop"
)

// ErrNotFound maps shepherd's {"error":"not_found"} onto a typed Go error so the
// loop can branch on outcome without scraping text.
var ErrNotFound = errors.New("shepherd: item not found")

// ShepherdStore talks to a real shepherd binary over os/exec.
type ShepherdStore struct {
	Bin     string // path or name of the shepherd binary; defaults to "shepherd"
	Project string // board to act on; empty means the default board
}

func (s ShepherdStore) bin() string {
	if s.Bin == "" {
		return "shepherd"
	}
	return s.Bin
}

// List returns the board, narrowed by f. Category and IncludeDone are applied in
// Go; Text is pushed down to shepherd's --filter.
func (s ShepherdStore) List(ctx context.Context, f loop.Filter) ([]loop.Item, error) {
	args := []string{"list", "--json"}
	if f.Text != "" {
		args = append(args, "--filter", f.Text)
	}
	out, err := s.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var items []loop.Item
	if err := json.Unmarshal(out, &items); err != nil {
		return nil, fmt.Errorf("shepherd list: %w", err)
	}
	kept := items[:0]
	for _, it := range items {
		if it.Done && !f.IncludeDone {
			continue
		}
		if f.Category != "" && it.Category != f.Category {
			continue
		}
		kept = append(kept, it)
	}
	return kept, nil
}

// Add creates an item from spec and returns it, parsed from `add --json`.
func (s ShepherdStore) Add(ctx context.Context, spec loop.Spec) (loop.Item, error) {
	out, err := s.run(ctx, "add", buildAddText(spec), "--json")
	if err != nil {
		return loop.Item{}, err
	}
	var it loop.Item
	if err := json.Unmarshal(out, &it); err != nil {
		return loop.Item{}, fmt.Errorf("shepherd add: %w", err)
	}
	return it, nil
}

// SetStatus marks an item done/undone, or sets a named status, addressing by id.
func (s ShepherdStore) SetStatus(ctx context.Context, id, status string) error {
	var args []string
	switch status {
	case "done":
		args = []string{"done", id, "--json"}
	case "undone", "":
		args = []string{"undone", id, "--json"}
	default:
		args = []string{"edit", id, "status:" + status, "--json"}
	}
	_, err := s.run(ctx, args...)
	return err
}

// run executes shepherd with the project flag prepended, and maps a --json error
// envelope on stdout to a typed error.
func (s ShepherdStore) run(ctx context.Context, args ...string) ([]byte, error) {
	// shepherd wants flags after the verb ("Flags follow the verb"), so the
	// project flag trails rather than leading.
	full := args
	if s.Project != "" {
		full = append(full, "--project", s.Project)
	}
	cmd := exec.CommandContext(ctx, s.bin(), full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	// --json surfaces failures as {"error":...} on stdout; check it first so the
	// stable error string wins over the raw exit code.
	if berr := parseErrorEnvelope(stdout.Bytes()); berr != nil {
		return nil, berr
	}
	if runErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = runErr.Error()
		}
		return nil, fmt.Errorf("shepherd %s: %s", args[0], msg)
	}
	return stdout.Bytes(), nil
}

func parseErrorEnvelope(out []byte) error {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var env struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(trimmed, &env) != nil || env.Error == "" {
		return nil
	}
	if env.Error == "not_found" {
		return fmt.Errorf("%w: %s", ErrNotFound, env.Detail)
	}
	return fmt.Errorf("shepherd: %s (%s)", env.Error, env.Detail)
}

// buildAddText renders a Spec into shepherd's single-line add syntax.
func buildAddText(s loop.Spec) string {
	parts := []string{s.Text}
	if s.Category != "" {
		parts = append(parts, "@"+s.Category)
	}
	switch strings.ToUpper(s.Priority) {
	case "H":
		parts = append(parts, "!h")
	case "M":
		parts = append(parts, "!m")
	case "L":
		parts = append(parts, "!l")
	}
	if s.Due != "" {
		parts = append(parts, "due:"+s.Due)
	}
	if s.Link != "" {
		parts = append(parts, "link:"+s.Link)
	}
	if s.Note != "" {
		// note: takes the rest of the line, so it goes last.
		parts = append(parts, "note:"+s.Note)
	}
	return strings.Join(parts, " ")
}
