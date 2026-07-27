package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/jwarykowski/drover/registry"
)

// Styles mirror shepherd's TUI: ANSI-16 colours, faint rules, no borders.
var (
	titleStyle = lipgloss.NewStyle().Bold(true)
	keyStyle   = lipgloss.NewStyle().Faint(true)
	valStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
	ruleStyle  = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("240"))
	hintStyle  = lipgloss.NewStyle().Faint(true)
)

// renderDetail renders an action as a read-only, shepherd-styled screen. The
// action model shows it in the sView state; any key returns to the list.
func renderDetail(a registry.Action, w int) string {
	rule := ruleStyle.Render(strings.Repeat("┈", w))
	repo := a.Repo
	if repo == "" {
		repo = "* (any)"
	}

	var b strings.Builder
	fmt.Fprintln(&b, titleStyle.Render(a.Name))
	fmt.Fprintln(&b, rule)
	field(&b, "id", a.ID)
	field(&b, "on", fmt.Sprintf("%s  (%s)", a.On, label(a.On)))
	field(&b, "repo", repo)
	if a.TargetBoard != "" {
		field(&b, "target", a.TargetBoard+"  (board dir)")
	} else {
		field(&b, "target", a.Target)
	}
	field(&b, "mode", a.Mode)
	if a.Source != "" {
		field(&b, "source", a.Source)
	}
	if a.Base != "" {
		field(&b, "base", a.Base)
	}
	if a.Interval != "" {
		field(&b, "interval", a.Interval)
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
	const w = 7
	if len(k) < w {
		k += strings.Repeat(" ", w-len(k))
	}
	fmt.Fprintf(b, "%s  %s\n", keyStyle.Render(k), valStyle.Render(v))
}
