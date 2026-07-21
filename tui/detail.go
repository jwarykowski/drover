package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
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

// showDetail renders an action in a read-only, shepherd-styled screen until the
// user presses q/esc/enter.
func showDetail(a registry.Action) error {
	_, err := tea.NewProgram(detailModel{a: a}, tea.WithAltScreen()).Run()
	return err
}

type detailModel struct {
	a registry.Action
	w int
}

func (m detailModel) Init() tea.Cmd { return nil }

func (m detailModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w = msg.Width
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "enter", "ctrl+c":
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m detailModel) View() string {
	w := m.w
	if w <= 0 || w > 80 {
		w = 80
	}
	rule := ruleStyle.Render(strings.Repeat("┈", w))

	repo := m.a.Repo
	if repo == "" {
		repo = "* (any)"
	}

	var b strings.Builder
	fmt.Fprintln(&b, titleStyle.Render(m.a.Name))
	fmt.Fprintln(&b, rule)
	field(&b, "id", m.a.ID)
	field(&b, "on", fmt.Sprintf("%s  (%s)", m.a.On, label(m.a.On)))
	field(&b, "repo", repo)
	field(&b, "target", m.a.Target)
	field(&b, "mode", m.a.Mode)
	if m.a.Source != "" {
		field(&b, "source", m.a.Source)
	}
	if m.a.Base != "" {
		field(&b, "base", m.a.Base)
	}
	if m.a.Interval != "" {
		field(&b, "interval", m.a.Interval)
	}
	fmt.Fprintln(&b, rule)
	fmt.Fprintln(&b, keyStyle.Render("do"))
	fmt.Fprintln(&b, m.a.Do)
	fmt.Fprintln(&b, rule)
	fmt.Fprint(&b, hintStyle.Render("q/esc to go back"))

	return lipgloss.NewStyle().Padding(1, 2).Render(b.String())
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
