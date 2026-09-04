# Durable run records

Store-bound agent runs persist three kinds of observability record next to the
canonical transcript: model attempts, tool executions, and run events. They
exist so applications can answer "which provider produced this outcome, what
did it cost, what did tools do, and what did plugins decide" without parsing
message content or attaching an external tracing backend first.

The transcript stays canonical. A single model request can produce one
assistant tool-call message plus several tool-result messages, so token usage
and finish reasons live on attempt records — never on tool-result messages.

## Record catalog

| Record | One row per | Key fields |
|---|---|---|
| `ModelAttemptRecord` | actual provider invocation | provider/model identity, routed target, status, usage, finish reason, timestamps, error classification, produced message IDs |
| `ToolExecutionRecord` | tool call lifecycle | tool/tool-call IDs, state, timing, redacted error data |
| `RunEventRecord` | non-delta `RunEvent` | sequence per run, type, correlation IDs, safe payload, plugin attribution |

Attempt statuses are `success`, `fallback`, `failed`, and `cancelled`. A
routed/fallback run persists every attempt; `fallback` marks a provider that
failed and handed off to the chain, per the routing contract. Direct (non-router)
model calls synthesize one attempt per `Generate` with provider ID `direct`.
Adapters can implement `ModelIdentityProvider` to persist their actual provider
ID instead.

## Correlation

All records carry `run_id`. Attempts, tool executions, and events also carry
the 1-indexed step and step ID; attempts expose `produced_message_ids`
linking the winning attempt to the terminal assistant message it created;
tool executions repeat the provider's `tool_call_id`; events keep their
in-memory sequence numbers, so an in-memory `RunRecorder` trace and the
durable stream interleave consistently for a single run.

Run IDs must be unique among concurrent agents sharing one store. Independently
constructed agents default to per-agent sequential sources that both start at
`agent-run-0001` (this mirrors nested-run correlation elsewhere); when such
agents share a store, inject a shared `IDSource` into every agent, or accept
that observability appends are idempotent — a duplicate `(run_id, id)` is
skipped, never fatal, and the first writer wins.

## Redaction and privacy defaults

Persisted by default: identity (provider, model), outcomes, token counts,
timings, normalized error kinds plus safe error messages, event lifecycle
types, and developer metadata that passes validation.

Never persisted by default:

- stream delta text, reasoning text, structured output payloads, and opaque
  provider replay details (deltas are skipped entirely; replay details stay in
  the canonical message JSON where they already live);
- tool arguments and tool results;
- raw prompts or arbitrary provider request/response payloads;
- secrets — lebro has no place to put them in these schemas by design.

Errors are stored as a typed kind plus the literal marker `redacted`.
Provider and tool messages can echo user data, arguments, or credentials, so
the runtime never stores them by default. Correlate the typed kind with your
application's protected logs when more detail is needed.

Retention is yours: these tables grow with traffic. Delete by `run_id`,
`thread_id`, or age with your own jobs; lebro does not expire records.

## Metadata (annotations)

Namespaced application metadata attaches at five scopes:

```go
lebro.RunInput{Annotations: lebro.Metadata{
    "app.customer_id": json.RawMessage(`"acme"`),
}}
```

`Metadata.Validate()` enforces:

- keys shaped `namespace.key` (`[A-Za-z0-9_-]` segments); the `lebro`
  namespace is reserved;
- at most `MaxMetadataEntries` (32) entries;
- values must be valid JSON, nested at most `MaxMetadataDepth` (8);
- combined encoded size at most `MaxMetadataBytes` (16 KiB).

Nil and empty metadata are always valid. Run annotations flow onto every
record the runtime writes for that run (messages via `MessageRecord.Annotations`,
attempts, tool executions, events); record-level metadata set independently is
overlaid on top, with the record's own keys winning. Layer stricter allowlists
(filtering before you attach) in your application code.

## Querying

```go
page, err := store.RunEvents().ListRunEvents(ctx, lebro.RunEventFilter{
    ThreadID: threadID,
    From:     since,
}, lebro.PageRequest{Limit: 50})
```

Filters: `RunEventFilter` (run/thread/type/provider/tool/time range),
`ModelAttemptFilter` (run/thread/provider/status), `ToolExecutionFilter`
(run/thread/tool/state). All filters also accept `Namespace` and `OwnerID` to
preserve tenant isolation. Listings order by run then insertion/sequence and use
the same cursor pagination as every repository. Records do not require a
thread row to exist — failed runs persist diagnostics before any transcript.

A complete walkthrough lives in `examples/run-timeline`.

## Store support and custom adapters

Observability persistence is opt-in and additive. A `Store` participates when
both the store itself **and** the repositories it hands to `Transaction`
implement:

```go
type ObservabilityRepositories interface {
    RunEvents() RunEventRepository
    ModelAttempts() ModelAttemptRepository
    ToolExecutions() ToolExecutionRepository
}
```

Built-in Memory, SQLite, and Postgres stores implement it everywhere, so
transcript messages, attempts, tools, and events commit in one transaction on
success. Stores that do not implement the interface behave exactly as before:
runs succeed, transcripts persist, observability records are simply not
written. Custom adapters back the interface with whatever developer-owned
schema they like — lebro-owned table names are not required. Implementations
must skip duplicate `(run_id, id)` appends instead of failing (see
`internal/testkit.StorageContractSuite`, which all built-in adapters pass).

Write semantics by outcome:

| Outcome | What persists |
|---|---|
| success | messages + attempts + tool executions + events in one transaction |
| failure | attempts + tool executions + events (no messages, no thread claim) |
| cancellation | same as failure; flushed with a detached context so dead run contexts still persist |
| panic | best-effort diagnostics via deferred flush |

Diagnostic flushes are best-effort: a failure writing them never masks the
run's own error.

## Cost model limitations

`CostMicros`/`Currency` and `ProviderRequestID` exist because some providers
report them; lebro never computes cost. Token counts are provider-reported
and may be absent (zero) for providers or streams that omit usage.

## Relationship to `obsv`

The `obsv` package remains the export pipeline for spans/logs/feedback and can
target a different database. These records are the durable, queryable system
of record inside the primary `Store`; neither replaces OpenTelemetry.
