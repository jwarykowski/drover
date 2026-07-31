package tui

import (
	"fmt"
	"slices"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/jwarykowski/drover/config"
)

// This file is drover's hand-rolled form widgets — a single-select picker and a
// multi-field editor — replacing huh. huh is its own tea.Model that owns its
// layout (it distributes fields to a forced height and draws its own help line),
// so it can't be pinned inside the dashboard's shell; these render plain line
// slices the shell positions exactly like every other view.

// ---- picker: a single-select list (action list, verb menu, delete confirm) ----

type pickOption struct{ label, value string }

type picker struct {
	title string
	desc  string
	opts  []pickOption
	cur   int
}

func (p *picker) up() {
	if p.cur > 0 {
		p.cur--
	}
}

func (p *picker) down() {
	if p.cur < len(p.opts)-1 {
		p.cur++
	}
}

func (p *picker) value() string {
	if len(p.opts) == 0 {
		return ""
	}
	return p.opts[p.cur].value
}

func (p *picker) render(w int) []string {
	out := []string{titleStyle.Render(p.title)}
	if p.desc != "" {
		out = append(out, hintStyle.Render(p.desc))
	}
	out = append(out, ruleStyle.Render(strings.Repeat("┈", w)), "")
	for i, o := range p.opts {
		cursor := "  "
		if i == p.cur {
			cursor = valStyle.Render("▸ ")
		}
		out = append(out, truncate(cursor+o.label, w))
	}
	return out
}

// ---- editor: a multi-field form ----

type fieldType int

const (
	tInput fieldType = iota // single-line textinput
	tArea                   // multi-line textarea
	tCycle                  // pick one of opts with space
)

// editField is one row of the editor. val points at the buffer string the field
// reads from and writes back to; for tArea it's the textarea's value. help is
// the explanation shown in the right-hand panel while the field is focused.
type editField struct {
	key       string
	typ       fieldType
	ti        textinput.Model
	ta        textarea.Model
	opts      []string // tCycle values
	optLabels []string // tCycle display labels (parallel to opts)
	oi        int      // tCycle selected index
	val       *string
	help      string
	req       bool
	hide      func() bool
}

// editResult is what an editor key press asks the host to do.
type editResult int

const (
	editNone editResult = iota
	editSave
	editCancel
)

const labelW = 9 // field label column width (e.g. "target  ")

type editor struct {
	title   string
	fields  []*editField
	types   []string       // event types the configured sources declare
	cf      *config.Config // runners and their accepted modes
	typeF   *editField     // event-family cycle; drives subF's options
	subF    *editField     // subaction cycle, rebuilt when typeF changes
	doF     *editField     // the prompt textarea, whose placeholder tracks subF
	runnerF *editField     // runner cycle; drives modeF's options
	modeF   *editField     // permission mode cycle, rebuilt when runnerF changes
	cur     int            // index into visible()
	notice  string
}

// kinds are the event families across the declared types, in sorted order.
func (e *editor) kinds() []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range e.types {
		k := kindOf(t)
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// subactionOns are the declared event types within a family (stored values).
func (e *editor) subactionOns(kind string) []string {
	var out []string
	for _, t := range e.types {
		if strings.HasPrefix(t, kind+".") {
			out = append(out, t)
		}
	}
	return out
}

// subactionLabels are the friendly labels parallel to subactionOns.
func (e *editor) subactionLabels(kind string) []string {
	ons := e.subactionOns(kind)
	out := make([]string, len(ons))
	for i, t := range ons {
		out[i] = label(t)
	}
	return out
}

// modes are the permission values the named runner accepts. A runner that
// declares none gets a single empty option — drover cannot invent a vocabulary
// for a tool it knows nothing about.
func (e *editor) modes(runner string) []string {
	if e.cf == nil {
		return []string{""}
	}
	g, err := e.cf.RunnerByName(runner)
	if err != nil || len(g.Modes) == 0 {
		return []string{""}
	}
	return g.Modes
}

func inputField(key string, val *string, placeholder, help string, req bool, hide func() bool) *editField {
	ti := textinput.New()
	ti.Prompt = "› "
	ti.Placeholder = placeholder
	ti.SetValue(*val)
	return &editField{key: key, typ: tInput, ti: ti, val: val, help: help, req: req, hide: hide}
}

func areaField(key string, val *string, help string) *editField {
	ta := textarea.New()
	ta.Prompt = "│ "
	ta.ShowLineNumbers = false
	ta.SetValue(*val)
	ta.SetHeight(8)
	return &editField{key: key, typ: tArea, ta: ta, val: val, help: help}
}

// cycleField aligns the buffer value with the shown option so hide funcs that
// read the buffer (e.g. base/interval keying off source) see the real value.
func cycleField(key string, opts, labels []string, val *string, help string, hide func() bool) *editField {
	oi := max(0, slices.Index(opts, *val))
	if len(opts) > 0 {
		*val = opts[oi]
	}
	return &editField{key: key, typ: tCycle, opts: opts, optLabels: labels, oi: oi, val: val, help: help, hide: hide}
}

func (e *editor) visible() []*editField {
	var v []*editField
	for _, f := range e.fields {
		if f.hide == nil || !f.hide() {
			v = append(v, f)
		}
	}
	return v
}

// reconcile keeps derived state consistent: rebuild the subaction options when
// the type changed and the mode options when the runner changed, refresh the
// prompt placeholder, clamp the cursor to the visible set, and refocus.
func (e *editor) reconcile() {
	if e.typeF != nil && e.subF != nil {
		kind := *e.typeF.val
		if ons := e.subactionOns(kind); !slices.Equal(e.subF.opts, ons) {
			e.subF.opts, e.subF.optLabels, e.subF.oi = ons, e.subactionLabels(kind), 0
			if len(ons) > 0 {
				*e.subF.val = ons[0]
			} else {
				*e.subF.val = ""
			}
		}
	}
	if e.doF != nil && e.subF != nil {
		e.doF.ta.Placeholder = defaultPrompt(*e.subF.val)
	}
	// The mode vocabulary belongs to the chosen tool, so it is rebuilt whenever
	// the runner changes rather than being a fixed list.
	if e.runnerF != nil && e.modeF != nil {
		if ms := e.modes(*e.runnerF.val); !slices.Equal(e.modeF.opts, ms) {
			e.modeF.opts, e.modeF.optLabels, e.modeF.oi = ms, ms, 0
			*e.modeF.val = ms[0]
		}
	}
	if n := len(e.visible()); n == 0 {
		e.cur = 0
	} else if e.cur >= n {
		e.cur = n - 1
	} else if e.cur < 0 {
		e.cur = 0
	}
	e.syncFocus()
}

func (e *editor) syncFocus() {
	for i, f := range e.visible() {
		switch f.typ {
		case tInput:
			if i == e.cur {
				f.ti.Focus()
			} else {
				f.ti.Blur()
			}
		case tArea:
			if i == e.cur {
				f.ta.Focus()
			} else {
				f.ta.Blur()
			}
		}
	}
}

func (e *editor) move(d int) {
	n := len(e.visible())
	if n == 0 {
		return
	}
	e.cur = ((e.cur+d)%n + n) % n
	e.syncFocus()
}

func (e *editor) cycle(f *editField, d int) {
	n := len(f.opts)
	if n == 0 {
		return
	}
	f.oi = ((f.oi+d)%n + n) % n
	if f.val != nil {
		*f.val = f.opts[f.oi]
	}
	e.reconcile()
}

func (e *editor) collect() {
	for _, f := range e.fields {
		if f.val == nil {
			continue
		}
		switch f.typ {
		case tInput:
			*f.val = f.ti.Value()
		case tArea:
			*f.val = f.ta.Value()
		case tCycle:
			if len(f.opts) > 0 {
				*f.val = f.opts[f.oi]
			}
		}
	}
}

// trySave collects the fields and validates the visible required ones.
func (e *editor) trySave() editResult {
	e.collect()
	for _, f := range e.visible() {
		if f.req && strings.TrimSpace(*f.val) == "" {
			e.notice = f.key + " is required"
			return editNone
		}
	}
	return editSave
}

// Update handles one message. Navigation is arrow-key intuitive: ↑/↓ (and
// tab/shift+tab) move between fields, ←/→ (and space) change a focused select.
// Arrows still edit inside a text field — the textarea keeps ↑/↓ for lines and
// text inputs keep ←/→ for the caret — so only fields where the arrow is
// otherwise meaningless treat it as navigation.
func (e *editor) Update(msg tea.Msg) (tea.Cmd, editResult) {
	vis := e.visible()
	if len(vis) == 0 {
		return nil, editNone
	}
	f := vis[e.cur]
	if k, ok := msg.(tea.KeyMsg); ok {
		switch k.String() {
		case "esc":
			return nil, editCancel
		case "ctrl+s":
			return nil, e.trySave()
		case "tab":
			e.move(1)
			return nil, editNone
		case "shift+tab":
			e.move(-1)
			return nil, editNone
		case "down":
			if f.typ != tArea { // textarea keeps ↓ for line movement
				e.move(1)
				return nil, editNone
			}
		case "up":
			if f.typ != tArea { // textarea keeps ↑ for line movement
				e.move(-1)
				return nil, editNone
			}
		case "left":
			if f.typ == tCycle { // text fields keep ← for the caret
				e.cycle(f, -1)
				return nil, editNone
			}
		case "right", " ": // space also cycles; on text fields it types a space
			if f.typ == tCycle {
				e.cycle(f, 1)
				return nil, editNone
			}
		case "enter":
			if f.typ != tArea { // in the textarea, enter inserts a newline
				return nil, e.trySave()
			}
		}
	}
	var cmd tea.Cmd
	switch f.typ {
	case tInput:
		f.ti, cmd = f.ti.Update(msg)
	case tArea:
		f.ta, cmd = f.ta.Update(msg)
	}
	return cmd, editNone
}

func (e *editor) cycleLabel(f *editField) string {
	if f.oi < len(f.optLabels) {
		return f.optLabels[f.oi]
	}
	if f.oi < len(f.opts) {
		return f.opts[f.oi]
	}
	return ""
}

// render lays the form out in two columns: fields on the left, a help panel
// describing the focused field on the right. Narrow terminals drop the panel.
func (e *editor) render(w int) []string {
	out := []string{titleStyle.Render(e.title), ruleStyle.Render(strings.Repeat("┈", w))}
	leftW := w * 11 / 20
	sep := "  " + ruleStyle.Render("│") + " "
	rightW := w - leftW - lipgloss.Width(sep)
	left := e.fieldLines(leftW)
	if rightW < 14 { // no room for a help column — stack the fields full width
		out = append(out, e.fieldLines(w)...)
	} else {
		right := e.helpLines(rightW)
		for i := 0; i < max(len(left), len(right)); i++ {
			out = append(out, cell(at(left, i), leftW)+sep+cell(at(right, i), rightW))
		}
	}
	if e.notice != "" {
		out = append(out, "", warnStyle.Render(e.notice))
	}
	return out
}

func (e *editor) fieldLines(w int) []string {
	indent := strings.Repeat(" ", 2+labelW)
	var out []string
	for i, f := range e.visible() {
		if i > 0 {
			out = append(out, "") // blank row between properties
		}
		mark := "  "
		if i == e.cur {
			mark = valStyle.Render("▸ ")
		}
		label := keyStyle.Render(fmt.Sprintf("%-*s", labelW, f.key))
		switch f.typ {
		case tInput:
			f.ti.Width = max(10, w-len(indent)-3)
			out = append(out, mark+label+f.ti.View())
		case tCycle:
			out = append(out, mark+label+valStyle.Render("‹ "+e.cycleLabel(f)+" ›"))
		case tArea:
			f.ta.SetWidth(max(10, w-len(indent)))
			for j, ln := range strings.Split(f.ta.View(), "\n") {
				if j == 0 {
					out = append(out, mark+label+ln)
				} else {
					out = append(out, indent+ln)
				}
			}
		}
	}
	return out
}

func (e *editor) helpLines(w int) []string {
	vis := e.visible()
	if len(vis) == 0 {
		return nil
	}
	f := vis[e.cur]
	out := []string{keyStyle.Render("help"), "", titleStyle.Render(f.key)}
	for _, ln := range strings.Split(ansi.Wrap(f.help, w, ""), "\n") {
		out = append(out, hintStyle.Render(ln))
	}
	return out
}
