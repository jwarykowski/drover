package exec

import (
	"fmt"

	"github.com/BurntSushi/toml"
)

// ActionSpec is one allowlisted named action. The command body lives here in
// drover's trusted config — never in a board item.
type ActionSpec struct {
	Cmd     []string `toml:"cmd"`     // argv template; {{key}} placeholders filled from RunAction.Args
	Cwd     string   `toml:"cwd"`     // working directory; may template {{key}}
	Confirm bool     `toml:"confirm"` // require confirmation before firing
	Timeout string   `toml:"timeout"` // Go duration; empty means no deadline beyond ctx
}

type config struct {
	Actions map[string]ActionSpec `toml:"actions"`
}

// LoadAllowlist reads the action allowlist from a config.toml. The returned map
// is the ONLY set of commands the runner will ever execute.
func LoadAllowlist(path string) (map[string]ActionSpec, error) {
	var c config
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return nil, fmt.Errorf("load allowlist %s: %w", path, err)
	}
	for name, spec := range c.Actions {
		if len(spec.Cmd) == 0 {
			return nil, fmt.Errorf("action %q: cmd is empty", name)
		}
	}
	return c.Actions, nil
}
