// Package tui is drover's interactive UI: the dashboard (lanes + watch control)
// and the action manager for drover.toml. The action manager is a bubbletea
// model so it runs standalone (`drover action`) or embedded in the dashboard —
// pressing `a` there never leaves the dashboard program.
package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jwarykowski/drover/config"
)

// newSentinel is the list value that means "create a new action".
const newSentinel = "\x00new"

// Run opens the action manager standalone (`drover action`).
func Run(cfgPath string) error {
	_, err := tea.NewProgram(newActionsModel(cfgPath, true), tea.WithAltScreen()).Run()
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

// actionsModel is the action manager as a tea.Model. Standalone it quits when
// the user leaves the list; embedded (standalone=false) it sets done so the
// dashboard pops back without tearing down the watch daemon.
type actionsModel struct {
	cfgPath    string
	cf         *config.Config
	standalone bool

	state   astate
	pick    *picker       // sList, sVerb, sDelete
	ed      *editor       // sDetail
	buf     form          // create/edit field buffer
	curID   string        // selected action id
	editing bool          // sDetail applies to an existing action
	choice  string        // last picker selection
	view    config.Action // sView subject

	w, h int
	done bool
}

func newActionsModel(cfgPath string, standalone bool) *actionsModel {
	cf, _ := config.Load(cfgPath)
	if cf == nil {
		cf = &config.Config{Path: cfgPath}
	}
	m := &actionsModel{cfgPath: cfgPath, cf: cf, standalone: standalone}
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
		m.ed = newEditor(m.editorTitle(), &m.buf, m.cf)
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
			m.buf = form{kind: firstKind(m.cf), runner: firstRunner(m.cf)}
			m.editing = false
			return m.enter(sDetail)
		}
		m.curID = m.choice
		return m.enter(sVerb)
	case sVerb:
		switch m.choice {
		case "view":
			if a, ok := m.cf.ByID(m.curID); ok {
				m.view = a
			}
			return m.enter(sView)
		case "edit":
			if a, ok := m.cf.ByID(m.curID); ok {
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
			a.ID = m.curID
			_ = m.cf.Replace(a)
		} else {
			_, _ = m.cf.Add(a)
		}
		_ = m.cf.Save()
		return m.enter(sList)
	case sDelete:
		if m.choice == "delete" {
			_ = m.cf.Remove(m.curID)
			_ = m.cf.Save()
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
	for _, a := range m.cf.Actions() {
		p.opts = append(p.opts, pickOption{a.Summary(), a.ID})
	}
	p.opts = append(p.opts, pickOption{"＋ new action", newSentinel})
	return p
}

func (m *actionsModel) verbPicker() *picker {
	a, _ := m.cf.ByID(m.curID)
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
	a, _ := m.cf.ByID(m.curID)
	return &picker{
		title: fmt.Sprintf("delete %q?", a.Name),
		opts:  []pickOption{{"cancel", "cancel"}, {"delete", "delete"}},
	}
}

// form is the editable state bound to the editor fields.
type form struct {
	kind   string // event family, e.g. "github" — derived from the declared types
	on     string // full event type, e.g. github.pull_request.merged
	onFree string // hand-typed event type, when no source declares any
	name   string
	where  string // event data filter as "key=value, key=value"
	runner string // which runner runs
	target string // runner cwd; may template {{key}} from event data
	mode   string
	auto   string // "false" | "true"
	do     string
}

// newEditor builds the action editor over a buffer.
//
// Everything offered here comes from config rather than from a compiled-in list:
// the event types are the ones the installed sources declare, the runners are the
// ones configured, and the modes are the ones that runner says it accepts. A new
// source or a new tool shows up in this form without drover being rebuilt.
func newEditor(title string, f *form, cf *config.Config) *editor {
	e := &editor{title: title, types: cf.Types(), cf: cf}

	declared := len(e.types) > 0
	typeF := cycleField("type", e.kinds(), e.kinds(), &f.kind,
		"Which source family emits the event. Sets the subactions below.",
		func() bool { return !declared })
	subF := cycleField("subaction", e.subactionOns(f.kind), e.subactionLabels(f.kind), &f.on,
		"The exact event that triggers this action.",
		func() bool { return !declared })
	// No source declares its types (a plugin that doesn't advertise, or none
	// installed yet) — take the event type as free text rather than offering an
	// empty picker.
	freeF := inputField("on", &f.onFree, "e.g. jira.issue.created",
		"The event type to match, exactly as the source emits it.", true,
		func() bool { return declared })
	doF := areaField("do", &f.do,
		"The task prompt handed to the runner. drover frames it with event context at run time. Leave empty to use the shown default.")
	e.typeF, e.subF, e.doF = typeF, subF, doF

	runners := cf.RunnerNames()
	if len(runners) == 0 {
		runners = []string{""}
	}
	runnerF := cycleField("runner", runners, runners, &f.runner,
		"Which runner runs this action. Configure more with [[runner]] in drover.toml.", nil)
	e.runnerF = runnerF

	modeF := cycleField("mode", e.modes(f.runner), e.modes(f.runner), &f.mode,
		"How much the runner may do unattended. The values come from the chosen runner.", nil)
	e.modeF = modeF

	autoF := cycleField("auto", []string{"false", "true"}, []string{"no — wait for a human release", "yes — fire on match"}, &f.auto,
		"auto=yes runs the runner with no human gate. Only for a source whose content you control: the runner acts on event text drover did not author.", nil)

	e.fields = []*editField{
		typeF,
		subF,
		freeF,
		inputField("name", &f.name, "e.g. fix-ci",
			"A short label for this action, shown in lists and logs.", true, nil),
		inputField("where", &f.where, "repo=acme/api",
			"Only react to events whose data carries these values. Comma-separated key=value. Empty = any.", false, nil),
		runnerF,
		modeF,
		inputField("target", &f.target, "~/src/acme-api",
			"Where the runner runs. May template event data: {{dir}}, ~/src/{{repo}}. Empty runs in drover's own directory.", false, nil),
		autoF,
		doF, // the prompt textarea sits last: it's the tallest field and the focus of authoring
	}
	e.reconcile()
	return e
}

func toAction(f form) config.Action {
	on := f.on
	if strings.TrimSpace(f.onFree) != "" {
		on = strings.TrimSpace(f.onFree)
	}
	do := strings.TrimSpace(f.do)
	if do == "" { // left empty → the shown default prompt for this event
		do = defaultPrompt(on)
	}
	return config.Action{
		Name:   strings.TrimSpace(f.name),
		On:     on,
		Where:  parseWhere(f.where),
		Runner: f.runner,
		Mode:   f.mode,
		Target: strings.TrimSpace(f.target),
		Auto:   f.auto == "true",
		Do:     do,
	}
}

func fromAction(a config.Action) form {
	auto := "false"
	if a.Auto {
		auto = "true"
	}
	return form{
		kind:   kindOf(a.On),
		on:     a.On,
		onFree: a.On,
		name:   a.Name,
		where:  formatWhere(a.Where),
		runner: a.Runner,
		target: a.Target,
		mode:   a.Mode,
		auto:   auto,
		do:     a.Do,
	}
}

// parseWhere reads "repo=acme/api, label=p0" into a filter map. Malformed
// segments are dropped rather than rejected — a half-typed filter shouldn't
// block saving the rest of the action.
func parseWhere(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func formatWhere(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// kindOf is the event family: the segment before the first dot.
func kindOf(on string) string {
	if i := strings.IndexByte(on, '.'); i >= 0 {
		return on[:i]
	}
	return on
}

func firstKind(cf *config.Config) string {
	for _, t := range cf.Types() {
		return kindOf(t)
	}
	return ""
}

func firstRunner(cf *config.Config) string {
	names := cf.RunnerNames()
	if len(names) == 0 {
		return ""
	}
	return names[0]
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
// run time, so these are just the task intent. An unlisted type (any plugin's)
// falls back to the generic seed.
var defaultPrompts = map[string]string{
	"github.pull_request.merged": "A PR merged. If CI on the base branch is red, open a fix PR.",
	"github.pull_request.opened": "A PR opened. Review the diff and leave your findings as a note.",
	"github.pull_request.closed": "A PR closed without merging. Note anything that needs following up.",
	"github.issues.opened":       "An issue was opened. Triage it and propose next steps.",
	"sentry.issue.opened":        "A Sentry issue opened. Investigate the stack trace and propose a fix.",
	"shepherd.added":             "A board item was added. Handle it per its description.",
	"shepherd.updated":           "A board item changed. Reconcile the change per its description.",
	"shepherd.removed":           "A board item was removed. Clean up anything it left behind.",
	"shepherd.archived":          "A board item was archived. Wrap up any loose ends.",
}

const genericPrompt = "Handle this event using the context above, then report what you did."

func defaultPrompt(on string) string {
	if p, ok := defaultPrompts[on]; ok {
		return p
	}
	if on == "" {
		return ""
	}
	return genericPrompt
}
