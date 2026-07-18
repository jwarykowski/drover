# drover

> drives the flock the shepherd tends.

drover is the sense → assemble-context → act loop **around**
[shepherd](https://github.com/jwarykowski/shepherd). shepherd stays the dumb,
safe blackboard that owns the todo file; drover senses events, reads the
relevant slice of the board, decides, and writes back — **only ever through
shepherd's CLI, never the file.** Keep that line clean and everything else stays
swappable.

## status

Phase 0 + 1 shipped: the boundary is proven and one real signal produces one
correct task, idempotently. Everything after is layering — see [roadmap](#roadmap).

## the boundary

drover never touches the todo markdown. It speaks shepherd's CLI contract
(0.15.0): stable item ids, `--json` on every mutating verb, structured errors.
`store/shepherd.go` is the **only** file that knows shepherd exists — the loop
itself sees just interfaces, so shepherd is one swappable `Store` implementation.

## the four seams

The loop is four interfaces; everything else is an implementation behind one.

```go
type Source    interface{ Events(ctx) <-chan Event }              // sense
type Store     interface{ List / Add / SetStatus }                // read + write
type Policy    interface{ Decide(ctx, Context) []Action }         // think
type Executor  interface{ Apply(ctx, []Action) error }            // act
```

An `Assembler` builds the `Context` a policy reasons over. Today that's Tier 1
only — the triggering event plus the relevant board slice (attention as a WHERE
clause). Project profile, retrieval and history are later tiers behind the same
interface.

`Loop.Run`: event in → assemble context → `Policy.Decide` → `Executor.Apply`.

## layout

```
drover/
  cmd/drover/main.go     doctor | ingest
  loop/loop.go           the seams + Loop wiring (interfaces only)
  store/shepherd.go      CLI adapter — the only file that knows shepherd
  context/assembler.go   WorkingContext — Tier 1 (event + board slice)
  policy/rules.go        deterministic rules; LLM slots in behind Policy later
  exec/store.go          StoreExecutor — task mutations, idempotent by link
  source/git.go          GitSource — one-shot batch event
```

## build

```sh
go build ./...
go test ./...
```

Requires the `shepherd` binary (≥ 0.15.0) on `PATH`.

## usage

`doctor` proves the round-trip — lists the board, adds a throwaway probe:

```sh
drover doctor --project <board>
```

`ingest` turns one signal into one loop run. Wire it into a CI step or a
`.git/hooks/post-commit`:

```sh
drover ingest --kind ci.failed \
  --link "$CI_RUN_URL" \
  --title "build failed" \
  --project <board>
```

A `ci.failed` event creates exactly one `@ci !h` task linking the run. Re-running
the same event is idempotent — dedup is by link until shepherd offers a real
idempotency key.

## design principles

- **Never exec strings from the board.** The synced, hand-editable file is
  untrusted input. The executor takes action *names* resolved against trusted
  config only — never command bodies from item fields. (The whole point of the
  later allowlisted runner.)
- **Address items by id, never index.** Indices shift as the board reorders;
  ids never do.
- **Policy is a pure function of context.** No I/O in `Decide` — table-testable,
  and an LLM reasoner drops in behind the same interface.

## roadmap

| phase | goal | status |
|---|---|---|
| 0 | skeleton + boundary proven via `doctor` | done |
| 1 | one signal → one correct task, one-shot | done |
| 2 | id-safe, multi-writer, fakes + tests | boundary done; test infra + concurrency proof left |
| 3 | `watch` → daemon, react live | ungated — `shepherd watch` (NDJSON) ships in 0.16.0; daemon left to build |
| 4 | LLM policy behind the same `Policy` seam | gated on `shepherd schema --json` |
| 5 | allowlisted named-action runner | later |

Each phase is independently valuable and stoppable.
