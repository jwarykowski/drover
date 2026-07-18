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

// Event is something that happened worth reacting to: a CI failure, a commit, a
// webhook. Sensing means structured events, not perception.
type Event struct {
	Kind    string
	Source  string
	Payload map[string]any
	At      time.Time
}

// Item mirrors a shepherd item as emitted by `shepherd list --json` (0.15.0).
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

// Context is the bundle handed to Policy.Decide. Phase 0+1 fills Event and Board
// only; Profile, Similar, History and a real Tenant are later context tiers and
// stay zero-valued for now.
type Context struct {
	Event  Event
	Board  []Item // the relevant slice, not the whole board
	Tenant string // scopes retrieval once the later tiers exist; empty in P0+1
}

// Source senses events. GitSource is the Phase 1 batch implementation.
type Source interface {
	Events(ctx context.Context) <-chan Event
}

// Store is drover's read/write view of a board. shepherd is one implementation,
// reached over its CLI — the loop must not be able to tell which Store it holds.
type Store interface {
	List(ctx context.Context, f Filter) ([]Item, error)
	Add(ctx context.Context, s Spec) (Item, error)
	SetStatus(ctx context.Context, id, status string) error
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
