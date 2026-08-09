# Changelog

All notable changes to this project are documented in this file.

## Unreleased

### Added

- Durable conversation threads. `AgentConfig.Store` optionally binds an agent
  to a `Store` so that conversation history survives across runs. When set
  and `RunInput.ThreadID` is non-empty, the agent loads prior messages from
  the thread, prepends them to the model request, and appends the new
  transcript on success. Failed runs leave no messages, so the thread's
  message sequence stays valid. When `Store` is nil or `ThreadID` is empty,
  agent behavior is unchanged. The thread is auto-created on the first
  successful run if it does not already exist.
- Thread namespace and ownership fields. `ThreadRecord` carries optional
  `Namespace` and `OwnerID` fields for multi-tenant and embedding
  applications. Both default to empty strings and do not affect repository
  behavior when unset. SQLite and PostgreSQL migrations add the columns
  idempotently; the in-memory adapter handles them via struct copy.
- Initial public contracts for agent-runtime primitives.
- Replaceable JSON Schema Draft 2020-12 validation for tool inputs and outputs.
- Deterministic model fixtures, assertions, streams, and provider contract tests
  for repository tests and examples.
- Provider-neutral text, tool-call, and structured-output model protocol with
  opaque vendor extensions and typed error mapping.
- Schema-backed tool registration and execution with request metadata,
  cancellation, and distinct normalized failure states.
- OpenAI-compatible text-generation model adapter with authenticated HTTP
  requests, timeouts, opaque request extensions, typed error translation, and
  recorded-HTTP contract tests.
- Bounded tool-using agent loop that combines instructions, caller messages, and
  tool schemas into model requests; appends tool calls and tool results to the
  conversation in canonical order; enforces configurable maximum steps and
  deadlines; and returns typed failures for unknown tools, invalid arguments,
  tool failures, provider failures, step-limit exhaustion, and cancellation.
- Schema-constrained structured output for the agent loop. `AgentConfig.OutputSchema`
  and `RunInput.OutputSchema` (per-run override) request a final JSON value that
  conforms to a caller-supplied schema; the agent forwards the schema to the
  model adapter on every step and validates the final assistant payload locally
  via `AgentConfig.SchemaCompiler`. Missing or schema-invalid final output
  returns `AgentErrorInvalidStructuredOutput`. `RunResult.StructuredOutput` and
  `RunResult.DecodeStructuredOutput` expose the validated typed result. Tool use
  and a structured final response compose in a single run.
- Deterministic run record for agent lifecycle events: run start/finish, model
  request start/finish, tool-call requested, tool started/finished, and
  terminal events (succeeded, failed, cancelled). Events carry stable run/step
  IDs, monotonic sequence numbers, timestamps, durations, model usage, and error
  summaries. A RunListener interface and RunRecorder collector capture
  events without requiring an observability backend. Injectable Clock and
  IDSource make event streams reproducible across runs with the same fixture.
- Typed linear workflow execution. `LinearWorkflow` composes ordered, named
  steps (`Step`) whose handlers implement `StepHandler` (or `StepHandlerFunc`
  for ordinary Go functions). Each step declares optional JSON Schemas for its
  input and output; the executor compiles them once and validates every
  handoff: a step's input is validated against its `InputSchema` before the
  handler runs, and the handler's output is validated against its
  `OutputSchema` before passing it to the next step. A failed step stops the
  workflow and the returned `*WorkflowError` identifies the failing step by
  1-indexed position and declared ID. Workflow and step lifecycle records
  (`step_started`, `step_finished`) flow through the existing `RunListener` /
  `RunRecorder` event model, ordered and correlated to one run ID. `WorkflowRunInput`
  and `WorkflowRunResult` carry raw JSON input and output; `DecodeOutput`
  unmarshals the final step result into a caller-supplied value.
- Agent and registered-tool workflow step adapters. `NewAgentStep` turns a
  workflow value into an agent user message and returns structured output or
  final assistant text as JSON; it forwards workflow context, thread ID, and
  metadata. `NewToolStep` invokes a schema-checked `RegisteredTool` with the
  workflow value as arguments and returns validated output. Nested agent run
  events now carry parent workflow run and step correlation fields.
- File-backed SQLite storage. `NewSQLiteStore` opens a pure-Go SQLite database
  (via `modernc.org/sqlite`, no CGO) at a caller-supplied DSN and implements
  the same `Store` and repository contracts as the in-memory adapter:
  threads, messages, workflow runs, and workflow snapshots survive process
  restarts. `Migrate` installs the schema atomically and idempotently; a
  failed migration rolls back and leaves the database in its prior state with
  an actionable error. Concurrent writers serialize on SQLite's transactional
  locking, with lock contention over the busy timeout surfaced as
  `ErrConflict` for callers to retry. A shared storage contract suite in the
  test kit runs against both adapters to keep the memory and durable
  implementations behaviorally aligned.
- PostgreSQL storage. `NewPostgresStore` opens a pure-Go PostgreSQL
  connection pool (via `github.com/jackc/pgx/v5`, no CGO) for production
  deployments where multiple processes share threads and workflow state.
  It implements the same `Store` and repository contracts as the in-memory
  and SQLite adapters: threads, messages, workflow runs, and workflow
  snapshots survive connection pool close/reopen. `Migrate` installs the
  schema atomically and idempotently using a `schema_migrations` version
  table; a failed migration rolls back and leaves the database unchanged
  with an actionable error. Transactions use `READ COMMITTED` isolation;
  serialization failures (SQLSTATE 40001) and lock timeouts (55P03) surface
  as `ErrConflict` for callers to retry, and foreign-key violations (23503)
  map to `ErrNotFound`. Required indexes (`idx_messages_thread_seq`,
  `idx_workflow_snapshots_run_seq`) and migration versioning are documented
  in the README. The adapter passes the shared storage contract suite when
  `LEBRO_POSTGRES_TEST_DSN` is set to a disposable database.
- Durable workflow run snapshots. `LinearWorkflowConfig.Store` optionally
  binds a linear workflow to a `Store` so run state survives at safe step
  boundaries. When set, the executor persists the run record as `Running`
  before the first step, and after every successful step writes a
  `WorkflowSnapshotRecord` plus an updated `WorkflowRunRecord` inside one
  `Store.Transaction` so the boundary is atomic. A persistence failure fails
  the run with `WorkflowErrorStepFailed` wrapping the storage error, so a
  process restart never observes a partially persisted step. `WorkflowRunRecord`
  now carries `CurrentStep`, `CurrentStepID`, `StepOutputs` (ordered completed
  outputs), `Failure` (`*WorkflowFailureData` with kind, step, step ID, and
  message), and `WorkflowVersion` (the opaque definition/version reference from
  `WorkflowDefinition.Version`). `WorkflowSnapshotRecord` carries
  `SchemaVersion` (the executor writes `1`; readers tolerate `0` as legacy).
  A new `ListWorkflowRuns(context.Context, WorkflowRunFilter, PageRequest)`
  method on `WorkflowRunRepository` lists runs for inspection, optionally
  filtered by `WorkflowID` and `Status`. SQLite and PostgreSQL add append-only
  migrations for the new columns; the in-memory adapter handles them via
  struct copy. When `Store` is nil, workflow behavior is unchanged.
- Token and event streaming with cancellation. `StreamingModel` extends the
  provider-neutral `Model` interface with `Stream`, which returns a
  `StreamReader` over ordered `StreamDelta` values (text, tool calls,
  structured output, terminal finish reason, usage). `AsStreamingModel`
  adapts a `Model` into a `StreamingModel` when the concrete value implements
  `Stream`, returning nil otherwise so callers fall back to `Generate`.
  `Agent.RunStream` runs the bounded agent loop against a `StreamingModel`
  and returns a `StreamRun` handle: the caller drains `Deltas` (a
  `<-chan StreamDelta`) in real time, then calls `Wait` (or `Drain`) for the
  terminal `RunResult` and error. The caller must `defer` `StreamRun.Cancel`
  so goroutine and channel resources are released even when the stream is
  abandoned before completion; closing the consumer does not leak goroutines.
  Cancellation propagates through the provider stream, tool execution, and the
  loop itself: cancelling the context (or calling `StreamRun.Cancel`) stops
  active work, closes the delta channel, and returns a result with status
  `RunStatusCancelled` wrapping `ErrAgentCancelled`. A `RunEventDelta`
  (`"model_delta"`) run event is emitted for each delta and flows through the
  existing `RunListener` / `RunRecorder` infrastructure. Streaming and
  non-streaming runs produce equivalent final records: when the configured
  model does not implement `StreamingModel`, `RunStream` falls back to
  `Generate` and emits a single delta per step. The OpenAI-compatible adapter
  implements `StreamingModel` over Server-Sent Events, mapping text deltas,
  usage, finish reasons, and error events into the neutral protocol.
