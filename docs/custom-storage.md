# Custom storage adapters

Lebro's runtime persistence is pluggable. The `Store` interface (Memory,
SQLite, Postgres) remains the full-featured, migration-managed path; the
capability-based `RuntimeStore` contract is the path for attaching Lebro to a
store the application already owns — an existing database, an API, an event
store, a document store — without adopting Lebro's predefined tables or
running Lebro migrations.

## The contract

```go
type RuntimeStore interface {
    Capabilities() StoreCapabilities
}
```

An adapter implements `RuntimeStore` plus only the capability interfaces it
supports, and advertises exactly that set:

```go
type StoreCapabilities struct {
    Transcript    bool // thread records + ordered message transcript (read and write)
    WorkingMemory bool // scoped working-memory fact CRUD, including recall reads
    WorkflowState bool // workflow runs + resumable snapshots
    Schedules     bool // schedule records + execution history
    Observability bool // durable run events, model attempts, tool executions
    Transactions  bool // atomic coupled writes via TransactionalStore
}
```

Each capability is provided by a small interface over Lebro's neutral,
versioned JSON record types — never SQL tables, database handles, or provider
types:

| Capability | Interface | Repositories |
|---|---|---|
| `Transcript` | `TranscriptStore` | `Threads()`, `Messages()` |
| `WorkingMemory` | `WorkingMemoryStore` | `WorkingMemory()` |
| `WorkflowState` | `WorkflowStateStore` | `WorkflowRuns()`, `WorkflowSnapshots()` |
| `Schedules` | `ScheduleStore` | `Schedules()`, `ScheduleExecutions()` |
| `Observability` | `ObservabilityStore` | `RunEvents()`, `ModelAttempts()`, `ToolExecutions()` |
| `Transactions` | `TransactionalStore` | `InTransaction(...)` |

Reads are explicit per capability: an event sink that can only write cannot
advertise `Observability`, because the capability includes the reads Lebro
performs (listing events, attempts, and tool executions). The same rule holds
for `Transcript`: agent runs load prior messages before the first model call,
so a write-only transcript is not a transcript.

`RuntimeStoreContractVersion` identifies the contract version; the record
types carry their own envelope versions where applicable (for example
`WorkflowSnapshotRecord.SchemaVersion`).

## Advertisement must match reality

Lebro validates the contract when the adapter is attached. `Capabilities()`
must advertise exactly the interfaces the adapter implements — advertising a
capability without implementing it, or implementing one without advertising
it, fails with a `*StoreCapabilityError` (match with `errors.Is(err,
lebro.ErrCapabilityMissing)`). A store that advertises nothing is rejected.

This is also why partial adapters are safe: capability repository accessors on
a misconfigured path fail with the same typed error instead of panicking, and
Lebro never silently substitutes its own storage. When a configured feature
needs a capability the adapter does not support, you get a typed, actionable
error before any run starts:

```
lebro: storage adapter does not support capability "transcript" required by thread persistence: the attached storage adapter does not advertise it
```

Feature requirements checked up front:

- `AgentConfig.Memory` (working-memory recall and extraction) requires
  `WorkingMemory` at construction time.
- A run with `RunInput.ThreadID` requires `Transcript` before the first model
  call.
- `Observability` is opt-in, matching the `ObservabilityRepositories`
  semantics of `Store`: an adapter without it simply leaves run events,
  attempts, and tool executions unpersisted.

## Transactions

`TransactionalStore` is how coupled writes commit atomically — for example an
agent run's transcript, thread record, and observability records in one
commit:

```go
type TransactionalStore interface {
    InTransaction(context.Context, func(context.Context, RuntimeStore) error) error
}
```

Writes through the `RuntimeStore` handed to the callback commit atomically
when the callback returns nil and are discarded when it returns an error.

Adapters that cannot provide a transaction omit the interface and the
`Transactions` capability. Lebro then runs coupled writes **sequentially, in
the order the transaction would have used**:

- There is no rollback. A failure mid-sequence leaves already-written records
  in place and returns the error.
- There is no optimistic concurrency, so `ErrConflict` never occurs; adapters
  that want retryable conflicts should implement `TransactionalStore` and map
  their native contention to it.

Choose this behavior deliberately: an append-only event store that cannot
read prior messages, or a document store without multi-document transactions,
is a legitimate adapter — the contract says what it costs.

## What adapters own

- The backing schema, storage layout, and any migrations for it. Lebro runs
  none.
- Mapping Lebro's record types onto that layout. The records are JSON
  contracts: `ThreadRecord`, `MessageRecord`, `WorkflowRunRecord`,
  `WorkflowSnapshotRecord`, `ScheduleRecord`, `ScheduleExecutionRecord`,
  `WorkingMemoryFact`, `RunEventRecord`, `ModelAttemptRecord`,
  `ToolExecutionRecord`.
- The repository semantics Lebro relies on: context cancellation, cursor
  pagination (`PageRequest`/`Page`, `ErrInvalidPage`), `ErrNotFound` for
  missing reads, `ErrConflict` for stale versions, defensive copies (returned
  records must not alias stored state), and idempotent observability appends
  (a repeated `(run_id, id)` pair is skipped, never duplicated).

The public `storetest` package exposes `RuntimeStoreContractSuite` for
external adapters. It covers capability advertisement, round-trips,
pagination, cancellation, tenant isolation, idempotent observability writes,
and transaction commit/rollback semantics:

```go
storetest.RuntimeStoreContractSuite(t, func(t *testing.T) lebro.RuntimeStore {
    return newControlPlaneStore(t) // run your application's migrations here
})
```

## Scope and authorization

`RuntimeScope{Namespace, OwnerID}` is the reusable tenant boundary used by
workflow runs, schedules, schedule executions, working memory, and durable
observability records. An organization commonly maps to `Namespace` and a user
or service principal maps to `OwnerID`. Empty fields preserve the existing
single-tenant behavior.

When using `PolicyStore`, have authenticated middleware attach the verified
scope with `lebro.WithRuntimeScope`. The guard rejects writes whose claimed
record scope differs from that value. Repository list filters accept the same
namespace/owner fields; do not use a client-supplied record namespace as an
authorization decision.

## PostgreSQL control-plane adapter shape

See [`examples/custom-postgres-runtime-store`](../examples/custom-postgres-runtime-store).
The application runs its own schema-qualified migrations and maps its own
organization and user IDs to `RuntimeScope`; the adapter exposes only the
Lebro capabilities it needs. A transaction callback receives transaction-
scoped repositories, while an adapter without transactions keeps the documented
sequential-write fallback.

Lebro intentionally does **not** own organizations, users, credentials, agent
definitions, workflow versions, uploaded files, deployments, queues, or
billing. Those are application control-plane concerns.

## End-to-end example

`examples/custom-store` attaches an in-process key/value blob store — a
stand-in for a developer-owned database with its own key layout — to an agent,
persists a multi-turn thread, reloads it for the second run, and prints the
typed capability error for a store that cannot support thread persistence:

```sh
go run ./examples/custom-store
```

The adapter's essential shape:

```go
type ownStore struct{ /* your storage */ }

func (s *ownStore) Capabilities() lebro.StoreCapabilities {
    return lebro.StoreCapabilities{Transcript: true}
}
func (s *ownStore) Threads() lebro.ThreadRepository   { return s }
func (s *ownStore) Messages() lebro.MessageRepository { return s }

agent, err := lebro.NewAgent(lebro.AgentConfig{
    Definition:   lebro.AgentDefinition{ID: "support", Model: "gpt-4o-mini"},
    Model:        openai.New(openai.Config{APIKey: key, Model: "gpt-4o-mini"}),
    RuntimeStore: store,
})
```

## Migrating from the broad `Store` interface

Existing `Store` implementations keep compiling and keep working; nothing
about `Store`, the built-in stores, transactions, or policy enforcement
changes. To move an existing custom `Store` to the new contract:

1. Split its repository accessors into the capability interfaces above —
   typically they already exist as methods.
2. Replace `Transaction(ctx, func(ctx, Repositories) error)` with
   `InTransaction(ctx, func(ctx, RuntimeStore) error)` if the backing store
   supports transactions; delete it if it does not.
3. Delete `Migrate` — the adapter owns its schema now.
4. Return the advertised `StoreCapabilities` set.

Built-in stores implement both contracts: `MemoryStore`, `SQLiteStore`, and
`PostgresStore` satisfy `RuntimeStore` with every capability advertised, so
they can be attached either way.
