# drover

> drives the flock the shepherd tends.

drover is the sense → assemble-context → act loop **around**
[shepherd](https://github.com/jwarykowski/shepherd). shepherd stays the dumb,
safe blackboard that owns the todo file; drover senses events, reads the
relevant slice of the board, decides, and — when allowed — acts. **It only ever
speaks shepherd's CLI, never the file.** Keep that line clean and everything
else stays swappable.

## status

All five roadmap phases are built. drover can prove the boundary, turn a signal
into a task, react live, reason with an LLM, and fire allowlisted actions —
each behind a clean seam.

## the boundary

drover never touches the todo markdown. It speaks shepherd's CLI contract:
stable item ids, `--json` on every mutating verb, structured errors, `watch`
(NDJSON), and `schema`. `store/shepherd.go` is the **only** file that knows
shepherd exists — the loop sees just interfaces, so shepherd is one swappable
`Store`.

## the seams

The loop is a handful of interfaces; everything else is an implementation behind
one.

```go
type Source    interface{ Events(ctx) <-chan Event }              // sense
type Assembler interface{ Assemble(ctx, Event) (Context, error) } // attend
type Store     interface{ List / Add / SetStatus }                // read + write
type Policy    interface{ Decide(ctx, Context) []Action }         // think
type Executor  interface{ Apply(ctx, []Action) error }            // act
```

`Loop.Run`: **event in → assemble the attention slice → decide actions →
validate → apply.** The loop imports only these interfaces — swap `ShepherdStore`
for `FakeStore` and it can't tell.

Actions are a closed vocabulary: `AddTask` (create an item) and `RunAction`
(fire an allowlisted named command). A policy proposes; the executor validates
before anything happens.

## layout

```
drover/
  cmd/drover/main.go     doctor | ingest | daemon | run
  loop/loop.go           the seams + Loop wiring (interfaces only)
  store/shepherd.go      CLI adapter — the only file that knows shepherd
  store/fake.go          in-memory Store for tests
  context/assembler.go   WorkingContext — the attention slice
  policy/rules.go        deterministic rules
  policy/llm.go          LLMReasoner + FallbackPolicy + ShadowPolicy
  policy/anthropic.go    the only file importing the Anthropic SDK
  source/git.go          GitSource — one-shot batch event
  source/watch.go        WatchSource — NDJSON stream over `shepherd watch`
  exec/store.go          StoreExecutor — task mutations, idempotent by link
  exec/runner.go         RunnerExecutor — allowlisted actions, never board-shell
  config/config.toml     drover's OWN config: the action allowlist
```

## the phases

Each phase is independently valuable and stoppable. Built in order; each layers
on the last.

### phase 0 — skeleton & boundary
Prove drover can round-trip a real board through the CLI: `drover doctor` lists
the board and adds a throwaway. The boundary — CLI only, never the file — is set
here and never crossed again.

### phase 1 — first vertical slice
One real signal → one correct task, batch. `drover ingest` builds one `Event`;
`WorkingContext` reads the `@ci` slice; `RulesPolicy` maps `ci.failed → AddTask`;
`StoreExecutor` adds it, **idempotent by link** (re-running the same event never
duplicates). Proves the whole thesis end to end.

### phase 2 — harden the boundary
Multi-writer safe and testable. Mutations address stable **ids**, mutating verbs
are parsed from `--json`, errors map to typed Go errors. `FakeStore` +
fake source make the loop and policies testable with no binary and no network;
`ShepherdStore` gets an integration test against a real binary.

### phase 3 — continuous: watch → daemon
React live, not only on invocation. `WatchSource` runs `shepherd watch`, parses
its NDJSON into `board.added`/`updated`/`removed` events, **reconnects** on
stream drop (survives a shepherd restart) with natural **backpressure** (an
unbuffered channel paces the reader). `drover daemon` streams those through the
loop until SIGINT/SIGTERM, draining cleanly.

### phase 4 — intelligence: LLM policy
Swap or augment rules with a reasoner behind the same `Policy` interface.
`LLMReasoner` asks Claude for proposals via one **constrained tool call** (a
fixed vocabulary, never free-form shell), consuming `shepherd schema` so the
model targets a valid item. Every proposal is **validated before it can act**;
on timeout or error it **falls back** to rules. `ShadowPolicy` runs rules and
the LLM on the same events and logs the diff — how the LLM earns trust before
it's allowed to act. `drover ingest --policy rules|llm|shadow`.

### phase 5 — safe actuation: allowlisted runner
Let drover *do* things — dispatch a fix to an agent, rerun CI, hit a webhook —
without becoming an RCE vector. Named actions live in `config/config.toml`
(drover's trusted config, **not** the board). `RunnerExecutor` fires a named
action only when it's in the allowlist, substitutes args as whole argv elements
(no shell; a newline/NUL is rejected), gates `confirm` actions, and writes a
provenance record of what ran and why. `drover run <name> --arg k=v`.

## how it acts on a signal

```
event (ci.failed)
  → policy emits RunAction{name:"fix-ci", args:{repo, task}}   # name from a fixed menu
  → RunnerExecutor: name allowlisted? → render argv (no shell) → confirm? → fire
  → cmd body from trusted config, e.g. `claude -p "{{task}}"` in the repo
  → the agent edits / opens a PR
  → that PR is the next signal → drover senses it → marks the @ci task done
```

The board only ever *names* an action; the command body lives in trusted config.
Board content can never cause execution. The board becomes the audit trail of
the whole loop.

## build

```sh
go build ./...
go test ./...                       # hermetic unit tests
go test -tags integration ./store/  # round-trip against a real shepherd binary
```

Requires the `shepherd` binary on `PATH` (≥ 0.15.0; `--policy llm` wants ≥ 0.17.0
for `schema`). LLM policies read `ANTHROPIC_API_KEY` or an `ant auth login`
profile from the environment.

## usage

```sh
# prove the boundary
drover doctor --project <board>

# one signal → one task (rules, llm, or shadow)
drover ingest --kind ci.failed --link "$CI_RUN_URL" --title "build failed" \
  --project <board> [--policy rules|llm|shadow]

# react to board changes live
drover daemon --project <board> [--verbose]

# fire an allowlisted named action
drover run fix-ci --arg repo=acme/api --arg task="fix the failing run" [--yes]
```

## design principles

- **Never exec strings from the board.** The synced, hand-editable file is
  untrusted input. The executor takes action *names* resolved against trusted
  config — never command bodies from item fields.
- **Address items by id, never index.** Indices shift as the board reorders;
  ids never do.
- **Policy is a pure function of context.** No I/O in `Decide` — table-testable,
  and an LLM reasoner drops in behind the same interface.
- **Validate before acting.** Every proposed action — an `AddTask` spec or a
  `RunAction` name — is checked against a fixed vocabulary before the executor
  sees it, so a nondeterministic model can't produce something unsafe.

## non-goals (v1)

- no perception — "sensing" means structured events (git/CI/webhooks/watch).
- no ML inside drover — adaptation lives in the model; drover keeps clean,
  queryable history via shepherd.
- no reimplementation of shepherd's storage — drover never owns the file.
