// Package loop defines drover's seams and wires them into a one-shot loop.
//
// drover is the sense to assemble-context to act loop. A source ingests events
// over a common protocol, an action matches one, and whichever agentic tool the
// action names runs against it. Nothing here knows about any particular source,
// tool or board — every one of those is an implementation behind an interface
// below, or a row in drover.toml.
package loop

import (
	"context"
	"time"
)

// Event is something that happened worth reacting to: a merge, an issue, a CI
// failure, a todo flipped. Sensing means structured events, not perception.
//
// Data is deliberately an open map rather than a sealed set of payload types: a
// source is a separate process, so it cannot add a Go type here, and every
// closed vocabulary in this file was a reason drover had to be recompiled to
// learn a new source. Actions match on Data, prompts render it, and targets
// template from it, so a new source needs no code — only a drover.toml row.
type Event struct {
	ID     string            // stable per logical event — the dedup key
	Type   string            // hierarchical, e.g. "github.pull_request.merged"
	Source string            // instance identity, e.g. "acme-api"
	Data   map[string]string // whatever the source sent
	At     time.Time
}

// TaskUpdated is the event type the task store re-drives itself with, so a task
// released by a human dispatches on the next tick. It is the one event type
// drover emits rather than ingests.
const TaskUpdated = "task.updated"

// Task is one drover run: raised by a matched event, parked for release, fired
// at an agent, closed from that agent's verdict. It lives in drover's own store
// — no external board is ever written.
type Task struct {
	ID        string            `json:"id"`
	Index     int               `json:"index"` // monotonic, display order only — never an address
	Done      bool              `json:"done"`
	Priority  string            `json:"priority,omitempty"`
	Text      string            `json:"text,omitempty"`
	Created   string            `json:"created,omitempty"`
	Completed string            `json:"completed,omitempty"`
	Due       string            `json:"due,omitempty"`
	Link      string            `json:"link,omitempty"`
	Status    string            `json:"status,omitempty"`
	Action    string            `json:"action,omitempty"`  // config action id — a reference, never a command
	Subject   string            `json:"subject,omitempty"` // stable id of the thing the run concerns; the one-at-a-time key
	Note      string            `json:"note,omitempty"`
	Source    string            `json:"source,omitempty"` // the source that raised it
	Data      map[string]string `json:"data,omitempty"`   // the raising event's data, replayed into the prompt
}

// Filter narrows a store read to the relevant slice — the "attention" a policy
// reasons over. Closed tasks are out of attention unless asked for.
type Filter struct {
	IncludeDone bool
}

// Spec is a request to create a Task.
type Spec struct {
	Text     string
	Priority string // H, M or L
	Status   string // named status, e.g. "hold"; empty means default/open
	Action   string // config action id the agent resolves on release
	Subject  string // stable id of the thing this run concerns; dedup key
	Due      string
	Link     string
	Note     string
	Source   string
	Data     map[string]string
}

// Action is something a policy decides to do — a closed vocabulary an executor
// validates and applies. A policy proposes; nothing here carries a command body.
type Action interface{ isAction() }

// AddTask asks the executor to create a task from Spec.
type AddTask struct{ Spec Spec }

func (AddTask) isAction() {}

// SetStatus transitions an existing task by id. "done" is terminal; any other
// value sets a named status (e.g. "running").
type SetStatus struct {
	ID     string
	Status string
}

func (SetStatus) isAction() {}

// RunAgent fires an action from drover.toml. Only ActionID travels — the
// prompt, target, mode and the agent binary all resolve in trusted config, so
// event content can at most select an allowlisted action, never introduce one.
// The executor reconciles TaskID from the agent's structured verdict.
type RunAgent struct {
	ActionID string            // config action id resolved by the executor
	TaskID   string            // task to reconcile from the verdict
	Data     map[string]string // event context for the prompt and the target template
}

func (RunAgent) isAction() {}

// Context is the bundle handed to Policy.Decide: the event, plus the slice of
// tasks it might concern.
type Context struct {
	Event Event
	Tasks []Task
}

// Source ingests events and streams them until its context is cancelled. Both
// transports (a spawned process, an HTTP listener) are implementations of this,
// and so is the task store's own re-drive.
type Source interface {
	Events(ctx context.Context) <-chan Event
}

// Store is drover's read/write view of its tasks.
type Store interface {
	List(ctx context.Context, f Filter) ([]Task, error)
	Add(ctx context.Context, s Spec) (Task, error)
	SetStatus(ctx context.Context, id, status string) error
	Note(ctx context.Context, id, text string) error
	Archive(ctx context.Context, id string) error
}

// Assembler turns an event into the Context a policy reasons over.
type Assembler interface {
	Assemble(ctx context.Context, e Event) (Context, error)
}

// Policy is the think step: a pure decision from Context to Actions. No I/O in
// Decide, so it stays table-testable.
type Policy interface {
	Decide(ctx context.Context, c Context) []Action
}

// Executor applies actions. It never runs a string sourced from an event.
type Executor interface {
	Apply(ctx context.Context, a []Action) error
}

// Loop wires the seams. It imports only the interfaces above.
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
