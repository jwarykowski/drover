package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/jwarykowski/drover/config"
)

// Styles mirror shepherd's TUI: ANSI-16 colours, faint rules, no borders.
var (
	titleStyle = lipgloss.NewStyle().Bold(true)
	keyStyle   = lipgloss.NewStyle().Faint(true)
	valStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
	ruleStyle  = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("240"))
	hintStyle  = lipgloss.NewStyle().Faint(true)
)

// renderDetail renders an action as a read-only screen. The action model shows
// it in the sView state; any key returns to the list.
func renderDetail(a config.Action, w int) string {
	rule := ruleStyle.Render(strings.Repeat("┈", w))
	where := formatWhere(a.Where)
	if where == "" {
		where = "* (any)"
	}
	runner := a.Runner
	if runner == "" {
		runner = "(first configured)"
	}

	var b strings.Builder
	fmt.Fprintln(&b, titleStyle.Render(a.Name))
	fmt.Fprintln(&b, rule)
	field(&b, "id", a.ID)
	field(&b, "on", fmt.Sprintf("%s  (%s)", a.On, label(a.On)))
	field(&b, "where", where)
	field(&b, "runner", runner)
	field(&b, "mode", a.Mode)
	field(&b, "target", a.Target)
	if a.Auto {
		gate := "yes — fires with no human gate"
		if a.Risky() {
			gate += "  ⚠ in a permission-waiving mode"
		}
		field(&b, "auto", gate)
	}
	fmt.Fprintln(&b, rule)
	fmt.Fprintln(&b, keyStyle.Render("do"))
	fmt.Fprintln(&b, a.Do)
	fmt.Fprintln(&b, rule)
	fmt.Fprint(&b, hintStyle.Render("press any key to go back"))

	return b.String()
}

func field(b *strings.Builder, k, v string) {
	if v == "" {
		v = "—"
	}
	const w = 7 // widest label: "target"/"where"
	if len(k) < w {
		k += strings.Repeat(" ", w-len(k))
	}
	fmt.Fprintf(b, "%s  %s\n", keyStyle.Render(k), valStyle.Render(v))
}
