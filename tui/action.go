// Package tui is drover's interactive authoring UI for the trusted action
// registry. `drover action` (no subcommand) opens a picker over the registry:
// create a new action through guided type/subaction selects with a seeded
// prompt, or edit/view/delete an existing one. The flag CLI (add|list|edit|rm)
// stays for scripting; this is the easy path.
package tui

import (
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/jwarykowski/drover/registry"
)

// newSentinel is the picker value that means "create a new action" rather than
// selecting an existing id.
const newSentinel = "\x00new"

// Run opens the registry manager and loops until the user quits (esc/ctrl+c on
// the top-level picker). Aborting a sub-form returns to the picker.
func Run(regPath string) error {
	reg, err := registry.Load(regPath)
	if err != nil {
		return err
	}
	for {
		choice, err := pickAction(reg)
		switch {
		case errors.Is(err, huh.ErrUserAborted):
			return nil
		case err != nil:
			return err
		}

		var actErr error
		if choice == newSentinel {
			actErr = createAction(reg)
		} else {
			actErr = manageAction(reg, choice)
		}
		if errors.Is(actErr, huh.ErrUserAborted) {
			continue // aborted a sub-form → back to the list
		}
		if actErr != nil {
			return actErr
		}
	}
}

// pickAction shows the registry as a select; the last row creates a new action.
func pickAction(reg *registry.Registry) (string, error) {
	opts := make([]huh.Option[string], 0, len(reg.Actions)+1)
	for _, a := range reg.Actions {
		opts = append(opts, huh.NewOption(a.Summary(), a.ID))
	}
	opts = append(opts, huh.NewOption("＋ new action", newSentinel))

	var choice string
	err := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("drover actions").
			Description("enter to open · esc to quit").
			Options(opts...).
			Value(&choice),
	)).Run()
	return choice, err
}

// manageAction offers view/edit/delete on an existing action.
func manageAction(reg *registry.Registry, id string) error {
	a, ok := reg.ByID(id)
	if !ok {
		return nil
	}
	var verb string
	err := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title(a.Name).
			Description(a.On).
			Options(
				huh.NewOption("view", "view"),
				huh.NewOption("edit", "edit"),
				huh.NewOption("delete", "delete"),
				huh.NewOption("back", "back"),
			).
			Value(&verb),
	)).Run()
	if err != nil {
		return err
	}
	switch verb {
	case "view":
		return showDetail(a)
	case "edit":
		return editAction(reg, id)
	case "delete":
		return deleteAction(reg, id)
	default:
		return nil
	}
}

// createAction runs the form for a fresh action and appends it.
func createAction(reg *registry.Registry) error {
	f := form{kind: "github", mode: registry.AutoModes[0]}
	if err := runForm(&f); err != nil {
		return err
	}
	if _, err := reg.Add(toAction(f)); err != nil {
		return err
	}
	return reg.Save()
}

// editAction runs the form pre-filled from an existing action and replaces it
// by id (mirrors the flag CLI's remove-then-append).
func editAction(reg *registry.Registry, id string) error {
	a, ok := reg.ByID(id)
	if !ok {
		return nil
	}
	f := fromAction(a)
	if err := runForm(&f); err != nil {
		return err
	}
	_ = reg.Remove(id)
	na := toAction(f)
	na.ID = id
	reg.Actions = append(reg.Actions, na)
	return reg.Save()
}

// deleteAction confirms, then removes the action by id.
func deleteAction(reg *registry.Registry, id string) error {
	a, _ := reg.ByID(id)
	var ok bool
	err := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title(fmt.Sprintf("delete %q?", a.Name)).
			Affirmative("delete").
			Negative("cancel").
			Value(&ok),
	)).Run()
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := reg.Remove(id); err != nil {
		return err
	}
	return reg.Save()
}

// form is the editable state bound to the huh fields.
type form struct {
	kind     string // event family: github | sentry | board
	on       string // full event type, e.g. github.pull_request.merged
	name     string
	repo     string
	base     string // github poll-mode branch
	source   string // github sense: forward | poll
	interval string // github poll interval, e.g. 60s
	target   string
	mode     string
	do       string
}

// runForm collects an action in two steps: pick type+subaction, then seed the
// prompt for that event type (when empty), then fill the remaining fields.
func runForm(f *form) error {
	if err := selectForm(f).Run(); err != nil {
		return err
	}
	if strings.TrimSpace(f.do) == "" {
		f.do = defaultPrompt(f.on)
	}
	return detailForm(f).Run()
}

// selectForm picks the event family and its subaction; subaction options track
// the chosen type via OptionsFunc's binding.
func selectForm(f *form) *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("type").
			Options(kindOptions()...).
			Value(&f.kind),
		huh.NewSelect[string]().
			Title("subaction").
			OptionsFunc(func() []huh.Option[string] { return subactionOptions(f.kind) }, &f.kind).
			Value(&f.on),
	))
}

// detailForm fills the remaining fields; do is pre-seeded by runForm. The
// second group holds github-only sensing knobs and hides for other families.
func detailForm(f *form) *huh.Form {
	return huh.NewForm(
		huh.NewGroup(
			huh.NewInput().Title("name").Value(&f.name).Validate(huh.ValidateNotEmpty()),
			huh.NewInput().Title("repo filter").Description("optional; owner/name").Value(&f.repo),
			huh.NewInput().Title("target dir").Description("cwd the agent runs in").Value(&f.target).Validate(huh.ValidateNotEmpty()),
			huh.NewSelect[string]().Title("mode").Options(modeOptions()...).Value(&f.mode),
			huh.NewText().Title("do").Description("the agent prompt; drover frames it further at runtime").Value(&f.do).Validate(huh.ValidateNotEmpty()),
		),
		huh.NewGroup(
			huh.NewSelect[string]().Title("source").Description("how drover watches this repo").Options(sourceOptions()...).Value(&f.source),
			huh.NewInput().Title("base branch").Description("poll mode; empty = master").Value(&f.base),
			huh.NewInput().Title("poll interval").Description("poll mode; e.g. 60s, empty = 60s").Value(&f.interval),
		).WithHideFunc(func() bool { return f.kind != "github" }),
	)
}

func toAction(f form) registry.Action {
	a := registry.Action{
		Name:   strings.TrimSpace(f.name),
		On:     f.on,
		Repo:   strings.TrimSpace(f.repo),
		Target: strings.TrimSpace(f.target),
		Mode:   f.mode,
		Do:     strings.TrimSpace(f.do),
	}
	// github-only knobs; leave empty for other families so their rows stay clean.
	if f.kind == "github" {
		a.Base = strings.TrimSpace(f.base)
		a.Source = f.source
		a.Interval = strings.TrimSpace(f.interval)
	}
	return a
}

func fromAction(a registry.Action) form {
	return form{
		kind:     kindOf(a.On),
		on:       a.On,
		name:     a.Name,
		repo:     a.Repo,
		base:     a.Base,
		source:   a.Source,
		interval: a.Interval,
		target:   a.Target,
		mode:     a.Mode,
		do:       a.Do,
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

type subaction struct {
	label string
	on    string
}

// subactions are the known event types within a family, with friendly labels.
func subactions(kind string) []subaction {
	var out []subaction
	for _, t := range registry.KnownEventTypes {
		if strings.HasPrefix(t, kind+".") {
			out = append(out, subaction{label: label(t), on: t})
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

func kindOptions() []huh.Option[string] {
	var o []huh.Option[string]
	for _, k := range kinds() {
		o = append(o, huh.NewOption(k, k))
	}
	return o
}

func subactionOptions(kind string) []huh.Option[string] {
	var o []huh.Option[string]
	for _, s := range subactions(kind) {
		o = append(o, huh.NewOption(s.label, s.on))
	}
	return o
}

func modeOptions() []huh.Option[string] {
	var o []huh.Option[string]
	for _, m := range registry.AutoModes {
		o = append(o, huh.NewOption(m, m))
	}
	return o
}

func sourceOptions() []huh.Option[string] {
	var o []huh.Option[string]
	for _, s := range registry.ValidSources {
		o = append(o, huh.NewOption(s, s))
	}
	return o
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
