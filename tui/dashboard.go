package tui

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/jwarykowski/drover/config"
	"github.com/jwarykowski/drover/daemon"
	"github.com/jwarykowski/drover/loop"
	"github.com/jwarykowski/drover/store"
)

var (
	runStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	stopStyle = lipgloss.NewStyle().Faint(true)
	warnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
)

// Version is drover's version, the single source of truth. Bump on release.
var Version = "0.1.0"

// chromeHead is the shared top of every full-screen view: the "🐕 drover" title
// on the left (identical position everywhere), an optional right-aligned status,
// then a rule.
func chromeHead(w int, right string) []string {
	left := titleStyle.Render("🐕 drover")
	top := left
	if right != "" {
		top = spread(w, left, right)
	}
	return []string{top, ruleStyle.Render(strings.Repeat("┈", w))}
}

// chromeFoot is the shared bottom: a rule then "jwarykowski/drover" on the left
// with the version flush-right, shepherd-style.
func chromeFoot(w int) []string {
	return []string{
		ruleStyle.Render(strings.Repeat("┈", w)),
		spread(w, hintStyle.Render("jwarykowski/drover"), hintStyle.Render(Version)),
	}
}

// Dashboard is the interactive control panel: start/stop the watch daemon
// in-process, filter the lanes by source, watch a live trace, and manage
// actions — all in one program. The Controller owns the daemon goroutine, so
// the watch keeps running while the user is in the embedded action manager.
func Dashboard(cfgPath string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctrl := newController(ctx, cfgPath)
	ctrl.Start() // sense from the moment the dashboard opens; `s` toggles it off
	m := dashboardModel{ctrl: ctrl, cfgPath: cfgPath, logDir: store.DefaultLogDir()}
	m.refresh()
	// ponytail: mouse capture disables native terminal text-selection; the wheel
	// scroll is worth it for the log pane. Drop WithMouseCellMotion to get select back.
	_, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	ctrl.Stop()
	return err
}

// ---- Controller: owns the in-process daemon, survives tea programs ----

type Controller struct {
	parent  context.Context
	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
	cfg     daemon.Config
	ring    *ring
	tasks   *store.FileStore // shared task store: daemon + dashboard mutate the same instance
}

func newController(parent context.Context, cfgPath string) *Controller {
	// A load failure (corrupt tasks.json) shouldn't sink the dashboard — start
	// empty and surface it in the trace; the file is rewritten on the next write.
	tasks, err := store.OpenFileStore(store.DefaultTasksPath())
	ring := newRing(500)
	if err != nil {
		tasks, _ = store.OpenFileStore("") // in-memory fallback
		ring.Logf("tasks: %v (starting empty)", err)
	}
	return &Controller{
		parent: parent,
		ring:   ring,
		tasks:  tasks,
		cfg: daemon.Config{
			ConfigPath: cfgPath,
			LogDir:     store.DefaultLogDir(),
			Agents:     1,
			Store:      tasks,
		},
	}
}

// Start launches the daemon goroutine (no-op if already running). Provenance and
// operational log both feed the ring so the dashboard can render them.
func (c *Controller) Start() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
		return
	}
	ctx, cancel := context.WithCancel(c.parent)
	done := make(chan struct{})
	c.cancel, c.done, c.running = cancel, done, true
	cfg := c.cfg
	go func() {
		defer close(done)
		if err := daemon.Run(ctx, cfg, c.ring, c.ring.Logf); err != nil {
			c.ring.Logf("daemon: %v", err)
		}
		// Natural exit (e.g. bind failure): reflect stopped, unless a newer run
		// has already taken over (c.done swapped by a restart).
		c.mu.Lock()
		if c.done == done {
			c.running = false
		}
		c.mu.Unlock()
	}()
}

// Stop cancels the daemon and BLOCKS until its goroutine returns — a restart
// must not launch before the old webhook port/goroutine is released.
func (c *Controller) Stop() {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return
	}
	cancel, done := c.cancel, c.done
	c.running = false
	c.mu.Unlock()
	cancel()
	<-done
}

// Status snapshots the daemon state for the view.
//
// There is no per-source daemon control any more: every configured source runs
// whenever the watch runs, and the dashboard's source selection is purely a
// view filter. Selecting one no longer restarts the daemon.
func (c *Controller) Status() (running bool, lines []logEntry) {
	c.mu.Lock()
	running = c.running
	c.mu.Unlock()
	return running, c.ring.Snapshot()
}

// ---- ring: mutex-guarded capped line buffer bridging daemon → UI ----

// logEntry is one log line plus the wall-clock time it was recorded, so the
// view can render a timestamp column.
type logEntry struct {
	ts   time.Time
	text string
}

type ring struct {
	mu  sync.Mutex
	buf []logEntry
	cap int
}

func newRing(capacity int) *ring { return &ring{cap: capacity} }

// Write implements io.Writer for the provenance stream; each newline-separated
// record becomes a line.
func (r *ring) Write(p []byte) (int, error) {
	for _, ln := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		r.add(ln)
	}
	return len(p), nil
}

// Logf is the operational-log sink.
func (r *ring) Logf(format string, a ...any) { r.add(fmt.Sprintf(format, a...)) }

func (r *ring) add(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, logEntry{ts: time.Now(), text: line})
	if len(r.buf) > r.cap {
		r.buf = r.buf[len(r.buf)-r.cap:]
	}
}

func (r *ring) Snapshot() []logEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]logEntry, len(r.buf))
	copy(out, r.buf)
	return out
}

// ---- dashboard model ----

const (
	modeMain = iota
	modeSourcePick
	modeActions
	modeJobDetail
	modeJobLog
	modeLog
)

// laneNames maps a lane index (the selected column) to its lane key.
var laneNames = []string{"held", "running", "done"}

type tickMsg struct{}

func tick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

type dashboardModel struct {
	ctrl    *Controller
	cfgPath string
	logDir  string // per-job agent stream logs, read for the job detail view

	mode      int
	running   bool
	source    string // lane filter; empty = every source, grouped
	lines     []logEntry
	acts      []config.Action
	items     []loop.Task // live runs, for the kanban lanes
	laneIdx   int         // selected lane column: 0 held, 1 running, 2 done
	hcursor   int         // cursor within the selected lane
	logScroll int         // lines scrolled up from the tail; 0 = follow newest

	sources []string // the sources that have actually raised runs
	pcursor int

	job       loop.Task // snapshot of the job open in modeJobDetail
	jobLog    []jobRow  // its parsed agent stream log, refreshed live
	jobScroll int       // lines scrolled up from the tail of the job log

	actionUI *actionsModel // embedded action manager when mode == modeActions

	w, h int
}

// selectedLane is the item list for the currently selected lane column.
func (m dashboardModel) selectedLane() []loop.Task { return m.lane(laneNames[m.laneIdx]) }

// laneCursor returns the live cursor for lane i, or -1 when i is not selected
// (so only the selected column shows a cursor).
func (m dashboardModel) laneCursor(i int) int {
	if m.laneIdx == i {
		return m.hcursor
	}
	return -1
}

func (m *dashboardModel) refresh() {
	m.running, m.lines = m.ctrl.Status()
	if cf, err := config.Load(m.cfgPath); err == nil {
		m.acts = cf.Actions()
	}
	if items, err := m.ctrl.tasks.List(context.Background(), loop.Filter{IncludeDone: true}); err == nil {
		// Include archived (completed) runs so the done lane stays reviewable — a
		// done agentic task is archived off the live list but its log lives on.
		m.items = m.forSource(append(items, m.ctrl.tasks.Archived()...))
	}
	if sel := m.selectedLane(); m.hcursor >= len(sel) {
		m.hcursor = max(0, len(sel)-1)
	}
	if max := max(0, m.logLineCount()-1); m.logScroll > max {
		m.logScroll = max
	}
	// Keep the open job live: re-sync its snapshot from the store so the detail
	// view reflects status/verdict changes (go → running → done) as the daemon
	// makes them, and tail its log while the log window is up.
	if (m.mode == modeJobDetail || m.mode == modeJobLog) && m.job.ID != "" {
		for _, it := range m.items {
			if it.ID == m.job.ID {
				m.job = it
				break
			}
		}
		if m.mode == modeJobLog {
			m.jobLog = m.readJobLog(m.job.ID)
		}
	}
}

// forSource narrows the runs to the selected source. An empty selection is the
// all-sources view (no filter — the lanes group by source instead).
func (m dashboardModel) forSource(items []loop.Task) []loop.Task {
	if m.source == "" {
		return items
	}
	out := items[:0]
	for _, it := range items {
		if it.Source == m.source {
			out = append(out, it)
		}
	}
	return out
}

// allSources reports whether the dashboard is in the grouped all-sources view.
func (m dashboardModel) allSources() bool { return m.source == "" }

// knownSources are the sources that have actually raised a run, sorted — the
// picker lists these rather than the configured rows, so it always reflects
// what is really on the lanes.
func (m dashboardModel) knownSources() []string {
	seen := map[string]bool{}
	var out []string
	for _, it := range m.items {
		if it.Source != "" && !seen[it.Source] {
			seen[it.Source] = true
			out = append(out, it.Source)
		}
	}
	sort.Strings(out)
	return out
}

// lane groups the runs into a pipeline column. held = parked or released but
// not yet claimed (hold/go); running = claimed; done = finished.
func (m dashboardModel) lane(which string) []loop.Task {
	var out []loop.Task
	for _, it := range m.items {
		switch {
		case which == "done" && it.Done:
			out = append(out, it)
		case it.Done:
			// finished items only belong to the done lane
		case which == "running" && it.Status == "running":
			out = append(out, it)
		case which == "held" && (it.Status == "hold" || it.Status == "go"):
			out = append(out, it)
		}
	}
	// Order by source so the all-sources view can group contiguously; runs with
	// no source sort last. Stable, so a single source keeps insertion order.
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := out[i].Source, out[j].Source
		if (si == "") != (sj == "") {
			return sj == ""
		}
		return si < sj
	})
	return out
}

func (m dashboardModel) Init() tea.Cmd { return tick() }

func (m dashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		if m.mode == modeActions && m.actionUI != nil {
			am, _ := m.actionUI.Update(msg)
			m.actionUI = am.(*actionsModel)
		}
		return m, nil
	case tickMsg:
		m.refresh() // keep status/trace live even while in the action manager
		return m, tick()
	case tea.MouseMsg:
		if m.mode == modeActions && m.actionUI != nil {
			return m.delegateActions(msg)
		}
		if m.mode == modeLog {
			switch msg.Button {
			case tea.MouseButtonWheelUp:
				m.logScroll++
				return m.clampLog(), nil
			case tea.MouseButtonWheelDown:
				m.logScroll--
				return m.clampLog(), nil
			}
		}
		if m.mode == modeJobLog {
			switch msg.Button {
			case tea.MouseButtonWheelUp:
				m.jobScroll++
				return m.clampJob(), nil
			case tea.MouseButtonWheelDown:
				m.jobScroll--
				return m.clampJob(), nil
			}
		}
		return m, nil
	case tea.KeyMsg:
		switch m.mode {
		case modeActions:
			return m.delegateActions(msg)
		case modeSourcePick:
			return m.updateSourcePick(msg)
		case modeJobDetail:
			return m.updateJobDetail(msg)
		case modeJobLog:
			return m.updateJobLog(msg)
		case modeLog:
			return m.updateLog(msg)
		default:
			return m.updateMain(msg)
		}
	}
	// forward huh's own (non-key) cmds to the embedded action manager
	if m.mode == modeActions && m.actionUI != nil {
		return m.delegateActions(msg)
	}
	return m, nil
}

// delegateActions forwards a message to the embedded action manager and pops
// back to the main view when it's done.
func (m dashboardModel) delegateActions(msg tea.Msg) (tea.Model, tea.Cmd) {
	am, cmd := m.actionUI.Update(msg)
	m.actionUI = am.(*actionsModel)
	if m.actionUI.done {
		m.mode = modeMain
		m.actionUI = nil
		m.refresh()
	}
	return m, cmd
}

func (m dashboardModel) updateMain(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		if m.hcursor > 0 {
			m.hcursor--
		}
	case "down", "j":
		if m.hcursor < len(m.selectedLane())-1 {
			m.hcursor++
		}
	case "left":
		if m.laneIdx > 0 {
			m.laneIdx--
			m.hcursor = 0
		}
	case "right":
		if m.laneIdx < len(laneNames)-1 {
			m.laneIdx++
			m.hcursor = 0
		}
	case "enter", "d":
		// open the selected job's detail view (any lane); `d` mirrors shepherd.
		if sel := m.selectedLane(); m.hcursor < len(sel) {
			m.job = sel[m.hcursor]
			m.jobScroll = 0
			m.mode = modeJobDetail
		}
	case "x":
		// delete the selected job and its log (shepherd's del key).
		if sel := m.selectedLane(); m.hcursor < len(sel) {
			id := sel[m.hcursor].ID
			_ = m.ctrl.tasks.Delete(context.Background(), id)
			m.removeJobLog(id)
			m.refresh()
		}
	case "g":
		// release the selected HELD task hold→go; the daemon claims it next tick.
		if m.laneIdx == 0 {
			if held := m.lane("held"); m.hcursor < len(held) {
				_ = m.ctrl.tasks.SetStatus(context.Background(), held[m.hcursor].ID, "go")
				m.refresh()
			}
		}
	case "l":
		// open the daemon trace log as its own full window.
		m.logScroll = 0
		m.mode = modeLog
	case "a":
		m.actionUI = newActionsModel(m.cfgPath, false)
		// seed the embedded form's size; no WindowSizeMsg fires on a mode switch.
		am, _ := m.actionUI.Update(tea.WindowSizeMsg{Width: m.w, Height: m.h})
		m.actionUI = am.(*actionsModel)
		m.mode = modeActions
		return m, m.actionUI.Init()
	case "s":
		if m.running {
			m.ctrl.Stop()
		} else {
			m.ctrl.Start()
		}
		m.refresh()
	case "b":
		m.sources = m.knownSources()
		m.pcursor = pickerIndex(m.sources, m.source)
		m.mode = modeSourcePick
	}
	return m.clampLog(), nil
}

// updateLog handles the full-window daemon trace log: scroll it, or back to the
// board.
func (m dashboardModel) updateLog(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "l":
		m.mode = modeMain
		return m, nil
	case "up", "k":
		m.logScroll++
	case "down", "j":
		m.logScroll--
	case "pgup":
		m.logScroll += 5
	case "pgdown":
		m.logScroll -= 5
	case "end":
		m.logScroll = 0
	}
	return m.clampLog(), nil
}

// clampLog keeps the log scroll offset inside the buffer.
func (m dashboardModel) clampLog() dashboardModel {
	if m.logScroll < 0 {
		m.logScroll = 0
	}
	if max := max(0, m.logLineCount()-1); m.logScroll > max {
		m.logScroll = max
	}
	return m
}

func (m dashboardModel) updateSourcePick(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.mode = modeMain
	case "up", "k":
		if m.pcursor > 0 {
			m.pcursor--
		}
	case "down", "j":
		if m.pcursor < len(m.sources) { // row 0 = "all sources", then one each
			m.pcursor++
		}
	case "enter":
		if m.pcursor == 0 {
			m.source = ""
		} else {
			m.source = m.sources[m.pcursor-1]
		}
		m.hcursor = 0
		m.refresh()
		m.mode = modeMain
	}
	return m, nil
}

// updateJobDetail handles the read-only job detail: open the full-window log,
// release/restart/delete the run, or back out.
func (m dashboardModel) updateJobDetail(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.mode = modeMain
		return m, nil
	case "l":
		// open the agent conversation log as its own full window.
		m.jobLog = m.readJobLog(m.job.ID)
		m.jobScroll = 0
		m.mode = modeJobLog
	case "g":
		// release a held job straight from its detail view.
		if releasable(m.job) {
			_ = m.ctrl.tasks.SetStatus(context.Background(), m.job.ID, "go")
			m.job.Status = "go"
		}
	case "r":
		// restart: re-queue this run at hold and clear its stale log.
		_ = m.ctrl.tasks.Restart(context.Background(), m.job.ID)
		m.removeJobLog(m.job.ID)
		m.job.Done, m.job.Status, m.job.Note = false, "hold", ""
		m.jobLog, m.jobScroll = nil, 0
		m.refresh()
	case "x":
		// delete: drop this run and its log, back to the lanes.
		_ = m.ctrl.tasks.Delete(context.Background(), m.job.ID)
		m.removeJobLog(m.job.ID)
		m.mode, m.jobScroll = modeMain, 0
		m.refresh()
		return m, nil
	}
	return m, nil
}

// updateJobLog handles the full-window claude log: scroll it, or back to the
// job detail.
func (m dashboardModel) updateJobLog(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "l":
		m.mode = modeJobDetail
		return m, nil
	case "up", "k":
		m.jobScroll++
	case "down", "j":
		m.jobScroll--
	case "pgup":
		m.jobScroll += 5
	case "pgdown":
		m.jobScroll -= 5
	case "end":
		m.jobScroll = 0
	}
	return m.clampJob(), nil
}

// releasable reports whether a job is a parked (held) run awaiting release.
func releasable(j loop.Task) bool { return !j.Done && j.Status == "hold" }

// clampJob keeps the job-log scroll offset inside the pane.
func (m dashboardModel) clampJob() dashboardModel {
	if m.jobScroll < 0 {
		m.jobScroll = 0
	}
	if max := max(0, len(renderJobRows(m.jobLog, innerWidth(m.w)))-1); m.jobScroll > max {
		m.jobScroll = max
	}
	return m
}

func (m dashboardModel) View() string {
	if m.mode == modeActions && m.actionUI != nil {
		return m.actionsView()
	}
	if m.mode == modeSourcePick {
		return m.sourcePickView()
	}
	if m.mode == modeJobDetail {
		return m.jobDetailView()
	}
	if m.mode == modeJobLog {
		return m.jobLogView()
	}
	if m.mode == modeLog {
		return m.logView()
	}
	return m.mainView()
}

// actionsView swaps the board's centre pane for the action form, keeping the
// dashboard's own header and footer so the title stays put.
func (m dashboardModel) actionsView() string {
	w := innerWidth(m.w)
	return shell(w, m.h, m.statusLine(), m.actionUI.bodyLines(w, fillCount(m.h, 4)))
}

func (m dashboardModel) mainView() string {
	w := innerWidth(m.w)
	hints := hintStyle.Render("↑/↓/←/→ move · d detail · g release · x del · l log · s start/stop · b source · a actions · q quit")

	laneH := max(3, fillCount(m.h, 4)-1) // body rows (head+foot=4), -1 for the hints line
	body := m.laneRows(w, laneH)
	body = append(body, hints)
	return shell(w, m.h, m.statusLine(), body)
}

// laneRows renders the three pipeline columns (held/running/done) into laneH rows.
func (m dashboardModel) laneRows(w, laneH int) []string {
	sep := " " + ruleStyle.Render("│") + " "
	colW := (w - 6) / 3 // two 3-wide separators
	lastW := w - 2*colW - 6

	held := m.laneCol("held", "held", m.laneCursor(0), laneH, colW, m.laneIdx == 0)
	run := m.laneCol("running", "running", m.laneCursor(1), laneH, colW, m.laneIdx == 1)
	done := m.laneCol("done", "done", m.laneCursor(2), laneH, lastW, m.laneIdx == 2)

	rows := make([]string, laneH)
	for i := 0; i < laneH; i++ {
		rows[i] = cell(at(held, i), colW) + sep + cell(at(run, i), colW) + sep + cell(at(done, i), lastW)
	}
	return rows
}

// eventSource is the trigger an item's run came from — the `on:` type of its
// registry action (e.g. "shepherd.added", "github.pull_request.merged"). Empty if
// the action is unknown. Shown as a card subline.
func (m dashboardModel) eventSource(it loop.Task) string {
	for _, a := range m.acts {
		if a.ID == it.Action {
			return a.On
		}
	}
	return ""
}

// laneCol builds one lane, windowed to laneH rows: a "title (n)" header then the
// items, each a card ("▸ label #id") plus a dim source subline. cursor >= 0
// marks the selected card and the window follows it; overflow shows "… +N".
// focused bolds the header to show it's the arrow-key target.
func (m dashboardModel) laneCol(which, title string, cursor, laneH, w int, focused bool) []string {
	items := m.lane(which)
	head := sectionTitle(fmt.Sprintf("%s (%d)", title, len(items)), focused)
	if len(items) == 0 {
		return []string{head, hintStyle.Render("(none)")}
	}
	vis := max(1, laneH-1)    // rows below the header for item blocks
	grouped := m.allSources() // group by source sub-header across all sources

	// each item renders as a block: an optional board sub-header (first item of a
	// group in the all-boards view), the card — label flush-left, id flush-right —
	// then its event source subline.
	block := func(i int) []string {
		var lines []string
		if grouped && (i == 0 || items[i].Source != items[i-1].Source) {
			lines = append(lines, hintStyle.Render(sourceGroupLabel(items[i].Source)))
		}
		id := keyStyle.Render("#" + shortID(items[i].ID))
		label := truncate(itemLabel(items[i]), max(1, w-lipgloss.Width(id)-1))
		if i == cursor {
			label = valStyle.Render(label)
		}
		lines = append(lines, spread(w, label, id))
		if src := m.eventSource(items[i]); src != "" {
			lines = append(lines, hintStyle.Render("  ↳ "+src))
		}
		return lines
	}

	// pick the first visible item so the cursor's block stays on screen.
	start := 0
	if cursor >= 0 {
		start = cursor
		rows := len(block(cursor))
		for i := cursor - 1; i >= 0; i-- {
			if r := len(block(i)); rows+r <= vis {
				rows += r
				start = i
			} else {
				break
			}
		}
	}

	out := []string{head}
	rows, i := 0, start
	for ; i < len(items); i++ {
		b := block(i)
		if rows+len(b) > vis {
			break
		}
		out = append(out, b...)
		rows += len(b)
	}
	if extra := len(items) - i; extra > 0 {
		marker := hintStyle.Render(fmt.Sprintf("  … +%d", extra))
		if rows >= vis && len(out) > 1 {
			out[len(out)-1] = marker // reuse the last row rather than overflow
		} else {
			out = append(out, marker)
		}
	}
	return out
}

// tsWidth is the fixed width of the timestamp column ("15:04:05").
const tsWidth = 8

// renderLog formats one entry into its display rows: a timestamp column, a
// divider, then the message word-wrapped into the remaining width. Wrapped
// continuation rows leave the timestamp column blank so the text stays aligned.
func renderLog(e logEntry, w int) []string {
	textW := max(10, w-tsWidth-3) // 3 = " │ "
	wrapped := strings.Split(ansi.Wrap(e.text, textW, ""), "\n")
	rows := make([]string, 0, len(wrapped))
	for i, ln := range wrapped {
		col := e.ts.Format("15:04:05")
		if i > 0 {
			col = strings.Repeat(" ", tsWidth)
		}
		rows = append(rows, stopStyle.Render(col)+ruleStyle.Render(" │ ")+ln)
	}
	return rows
}

// logLineCount is the total wrapped rows the log occupies at the current width —
// the scroll ceiling, since one entry can span several rows.
func (m dashboardModel) logLineCount() int {
	w := innerWidth(m.w)
	n := 0
	for _, e := range m.lines {
		n += len(renderLog(e, w))
	}
	return n
}

// logView is the full-window daemon trace: the watch log wrapped into a
// timestamp column. logScroll offsets the window up from the tail; 0 follows
// the newest line.
func (m dashboardModel) logView() string {
	w := innerWidth(m.w)
	rule := ruleStyle.Render(strings.Repeat("┈", w))
	head := []string{titleStyle.Render("log"), rule}
	foot := []string{rule, hintStyle.Render("↑/↓ scroll · esc back")}

	paneH := m.fillRows(len(head) + len(foot))
	if len(m.lines) == 0 {
		body := pad([]string{hintStyle.Render("(no activity yet — press s to start)")}, paneH)
		return frame(head, body, foot)
	}
	var rows []string
	for _, e := range m.lines {
		rows = append(rows, renderLog(e, w)...)
	}
	scroll := m.logScroll
	if max := max(0, len(rows)-paneH); scroll > max {
		scroll = max
	}
	end := len(rows) - scroll
	start := max(0, end-paneH)
	if scroll > 0 {
		head[0] += hintStyle.Render(fmt.Sprintf("  ↑%d · end to tail", scroll))
	}
	body := pad(rows[start:end], paneH)
	return frame(head, body, foot)
}

// sectionTitle renders a pane header: bold when it holds focus, faint otherwise.
func sectionTitle(s string, focused bool) string {
	if focused {
		return titleStyle.Render(s)
	}
	return keyStyle.Render(s)
}

// sourceGroupLabel is the dim sub-header dividing one source's runs from the
// next in the all-sources view.
func sourceGroupLabel(source string) string {
	name := source
	if name == "" {
		name = "unattributed"
	}
	label := "─ " + name + " "
	if n := 18 - lipgloss.Width(label); n > 0 {
		label += strings.Repeat("─", n)
	}
	return label
}

func itemLabel(it loop.Task) string {
	if s := strings.TrimSpace(it.Text); s != "" {
		return s
	}
	return it.Action
}

func shortID(id string) string {
	if len(id) > 4 {
		return id[:4]
	}
	return id
}

// metaField renders a "key      value" row for the board/job detail views.
func metaField(k, v string) string {
	return fmt.Sprintf("%s  %s", keyStyle.Render(fmt.Sprintf("%-8s", k)), v)
}

// spread places left and right on one line, right-aligned to width w.
func spread(w int, left, right string) string {
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// at returns lines[i] or "" past the end.
func at(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return ""
}

// cell truncates s to width and right-pads with spaces to fill the column.
func cell(s string, w int) string {
	s = truncate(s, w)
	if gap := w - lipgloss.Width(s); gap > 0 {
		s += strings.Repeat(" ", gap)
	}
	return s
}

func (m dashboardModel) sourcePickView() string {
	w := innerWidth(m.w)
	rule := ruleStyle.Render(strings.Repeat("┈", w))
	head := []string{titleStyle.Render("filter by source"), rule}
	foot := []string{rule, hintStyle.Render("↑/↓ move · enter select · esc back")}

	// mark the row the lanes are filtered to: ● on the selected source, or on
	// "all sources" when nothing is pinned (the default).
	dot := func(on bool) string {
		if on {
			return runStyle.Render("● ")
		}
		return "  "
	}
	arrow := func(on bool) string {
		if on {
			return valStyle.Render("▸ ")
		}
		return "  "
	}

	// row 0: the all-sources option.
	body := []string{truncate(fmt.Sprintf("%s%sall sources", arrow(m.pcursor == 0), dot(m.source == "")), w)}
	for i, name := range m.sources {
		open, total := m.sourceCounts(name)
		counts := keyStyle.Render(fmt.Sprintf("%d/%d", open, total))
		body = append(body, truncate(fmt.Sprintf("%s%s%s  %s",
			arrow(i+1 == m.pcursor), dot(m.source != "" && name == m.source), name, counts), w))
	}
	body = pad(body, m.fillRows(len(head)+len(foot)))
	return frame(head, body, foot)
}

// sourceCounts is open/total runs raised by a source, for the picker rows.
// Counted over the unfiltered set so the numbers don't change with the filter.
func (m dashboardModel) sourceCounts(name string) (open, total int) {
	for _, it := range m.items {
		if it.Source != name {
			continue
		}
		total++
		if !it.Done {
			open++
		}
	}
	return open, total
}

// jobDetailView is the read-only detail for a selected job: its verdict/status
// fields. The conversation log opens as its own full window (`l`).
func (m dashboardModel) jobDetailView() string {
	w := innerWidth(m.w)
	rule := ruleStyle.Render(strings.Repeat("┈", w))
	j := m.job

	status := j.Status
	if j.Done {
		status = "done"
	}
	if status == "" {
		status = "open"
	}

	back := "l log · r restart · x delete · esc back"
	if releasable(j) {
		back = "l log · g release · r restart · x delete · esc back"
	}
	head := []string{titleStyle.Render("job · " + itemLabel(j)), rule}
	foot := []string{rule, hintStyle.Render(back)}

	body := []string{
		metaField("status", valStyle.Render(status)),
		metaField("action", keyStyle.Render(j.Action)),
		metaField("id", keyStyle.Render(shortID(j.ID))),
	}
	// The verdict note can be long — wrap it under a blank key column.
	if n := strings.TrimSpace(j.Note); n != "" {
		segs := strings.Split(ansi.Wrap(n, max(10, w-10), ""), "\n")
		for i, seg := range segs {
			k := "verdict"
			if i > 0 {
				k = ""
			}
			body = append(body, metaField(k, seg))
		}
	}
	body = pad(body, m.fillRows(len(head)+len(foot)))
	return frame(head, body, foot)
}

// jobLogView is the full-window claude conversation log for the open job: a
// scrollable, columnar dump tailed live. jobScroll offsets up from the tail.
func (m dashboardModel) jobLogView() string {
	w := innerWidth(m.w)
	rule := ruleStyle.Render(strings.Repeat("┈", w))
	head := []string{titleStyle.Render("output · " + itemLabel(m.job)), rule}
	foot := []string{rule, hintStyle.Render("↑/↓ scroll · esc back")}

	paneH := m.fillRows(len(head) + len(foot))
	rows := renderJobRows(m.jobLog, w)
	if len(rows) == 0 {
		rows = []string{hintStyle.Render("(no log yet — the run may not have started)")}
	}
	scroll := m.jobScroll
	if max := max(0, len(rows)-paneH); scroll > max {
		scroll = max
	}
	end := len(rows) - scroll
	start := max(0, end-paneH)
	if scroll > 0 {
		head[0] += hintStyle.Render(fmt.Sprintf("  ↑%d · end to tail", scroll))
	}

	body := pad(rows[start:end], paneH)
	return frame(head, body, foot)
}

// jobRow is one rendered log event: a capture timestamp, a short event label,
// and the detail text (wrapped at display time).
type jobRow struct {
	ts    string
	event string
	text  string
}

// renderJobRows lays the log out in three columns — timestamp, event, detail —
// wrapping the detail and blanking the ts/event columns on continuation rows.
// The event column is sized to the widest label present so the detail sits as
// far left as it can.
func renderJobRows(rows []jobRow, w int) []string {
	evW := 0
	for _, r := range rows {
		if l := len(r.event); l > evW {
			evW = l
		}
	}
	textW := max(10, w-tsWidth-evW-2)
	var out []string
	for _, r := range rows {
		for i, seg := range strings.Split(ansi.Wrap(r.text, textW, ""), "\n") {
			ts, ev := "", ""
			if i == 0 {
				ts, ev = r.ts, r.event
			}
			out = append(out, stopStyle.Render(fmt.Sprintf("%-*s", tsWidth, ts))+" "+
				keyStyle.Render(fmt.Sprintf("%-*s", evW, ev))+" "+seg)
		}
	}
	return out
}

// jobLogPath is a job's on-disk stream-log file, or "" when unaddressable.
func (m dashboardModel) jobLogPath(id string) string {
	if m.logDir == "" || id == "" {
		return ""
	}
	return filepath.Join(m.logDir, id+".jsonl")
}

// readJobLog reads and parses a job's claude stream log; nil if none yet.
func (m dashboardModel) readJobLog(id string) []jobRow {
	p := m.jobLogPath(id)
	if p == "" {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	return formatJobLog(b)
}

// removeJobLog deletes a job's stream log (on delete/restart); no-op if absent.
func (m dashboardModel) removeJobLog(id string) {
	if p := m.jobLogPath(id); p != "" {
		_ = os.Remove(p)
	}
}

// formatJobLog turns the stamped stream log (one "15:04:05\t<json>" line per
// event, with stderr passing through raw) into display rows.
func formatJobLog(raw []byte) []jobRow {
	var out []jobRow
	for _, ln := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		ts, rest := "", ln
		if i := strings.IndexByte(ln, '\t'); i >= 0 {
			ts, rest = ln[:i], ln[i+1:]
		}
		out = append(out, formatStreamEvent(ts, rest)...)
	}
	return out
}

// streamEvent is the subset of a claude stream-json event we render.
type streamEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Result  string `json:"result"`
	Message struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
}

func formatStreamEvent(ts, line string) []jobRow {
	var e streamEvent
	if json.Unmarshal([]byte(line), &e) != nil {
		return []jobRow{{ts, "stderr", line}} // not JSON — a stderr diagnostic
	}
	switch e.Type {
	case "system":
		return []jobRow{{ts, "system", cmp.Or(e.Subtype, "event")}}
	case "assistant":
		var out []jobRow
		for _, c := range e.Message.Content {
			switch c.Type {
			case "text":
				if t := strings.TrimSpace(c.Text); t != "" {
					out = append(out, jobRow{ts, "agent", t})
				}
			case "tool_use":
				out = append(out, jobRow{ts, "tool", c.Name + " " + compactJSON(c.Input)})
			}
		}
		return out
	case "user":
		return []jobRow{{ts, "tool", "← result"}}
	case "result":
		return []jobRow{{ts, "result", cmp.Or(e.Subtype, "?") + ": " + strings.TrimSpace(e.Result)}}
	default:
		return []jobRow{{ts, e.Type, ""}}
	}
}

// compactJSON renders a raw JSON value on one line, truncated for a log row.
func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return string(raw)
	}
	s := buf.String()
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}

// expandTilde replaces a leading ~ with the user's home dir, for on-disk checks.
func expandTilde(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + strings.TrimPrefix(p, "~")
		}
	}
	return p
}

// shell is the one wrapper every full-screen view goes through: the shared
// "🐕 drover" header (with optional right-aligned status), the body padded to
// fill, and the footer pinned to the bottom row. w is the inner content width
// (innerWidth), h the raw terminal height. Routing the dashboard, the embedded
// action form, and the standalone action manager through this is what keeps the
// title and footer in the same place across all of them.
func shell(w, h int, right string, body []string) string {
	head := chromeHead(w, right)
	foot := chromeFoot(w)
	return frame(head, pad(body, fillCount(h, len(head)+len(foot))), foot)
}

// frame stacks head + body + foot and pads to fill so the footer sits at the
// terminal's bottom row.
func frame(head, body, foot []string) string {
	lines := make([]string, 0, len(head)+len(body)+len(foot))
	lines = append(lines, head...)
	lines = append(lines, body...)
	lines = append(lines, foot...)
	return lipgloss.NewStyle().Padding(1, 2).Render(strings.Join(lines, "\n"))
}

func (m dashboardModel) statusLine() string {
	status := stopStyle.Render("○ stopped")
	if m.running {
		status = runStyle.Render("● running")
	}
	source := "all sources"
	if m.source != "" {
		source = m.source
	}
	return fmt.Sprintf("%s   %s   %s",
		status,
		valStyle.Render(source),
		keyStyle.Render(fmt.Sprintf("%d action(s)", len(m.acts))),
	)
}

// fillRows is how many body lines to emit so the footer sits at the bottom:
// terminal height minus the vertical padding (2) and the chrome (head + foot).
func (m dashboardModel) fillRows(chrome int) int { return fillCount(m.h, chrome) }

// fillCount is body rows for terminal height h minus the outer padding (2) and
// the chrome lines, so the footer lands on the bottom row. Shared by every view.
func fillCount(h, chrome int) int {
	if h <= 0 {
		h = 24
	}
	if n := h - 2 - chrome; n > 1 {
		return n
	}
	return 1
}

func pad(lines []string, n int) []string {
	for len(lines) < n {
		lines = append(lines, "")
	}
	return lines
}

// pickerIndex maps the selected source to a picker row: 0 = "all sources",
// otherwise the matching source's row (its index + 1). Unknown/empty → all.
func pickerIndex(names []string, source string) int {
	if source == "" {
		return 0
	}
	for i, n := range names {
		if n == source {
			return i + 1
		}
	}
	return 0
}

func innerWidth(w int) int {
	if w <= 0 {
		w = 80
	}
	w -= 4 // horizontal padding (2 each side)
	if w < 10 {
		w = 10
	}
	return w
}

// truncate clips s to display width w, ANSI-aware so styled lane/status strings
// never get sliced mid-escape.
func truncate(s string, w int) string {
	if w <= 1 || lipgloss.Width(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, "…")
}
