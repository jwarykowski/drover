// Package tui is drover's interactive UI: the dashboard (board + watch control)
// and the action manager for the trusted registry. The action manager is a
// bubbletea model so it runs standalone (`drover action`) or embedded in the
// dashboard — pressing `a` there never leaves the dashboard program.
package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jwarykowski/drover/registry"
	"github.com/jwarykowski/drover/store"
)

// newSentinel is the list value that means "create a new action".
const newSentinel = "\x00new"

// targetCustom is the target cycle value meaning "a custom path, not a board".
const targetCustom = "\x00custom"

// Run opens the action manager standalone (`drover action`).
func Run(regPath string) error {
	_, err := tea.NewProgram(newActionsModel(regPath, true), tea.WithAltScreen()).Run()
	return err
}

// astate is the action manager's current step.
type astate int

const (
	sList   astate = iota // choose an action, or "new"
	sVerb                 // view / edit / delete / back
	sDetail               // create/edit the fields
	sDelete               // confirm delete
	sView                 // read-only detail
)

// actionsModel is the registry manager as a tea.Model. Standalone it quits when
// the user leaves the list; embedded (standalone=false) it sets done so the
// dashboard pops back without tearing down the watch daemon.
type actionsModel struct {
	regPath    string
	reg        *registry.Registry
	standalone bool
	store      store.ShepherdStore // board listing for the target picker

	state   astate
	pick    *picker         // sList, sVerb, sDelete
	ed      *editor         // sDetail
	buf     form            // create/edit field buffer
	curID   string          // selected action id
	editing bool            // sDetail applies to an existing action
	choice  string          // last picker selection
	view    registry.Action // sView subject

	w, h int
	done bool
}

func newActionsModel(regPath string, standalone bool) *actionsModel {
	reg, _ := registry.Load(regPath)
	if reg == nil {
		reg = &registry.Registry{Path: regPath}
	}
	m := &actionsModel{regPath: regPath, reg: reg, standalone: standalone}
	m.enter(sList)
	return m
}

func (m *actionsModel) Init() tea.Cmd {
	if m.ed != nil {
		return textinput.Blink
	}
	return nil
}

func (m *actionsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		switch m.state {
		case sView: // any key leaves the read-only view
			return m, m.enter(sList)
		case sDetail:
			cmd, res := m.ed.Update(msg)
			switch res {
			case editSave:
				return m, m.advance()
			case editCancel:
				return m, m.back()
			}
			return m, cmd
		default: // picker states
			return m, m.pickKey(msg)
		}
	}
	// non-key messages (cursor blink) drive the editor's focused field
	if m.state == sDetail && m.ed != nil {
		cmd, _ := m.ed.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *actionsModel) pickKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "up", "k":
		m.pick.up()
	case "down", "j":
		m.pick.down()
	case "esc", "q":
		return m.back()
	case "enter":
		m.choice = m.pick.value()
		return m.advance()
	}
	return nil
}

// bodyLines renders the inner content (no header/footer) filled to bodyH so the
// key hint lands on the last body row and the footer sits at the terminal
// bottom. The dashboard and the standalone View both wrap it in the shared
// shell so the title never moves.
func (m *actionsModel) bodyLines(w, bodyH int) []string {
	var content []string
	hint := ""
	switch m.state {
	case sView:
		content = strings.Split(renderDetail(m.view, w), "\n") // carries its own hint
	case sDetail:
		if m.ed != nil {
			content = m.ed.render(w)
		}
		hint = "↑↓/tab move · ←→ change · enter save · esc cancel"
	default:
		if m.pick != nil {
			content = m.pick.render(w)
		}
		hint = "↑/↓ move · enter select · esc back"
	}
	if hint == "" {
		return content
	}
	content = pad(content, max(len(content), bodyH-1))
	return append(content, hintStyle.Render(hint))
}

func (m *actionsModel) View() string {
	w := innerWidth(m.w)
	return shell(w, m.h, "", m.bodyLines(w, fillCount(m.h, 4)))
}

// enter switches state and builds the widget it uses.
func (m *actionsModel) enter(s astate) tea.Cmd {
	m.state = s
	m.pick, m.ed = nil, nil
	switch s {
	case sList:
		m.curID, m.editing = "", false
		m.pick = m.listPicker()
	case sVerb:
		m.pick = m.verbPicker()
	case sDetail:
		boards, _ := m.store.Boards(context.Background()) // best-effort; empty → custom-only
		m.ed = newEditor(m.editorTitle(), &m.buf, boards)
		return textinput.Blink
	case sDelete:
		m.pick = m.deletePicker()
	}
	return nil
}

func (m *actionsModel) editorTitle() string {
	if m.editing {
		return "edit action"
	}
	return "new action"
}

// advance handles a confirmed picker choice or a saved editor.
func (m *actionsModel) advance() tea.Cmd {
	switch m.state {
	case sList:
		if m.choice == newSentinel {
			m.buf = form{kind: "github", mode: registry.AutoModes[0]}
			m.editing = false
			return m.enter(sDetail)
		}
		m.curID = m.choice
		return m.enter(sVerb)
	case sVerb:
		switch m.choice {
		case "view":
			if a, ok := m.reg.ByID(m.curID); ok {
				m.view = a
			}
			return m.enter(sView)
		case "edit":
			if a, ok := m.reg.ByID(m.curID); ok {
				m.buf = fromAction(a)
			}
			m.editing = true
			return m.enter(sDetail)
		case "delete":
			return m.enter(sDelete)
		default: // back
			return m.enter(sList)
		}
	case sDetail:
		a := toAction(m.buf)
		if m.editing {
			_ = m.reg.Remove(m.curID)
			a.ID = m.curID
			m.reg.Actions = append(m.reg.Actions, a)
		} else {
			_, _ = m.reg.Add(a)
		}
		_ = m.reg.Save()
		return m.enter(sList)
	case sDelete:
		if m.choice == "delete" {
			_ = m.reg.Remove(m.curID)
			_ = m.reg.Save()
		}
		return m.enter(sList)
	}
	return nil
}

// back drops down a level, or leaves from the list.
func (m *actionsModel) back() tea.Cmd {
	if m.state == sList {
		m.done = true
		if m.standalone {
			return tea.Quit
		}
		return nil
	}
	return m.enter(sList)
}

func (m *actionsModel) listPicker() *picker {
	p := &picker{title: "drover actions", desc: "enter to open · esc to go back"}
	for _, a := range m.reg.Actions {
		p.opts = append(p.opts, pickOption{a.Summary(), a.ID})
	}
	p.opts = append(p.opts, pickOption{"＋ new action", newSentinel})
	return p
}

func (m *actionsModel) verbPicker() *picker {
	a, _ := m.reg.ByID(m.curID)
	return &picker{
		title: a.Name,
		desc:  a.On,
		opts: []pickOption{
			{"view", "view"},
			{"edit", "edit"},
			{"delete", "delete"},
			{"back", "back"},
		},
	}
}

func (m *actionsModel) deletePicker() *picker {
	a, _ := m.reg.ByID(m.curID)
	return &picker{
		title: fmt.Sprintf("delete %q?", a.Name),
		opts:  []pickOption{{"cancel", "cancel"}, {"delete", "delete"}},
	}
}

// form is the editable state bound to the editor fields.
type form struct {
	kind      string // event family: github | sentry | board
	on        string // full event type, e.g. github.pull_request.merged
	name      string
	repo      string
	base      string // github poll-mode branch
	source    string // github sense: forward | poll
	interval  string // github poll interval, e.g. 60s
	targetSel string // board name, or targetCustom for a literal path
	target    string // custom path (when targetSel == targetCustom)
	mode      string
	do        string
}

// newEditor builds the action editor over a buffer. Fields hide by relevance:
// board has no repo filter, the sensing knobs are github-only, base/interval
// only apply in poll mode, and the custom-dir input only shows when target isn't
// a board. boards populates the target picker.
func newEditor(title string, f *form, boards []store.Board) *editor {
	e := &editor{title: title}
	typeF := cycleField("type", kinds(), kinds(), &f.kind,
		"Which product emits the event. Sets the subactions and sensing options below.", nil)
	subF := cycleField("subaction", subactionOns(f.kind), subactionLabels(f.kind), &f.on,
		"The exact event that triggers this action.", nil)
	doF := areaField("do",
		&f.do, "The task prompt handed to the agent. drover frames it with event context at run time. Leave empty to use the shown default.")
	e.typeF, e.subF, e.doF = typeF, subF, doF

	// target: a shepherd board (its dir, resolved at run time) or a custom path.
	tOpts, tLabels := boardTargetOptions(boards)
	if f.targetSel == "" {
		f.targetSel = tOpts[0]
	}
	targetF := cycleField("target", tOpts, tLabels, &f.targetSel,
		"Where the agent runs. Pick a shepherd board (its dir, resolved live at run time) or a custom path.", nil)
	customF := inputField("dir", &f.target, "e.g. ~/src/acme-api",
		"Literal working directory for this action.", true, func() bool { return f.targetSel != targetCustom })

	// source filter: repo for github (owner/name), project for sentry. reconcile
	// swaps the label/placeholder by kind; hidden for board (no source filter).
	repoF := inputField("repo", &f.repo, "owner/name",
		"Only react to events from this source. github: owner/name. sentry: the project slug. Empty = any.", false,
		func() bool { return f.kind == "board" })
	e.repoF = repoF

	notGithub := func() bool { return f.kind != "github" }
	notPoll := func() bool { return f.kind != "github" || f.source != "poll" }

	e.fields = []*editField{
		typeF,
		subF,
		inputField("name", &f.name, "e.g. fix-ci",
			"A short label for this action, shown in lists and logs.", true, nil),
		targetF,
		customF,
		cycleField("mode", registry.AutoModes, registry.AutoModes, &f.mode,
			"How much the agent may do unattended.", nil),
		repoF,
		cycleField("source", registry.ValidSources, registry.ValidSources, &f.source,
			"How drover watches the repo. forward receives webhooks; poll checks on an interval.", notGithub),
		inputField("base", &f.base, "master",
			"Poll mode: the branch to watch. Empty = master.", false, notPoll),
		inputField("interval", &f.interval, "60s",
			"Poll mode: how often to check, e.g. 60s.", false, notPoll),
		doF, // the prompt textarea sits last: it's the tallest field and the focus of authoring
	}
	e.reconcile()
	return e
}

// boardTargetOptions builds the target cycle: each board (label shows its dir),
// then a "custom path" entry. Always non-empty (custom is the fallback).
func boardTargetOptions(boards []store.Board) (opts, labels []string) {
	for _, b := range boards {
		dir := b.Dir
		if dir == "" {
			dir = "no dir set"
		}
		opts = append(opts, b.Name)
		labels = append(labels, b.Name+" — "+dir)
	}
	return append(opts, targetCustom), append(labels, "custom path…")
}

func toAction(f form) registry.Action {
	do := strings.TrimSpace(f.do)
	if do == "" { // left empty → the shown default prompt for this event
		do = defaultPrompt(f.on)
	}
	a := registry.Action{
		Name: strings.TrimSpace(f.name),
		On:   f.on,
		Repo: strings.TrimSpace(f.repo),
		Mode: f.mode,
		Do:   do,
	}
	// target is either a board reference (resolved at run time) or a literal path.
	if f.targetSel == "" || f.targetSel == targetCustom {
		a.Target = strings.TrimSpace(f.target)
	} else {
		a.TargetBoard = f.targetSel
	}
	// github-only knobs; leave empty for other families so their rows stay clean.
	if f.kind == "github" {
		a.Base = strings.TrimSpace(f.base)
		a.Source = f.source
		a.Interval = strings.TrimSpace(f.interval)
	} else {
		a.Repo = "" // repo filter is a github/sentry notion; board has none
	}
	return a
}

func fromAction(a registry.Action) form {
	targetSel := targetCustom
	if a.TargetBoard != "" {
		targetSel = a.TargetBoard
	}
	return form{
		kind:      kindOf(a.On),
		on:        a.On,
		name:      a.Name,
		repo:      a.Repo,
		base:      a.Base,
		source:    a.Source,
		interval:  a.Interval,
		targetSel: targetSel,
		target:    a.Target,
		mode:      a.Mode,
		do:        a.Do,
	}
}

// kindOf is the event family: the segment before the first dot.
func kindOf(on string) string {
	if i := strings.IndexByte(on, '.'); i >= 0 {
		return on[:i]
	}
	return on
}

// kinds are the event families in the order they first appear in the registry's
// known types (github, sentry, board).
func kinds() []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range registry.KnownEventTypes {
		k := kindOf(t)
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// subactionOns are the known event types within a family (the stored values).
func subactionOns(kind string) []string {
	var out []string
	for _, t := range registry.KnownEventTypes {
		if strings.HasPrefix(t, kind+".") {
			out = append(out, t)
		}
	}
	return out
}

// subactionLabels are the friendly labels parallel to subactionOns.
func subactionLabels(kind string) []string {
	var out []string
	for _, t := range registry.KnownEventTypes {
		if strings.HasPrefix(t, kind+".") {
			out = append(out, label(t))
		}
	}
	return out
}

// label renders an event type without its family prefix, in words:
// "github.pull_request.merged" → "pull request merged".
func label(on string) string {
	i := strings.IndexByte(on, '.')
	if i < 0 {
		return on
	}
	return strings.NewReplacer("_", " ", ".", " ").Replace(on[i+1:])
}

// defaultPrompts seed the `do` field per event type — the "generic prompt" the
// user then edits. buildAgentPrompt frames this into the full agent prompt at
// run time, so these are just the task intent.
var defaultPrompts = map[string]string{
	"github.pull_request.merged": "A PR merged. If CI on the base branch is red, open a fix PR.",
	"github.pull_request.opened": "A PR opened. Review the diff and leave your findings as a note.",
	"github.pull_request.closed": "A PR closed without merging. Note anything that needs following up.",
	"github.issues.opened":       "An issue was opened. Triage it and propose next steps.",
	"sentry.issue.opened":        "A Sentry issue opened. Investigate the stack trace and propose a fix.",
	"board.added":                "A board item was added. Handle it per its description.",
	"board.updated":              "A board item changed. Reconcile the change per its description.",
	"board.removed":              "A board item was removed. Clean up anything it left behind.",
	"board.archived":             "A board item was archived. Wrap up any loose ends.",
}

func defaultPrompt(on string) string { return defaultPrompts[on] }
