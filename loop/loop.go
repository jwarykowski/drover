// Package loop defines drover's four seams and wires them into a one-shot loop.
//
// drover is the sense to assemble-context to act loop around shepherd. It never
// touches the todo file; it speaks shepherd's CLI through the Store seam. Every
// other component is an implementation behind one of the interfaces here.
package loop

import (
	"context"
	"time"
)

// Event is something that happened worth reacting to: a merge, an issue, a CI
// failure. Sensing means structured events, not perception. The envelope is
// CloudEvents-shaped so policies match on Type and dedup on ID.
type Event struct {
	ID     string  // stable per logical event — the dedup key
	Type   string  // hierarchical, e.g. "github.pull_request.merged", "board.updated"
	Source string  // instance identity, e.g. "github/acme/api"
	Data   Payload // typed, source-specific (see the Payload impls below)
	At     time.Time
}

// Payload is the typed body of an Event — a sealed sum type so policies switch
// on the concrete shape instead of digging in a map. Sources normalise vendor
// JSON into these at the edge; a policy never sees raw gh/Sentry shapes.
type Payload interface{ isPayload() }

// Signal is the common "something happened upstream, maybe park a task" shape
// every ingress source normalises to. Extra holds source-specific bits (labels,
// sha, level) that only an escape-hatch policy reads.
type Signal struct {
	Repo  string
	Title string
	URL   string
	Key   string // stable per-resource key, feeds the Event ID
	Extra map[string]any
}

// BoardChange carries a shepherd item that changed — emitted by the watch
// stream, consumed by the release policy.
type BoardChange struct{ Item Item }

func (Signal) isPayload()      {}
func (BoardChange) isPayload() {}

// Item mirrors a shepherd item as emitted by `shepherd list --json`.
// ID is stable across board reorders; Index is not, so mutations address ID.
type Item struct {
	ID        string `json:"id"`
	Index     int    `json:"index"`
	Done      bool   `json:"done"`
	Priority  string `json:"priority,omitempty"`
	Text      string `json:"text,omitempty"`
	Category  string `json:"category,omitempty"`
	Created   string `json:"created,omitempty"`
	Completed string `json:"completed,omitempty"`
	Due       string `json:"due,omitempty"`
	Link      string `json:"link,omitempty"`
	Status    string `json:"status,omitempty"`
	Agentic   bool   `json:"agentic,omitempty"` // task raised and driven by drover
	Action    string `json:"action,omitempty"`  // opaque allowlist action to fire on release
	Note      string `json:"note,omitempty"`
}

// Filter narrows a board read to the relevant slice — the "attention" a policy
// reasons over, expressed as a WHERE clause rather than the whole board.
type Filter struct {
	Category    string
	Text        string
	IncludeDone bool
}

// Spec is a request to create an item, mapped onto shepherd's add syntax.
type Spec struct {
	Text     string
	Category string
	Priority string // H, M or L
	Status   string // named status, e.g. "hold"; empty means default/open
	Agentic  bool   // mark the task agent-driven (shepherd's `agentic` flag)
	Action   string // opaque action name the agent fires on release (shepherd's `action:`)
	Due      string
	Link     string
	Note     string
}

// Action is something a policy decides to do. AddTask is the only Action in
// Phase 1; later phases add RunAction and friends behind the same seam.
type Action interface{ isAction() }

// AddTask asks the executor to create an item from Spec.
type AddTask struct{ Spec Spec }

func (AddTask) isAction() {}

// RunAction fires a named action from drover's trusted config allowlist. The
// board (and any policy) references an action by Name only — the command body
// lives in config, never in an item field. Args are typed, validated values
// substituted into the config template as whole argv elements, never shell.
type RunAction struct {
	Name   string            // allowlist key, e.g. "fix-ci"
	Args   map[string]string // substituted into the config command template
	Reason string            // why this fired (event kind, task id) — for provenance
}

func (RunAction) isAction() {}

// SetStatus transitions an existing item by id — the loop's way to close or
// advance a task it created, not just spawn new ones. "done" is terminal; any
// other value sets a named status (e.g. "running").
type SetStatus struct {
	ID     string
	Status string
}

func (SetStatus) isAction() {}

// RunAgent fires an agent action from drover's trusted registry. The board
// references the action by ActionID (a registry id) only — the prompt, target
// and permission mode live in the registry, never in a board field. The
// executor resolves the id, runs the agent, and reconciles TaskID from the
// agent's structured verdict.
type RunAgent struct {
	ActionID string            // registry action id resolved by the executor
	Args     map[string]string // event context substituted into the wrapping prompt
	TaskID   string            // shepherd item to reconcile from the verdict
}

func (RunAgent) isAction() {}

// Context is the bundle handed to Policy.Decide. Phase 0+1 fills Event and Board
// only; Profile, Similar, History and a real Tenant are later context tiers and
// stay zero-valued for now.
type Context struct {
	Event  Event
	Board  []Item // the relevant slice, not the whole board
	Tenant string // scopes retrieval once the later tiers exist; empty in P0+1
}

// Source senses events and streams them until its context is cancelled.
type Source interface {
	Events(ctx context.Context) <-chan Event
}

// Store is drover's read/write view of a board. shepherd is one implementation,
// reached over its CLI — the loop must not be able to tell which Store it holds.
type Store interface {
	List(ctx context.Context, f Filter) ([]Item, error)
	Add(ctx context.Context, s Spec) (Item, error)
	SetStatus(ctx context.Context, id, status string) error
	Note(ctx context.Context, id, text string) error // attach a note to an item by id
	Archive(ctx context.Context, id string) error    // move an item off the live board (board emits a terminal "archived" event)
}

// Assembler turns an event into the Context a policy reasons over. The Phase 0+1
// implementation fills only the working-context tier (event + board slice).
type Assembler interface {
	Assemble(ctx context.Context, e Event) (Context, error)
}

// Policy is the think step: a pure decision from Context to Actions. Rules first;
// an LLM reasoner slots in behind the same interface later.
type Policy interface {
	Decide(ctx context.Context, c Context) []Action
}

// Executor applies actions. It never runs strings sourced from the board.
type Executor interface {
	Apply(ctx context.Context, a []Action) error
}

// Loop wires the four seams. It imports only the interfaces above.
type Loop struct {
	Assembler Assembler
	Policy    Policy
	Executor  Executor
}

// Run drives one event through the loop: assemble to decide to apply.
func (l Loop) Run(ctx context.Context, e Event) error {
	c, err := l.Assembler.Assemble(ctx, e)
	if err != nil {
		return err
	}
	actions := l.Policy.Decide(ctx, c)
	if len(actions) == 0 {
		return nil
	}
	return l.Executor.Apply(ctx, actions)
}
