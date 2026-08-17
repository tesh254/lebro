# lebro

`lebro` is a Go library for composing AI agents, schema-backed tools,
workflows, and their durable runtime state.

The first release establishes stable public contracts and safe local tool
execution. Provider adapters, the agent loop, and workflow execution arrive in
the following incremental releases. This keeps each layer independently
testable and avoids locking users into a model provider or storage backend.

## Package layout

`github.com/tesh254/lebro` is the only import most applications need. It is a
stable façade for every public contract and constructor, so existing code keeps
using `lebro.Message`, `lebro.NewToolRegistry`, and `lebro.NewMemoryStore`.

```
lebro/                   public API contracts and domain constructors
internal/runtime/        model, tools, schema, workflow, storage, vector, and RAG runtime
jsonschema/              optional JSON Schema compiler implementation
httpapi/                 optional HTTP server and generated OpenAPI contract
channels/                optional messaging channel adapters (inbound webhook, streamed reply)
voice/                   optional speech-to-text and text-to-speech integration points
studio/                  optional local Studio-style developer UI
evals/                   optional datasets, scorers, and experiment runs
internal/testkit/        deterministic provider fixtures and contract suites for tests
examples/                runnable feature-focused examples
docs/                    installation and release guides
```

Root API files are grouped by domain (`agent.go`, `network.go`, `storage.go`,
`workflow.go`, `tools.go`, and related files); `api_types.go` contains shared
aliases and constants. Keep runtime implementation out of module root. Add
optional integrations as their own packages, never as dependencies of root API.

## Requirements

- Go 1.26.5 or newer

The module pins Go 1.26.5 with Go's `toolchain` directive. With the default
`GOTOOLCHAIN=auto`, Go downloads that toolchain automatically when needed.

## Install

After the first release tag is published:

```sh
go get github.com/tesh254/lebro@v0.1.0
```

For the latest development version before the first tag:

```sh
go get github.com/tesh254/lebro@latest
```

See [the installation guide](docs/installation.md) for toolchain setup and
upgrade instructions.

## Current API foundation

The module already exposes provider-neutral contracts for messages, model
adapters, tools, workflows, and storage. This example validates a canonical
message:

```go
package main

import (
	"fmt"

	"github.com/tesh254/lebro"
)

func main() {
	message := lebro.Message{Role: lebro.RoleUser, Content: "Hello"}
	if err := message.Validate(); err != nil {
		panic(err)
	}

	fmt.Println("message is valid")
}
```

## Provider-neutral model protocol

Model adapters implement `lebro.Model` and exchange only neutral request and
response values. The protocol supports assistant text, multiple tool calls,
schema-constrained JSON, normalized usage and finish reasons, typed provider
errors, and opaque JSON extensions for vendor metadata:

```go
request := lebro.ModelRequest{
	Model:    "example/model",
	Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "Return the weather as JSON"}},
	OutputSchema: &lebro.ModelOutputSchema{
		Name:   "weather",
		Schema: json.RawMessage(`{"type":"object"}`),
		Strict: true,
	},
}

if err := request.Validate(); err != nil {
	panic(err)
}
```

Tool calls and structured JSON are recorded on the assistant `Message`, so the
same canonical transcript can be persisted and replayed on the next turn.

Provider failures can be inspected with `errors.As` as `*lebro.ModelError` or
with `errors.Is` against sentinels such as `lebro.ErrModelRateLimited`.

## Model-provider adapters

The optional `github.com/tesh254/lebro/openai` package implements `lebro.Model`
against any OpenAI-compatible chat-completions endpoint. It is text-only: tool
definitions and structured output are rejected here and handled by richer
adapters built on the same protocol. OpenAI-specific wire types stay inside the
package, and opaque `ModelRequest.Extension` fields are merged into the request
body so callers can pass vendor knobs (`temperature`, `max_tokens`, `seed`,
...) without coupling the neutral protocol to a vendor.

```go
import (
    "github.com/tesh254/lebro"
    "github.com/tesh254/lebro/openai"
)

model, err := openai.New(openai.Config{
    APIKey: os.Getenv("OPENAI_API_KEY"),
    Model:  "gpt-4o",
})
if err != nil {
    panic(err)
}

response, err := model.Generate(context.Background(), lebro.ModelRequest{
    Model:    "gpt-4o",
    Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "Hello"}},
})
if err != nil {
    var apiErr *lebro.ModelError
    if errors.As(err, &apiErr) {
        // apiErr.Kind is normalized (authentication, rate_limited, timeout, ...).
    }
    return err
}
```

The optional `github.com/tesh254/lebro/anthropic` and
`github.com/tesh254/lebro/gemini` packages add native Claude and Gemini
adapters. Both support text, client tool calls, streaming, and structured
output without coupling those provider SDKs to the root package:

```go
model, err := anthropic.New(anthropic.Config{
    APIKey: os.Getenv("ANTHROPIC_API_KEY"),
    Model:  "claude-sonnet-4-5",
})
// Or: gemini.New(gemini.Config{APIKey: os.Getenv("GEMINI_API_KEY"), Model: "gemini-2.5-flash"})
```

Anthropic JSON output requires a model with structured-output support. Gemini
accepts only its documented JSON Schema subset. Provider schema rejection is an
invalid model request; Lebro still validates returned structured JSON locally.

Network, authentication, permission, not-found, rate-limit, timeout, and
malformed-response failures each map to a distinct `lebro.ModelErrorKind`;
context cancellation is returned as `context.Canceled` so `errors.Is` works
against it directly.

## Schema-backed tool execution

Register application tools with a `ToolRegistry` to compile their input and
output schemas once. Every invocation validates arguments before calling the
handler and validates the result before returning it. Request metadata is
available through `ToolMetadataFromContext`, while validation errors, handler
errors, panics, cancellation, and missing tools have distinct result states.

```go
registry, err := lebro.NewToolRegistry(lebrojsonschema.NewCompiler())
if err != nil {
	panic(err)
}
if err := registry.Register(weatherTool{}); err != nil {
	panic(err)
}

result := registry.Execute(ctx, "weather.lookup", lebro.ToolExecutionRequest{
	Arguments: json.RawMessage(`{"city":"Nairobi"}`),
	Metadata:  map[string]string{"request_id": "req-42"},
})
```

## Bounded tool-using agent loop

`lebro.NewAgent` builds an agent that repeatedly asks a model, executes
requested tools through a `ToolRegistry`, and feeds results back until the
model produces a terminal response or a configured bound is reached.

```go
agent, err := lebro.NewAgent(lebro.AgentConfig{
    Definition: lebro.AgentDefinition{
        ID:           "weather-agent",
        Instructions: "Use weather.lookup to answer weather questions.",
        Model:        "gpt-4o",
        Tools:        []lebro.ToolID{"weather.lookup"},
    },
    Model:    model,
    Tools:    registry,
    MaxSteps: 10,
    Deadline: 30 * time.Second,
})
if err != nil {
    panic(err)
}

result, err := agent.Run(context.Background(), lebro.RunInput{
    Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "Weather in Nairobi?"}},
})
```

The loop prepends `Instructions` as a system message, exposes only the tool
schemas listed in `AgentDefinition.Tools`, and appends every assistant response
and tool result to `RunResult.Messages` in canonical order. `MaxSteps` defaults
to `lebro.DefaultAgentMaxSteps` when zero. Failures are returned as
`*lebro.AgentError` with a normalized `Kind`:

| Kind | Cause |
|------|-------|
| `AgentErrorUnknownTool` | Model requested or definition references an unregistered tool |
| `AgentErrorInvalidToolArguments` | Tool input schema rejected model arguments |
| `AgentErrorInvalidToolOutput` | Tool output schema rejected a handler result |
| `AgentErrorToolFailure` | Handler error or recovered panic |
| `AgentErrorProviderFailure` | Model adapter failure or invalid model response |
| `AgentErrorStepLimitExhausted` | Loop consumed every permitted step |
| `AgentErrorCancelled` | Run context or deadline cancelled |
| `AgentErrorInvalidStructuredOutput` | Terminal response omitted or failed schema validation of requested structured output |
| `AgentErrorUnauthorized` | Configured `Policy` denied the run or a tool call (see [Authentication and authorization hooks](#authentication-and-authorization-hooks)) |

`errors.Is` works against the `lebro.ErrAgent*` sentinels and against
`context.Canceled` / `context.DeadlineExceeded`. The wrapped error preserves the
original `*lebro.ModelError` for provider failures so `errors.As` keeps working.

Run the bounded agent-loop example (no network or API key required):

```sh
go run ./examples/agent-loop
```

## Durable conversation threads

An agent can optionally bind to a `Store` so that conversation history
survives across runs. Set `AgentConfig.Store` to a `MemoryStore`,
`SQLiteStore`, or `PostgresStore` and pass a `ThreadID` in `RunInput`. On
each run the agent loads prior messages from the thread, prepends them to
the model request, and appends the new transcript on success. Failed runs
leave no messages, so the thread's message sequence stays valid. When
`Store` is nil or `ThreadID` is empty, agent behavior is unchanged.

```go
store, _ := lebro.NewSQLiteStore("state.db")
defer store.Close()
store.Migrate(ctx)

agent, _ := lebro.NewAgent(lebro.AgentConfig{
    Definition: lebro.AgentDefinition{ID: "chat-agent", Model: "gpt-4o"},
    Model:      model,
    Store:      store,
})

result, _ := agent.Run(ctx, lebro.RunInput{
    ThreadID: "thread-1",
    Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "Hello"}},
})

// A second run with the same ThreadID receives the prior messages.
result2, _ := agent.Run(ctx, lebro.RunInput{
    ThreadID: "thread-1",
    Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "Follow up?"}},
})
```

`ThreadRecord` carries optional `Namespace` and `OwnerID` fields for
multi-tenant and embedding applications. Both default to empty strings and
do not affect repository behavior when unset.

```go
store.Threads().CreateThread(ctx, lebro.ThreadRecord{
    ID:        "thread-1",
    Namespace: "tenant-acme",
    OwnerID:   "user-42",
})
```

## Schema-constrained structured output

An agent can request a final JSON value that conforms to a caller-supplied
schema. Set `AgentConfig.OutputSchema` (per agent) or `RunInput.OutputSchema`
(per run, overrides the agent default) alongside `AgentConfig.SchemaCompiler`.
The agent forwards the schema to the model adapter on every step and validates
the final assistant payload locally before returning a successful result.

```go
agent, err := lebro.NewAgent(lebro.AgentConfig{
    Definition: lebro.AgentDefinition{ID: "weather-agent", Model: "gpt-4o"},
    Model:       model,
    SchemaCompiler: lebrojsonschema.NewCompiler(),
    OutputSchema: &lebro.ModelOutputSchema{
        Name:   "weather_result",
        Schema: json.RawMessage(`{"type":"object","required":["temperature_c"]}`),
    },
})
if err != nil {
    panic(err)
}

result, err := agent.Run(context.Background(), lebro.RunInput{
    Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "Weather in Nairobi?"}},
})
if err != nil {
    panic(err)
}

var report struct {
    TemperatureC float64 `json:"temperature_c"`
}
if err := result.DecodeStructuredOutput(&report); err != nil {
    panic(err)
}
```

A run that ends without structured output, or whose final payload fails schema
validation, returns `*lebro.AgentError` with `Kind` set to
`AgentErrorInvalidStructuredOutput` (sentinel `lebro.ErrAgentInvalidStructuredOutput`).
Tool use and a structured final response work in the same run: validation runs
only on the terminal non-tool response. `RunResult.StructuredOutput()` returns
the validated raw JSON; `RunResult.DecodeStructuredOutput` unmarshals it into a
caller-provided value.

## Deterministic run recording

Every agent run can emit an ordered record of lifecycle events: run start,
model request start/finish, tool-call requested, tool started/finished, and
terminal events (succeeded, failed, cancelled). Events carry stable run/step
IDs, monotonic sequence numbers, timestamps, durations, model usage, and error
summaries.

Recording is opt-in via a `RunListener`. When no listener is configured, agent
behavior is unchanged. A `RunRecorder` collects events into a slice for
programmatic inspection without an observability backend:

```go
recorder := lebro.NewRunRecorder()
agent, err := lebro.NewAgent(lebro.AgentConfig{
    Definition: lebro.AgentDefinition{ID: "agent", Model: "gpt-4o"},
    Model:      model,
    Listener:   recorder,
})
result, err := agent.Run(context.Background(), lebro.RunInput{
    Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "Hello"}},
})

events := recorder.Events()
for _, event := range events {
    fmt.Printf("%d %s step=%d\n", event.Sequence, event.Type, event.Step)
}

terminal, ok := recorder.TerminalEvent()
if ok {
    fmt.Printf("terminal: %s status=%s\n", terminal.Type, terminal.Status)
}
```

For deterministic tests, inject a fixed `Clock` and `IDSource` so the same
fixture produces the same event type, order, and identifiers every run:

```go
recorder := lebro.NewRunRecorder()
agent, _ := lebro.NewAgent(lebro.AgentConfig{
    Definition: lebro.AgentDefinition{ID: "agent", Model: "fixture"},
    Model:      model,
    Listener:   recorder,
    Clock:      lebro.NewFixedClock(time.Unix(0, 0)),
    IDSource:   lebro.NewFixedIDSource(
        []lebro.RunID{"run-1"},
        []lebro.StepID{"step-1"},
    ),
})
```

| Event type | Description |
|------------|-------------|
| `RunEventStarted` | Run begins, before the first model call |
| `RunEventModelStarted` | Before each model Generate call |
| `RunEventModelFinished` | After a model call returns (usage, finish reason, duration) |
| `RunEventToolRequested` | Model requested a tool call (once per call) |
| `RunEventToolStarted` | Before a tool handler is invoked |
| `RunEventToolFinished` | After a tool handler returns (state, duration, error) |
| `RunEventSucceeded` | Terminal: run completed successfully |
| `RunEventFailed` | Terminal: run failed |
| `RunEventCancelled` | Terminal: run was cancelled |
| `RunEventSuspended` | Run suspended at a step boundary (non-terminal; resumes with `RunEventResumed`) |
| `RunEventResumed` | Previously suspended run resumed from its durable snapshot |

## File-backed SQLite storage

`lebro.NewSQLiteStore` opens a file-backed SQLite database (pure Go via
`modernc.org/sqlite`, no CGO). Call `Migrate` once to install the schema, then
use the same `Store` / repository interfaces as `NewMemoryStore`. Records
survive process restarts:

```go
store, err := lebro.NewSQLiteStore("state.db")
if err != nil {
	panic(err)
}
defer store.Close()

if err := store.Migrate(ctx); err != nil {
	panic(err)
}
if err := store.Transaction(ctx, func(ctx context.Context, repositories lebro.Repositories) error {
	return repositories.Threads().CreateThread(ctx, lebro.ThreadRecord{ID: "thread-1"})
}); err != nil {
	panic(err)
}
```

Concurrent writers are serialized per SQLite's transactional locking; a lock
blocked longer than the busy timeout surfaces as `lebro.ErrConflict`, which
callers may retry, and migration failures roll back and leave the database
unchanged. Both adapters pass the shared storage contract suite in
[testkit](internal/testkit/contract_storage.go).

## PostgreSQL storage

`lebro.NewPostgresStore` opens a PostgreSQL connection pool (pure Go via
`github.com/jackc/pgx/v5`, no CGO) for production deployments where multiple
processes share threads and workflow state. Call `Migrate` once to install
the schema, then use the same `Store` / repository interfaces as the other
adapters:

```go
store, err := lebro.NewPostgresStore("postgres://user:pass@host:5432/db?sslmode=disable", lebro.PostgresStoreOptions{})
if err != nil {
	panic(err)
}
defer store.Close()

if err := store.Migrate(ctx); err != nil {
	panic(err)
}
if err := store.Transaction(ctx, func(ctx context.Context, repositories lebro.Repositories) error {
	return repositories.Threads().CreateThread(ctx, lebro.ThreadRecord{ID: "thread-1"})
}); err != nil {
	panic(err)
}
```

Transactions use `READ COMMITTED` isolation. Serialization failures
(SQLSTATE 40001) and lock timeouts (55P03) surface as `lebro.ErrConflict`
for callers to retry. Foreign-key violations (23503) map to
`lebro.ErrNotFound`. The schema is versioned in a `schema_migrations` table;
migrations are append-only and run atomically, so a failed migration leaves
the database in its prior state. Required indexes:

| Index | Purpose |
|-------|---------|
| `idx_messages_thread_seq` | Ordered message listing per thread |
| `idx_workflow_snapshots_run_seq` | Ordered snapshot listing per run |

The adapter passes the shared storage contract suite. Set
`LEBRO_POSTGRES_TEST_DSN` to run the PostgreSQL tests against a disposable
database.

## Vector storage

Vector storage is optional and separate from the core `Store` interface. A
provider-neutral `VectorStore` provides index management, embedding
upsert/delete, and cosine-similarity search with metadata filtering. Agent
and workflow packages never reference vector types, so the core runtime
remains usable with no vector dependency.

```go
store := lebro.NewMemoryVectorStore()
store.CreateIndex(ctx, "documents", 128)
store.Upsert(ctx, []lebro.EmbeddingRecord{
    {ID: "doc-1", Index: "documents", Vector: embedding, Metadata: json.RawMessage(`{"source":"api"}`)},
})
results, _ := store.Search(ctx, lebro.SimilarityQuery{
    Vector: query,
    Index:  "documents",
    Filter: lebro.VectorMetadataFilter{Match: map[string]json.RawMessage{"source": json.RawMessage(`"api"`)}},
    TopK:   10,
    MinScore: 0.7,
})
```

Three adapters ship:

| Adapter | Backend | Search | Migrations |
|---------|---------|--------|------------|
| `MemoryVectorStore` | In-process | Brute-force cosine | No-op |
| `SQLiteVectorStore` | SQLite (JSON TEXT) | Brute-force cosine | `vector_schema_migrations` table |
| `PostgresVectorStore` | PostgreSQL + pgvector | `<=>` operator | `vector_schema_migrations` table |
| `QdrantVectorStore` | Qdrant gRPC | Cosine collection search | Qdrant collection per index |

All adapters pass the shared `VectorContractSuite`. PostgreSQL vector tests
are gated by `LEBRO_POSTGRES_TEST_DSN` and require the pgvector extension.
The pgvector adapter uses `github.com/pgvector/pgvector-go` (pure Go, no CGO).
Qdrant vector tests are gated by `LEBRO_QDRANT_TEST_HOST` and optionally
`LEBRO_QDRANT_TEST_PORT`. Qdrant uses a deterministic UUID for each Lebro
record ID and preserves original ID, content, metadata, and exact metadata
filters in point payload. In shared collections, callers must include their
tenant/owner metadata condition in every query; this adapter does not infer or
enforce a tenant policy.

## Retrieval-augmented generation

Retrieval is assembled from four provider-neutral contracts — `Chunker`,
`EmbeddingModel`, `Retriever`, and the existing `VectorStore` — and is exposed
to an agent as an ordinary schema-backed `Tool`. Nothing in the agent loop
references RAG types, so the core runtime stays usable with no RAG or vector
dependency, and no hidden retrieval behavior is added to any agent.

```go
chunker, _ := lebro.NewCharacterChunker(lebro.CharacterChunkerConfig{Size: 1000, Overlap: 200})
embeddings, _ := openai.NewEmbedder(openai.EmbedderConfig{
    APIKey:    os.Getenv("OPENAI_API_KEY"),
    Model:     "text-embedding-3-small",
    Dimension: 1536,
})
store := lebro.NewMemoryVectorStore()

indexer, _ := lebro.NewIndexer(lebro.IndexerConfig{
    Chunker:    chunker,
    Embeddings: embeddings,
    Store:      store,
    Index:      "handbook",
})
indexer.EnsureIndex(ctx)
indexer.Ingest(ctx, lebro.Document{
    ID:       "refunds",
    Content:  policyText,
    Source:   "policies/refunds.md",
    Metadata: json.RawMessage(`{"visibility":"public"}`),
})
```

`Ingest` chunks the document, embeds the chunks in batches, and upserts them
with their provenance. Chunk IDs are `"<DocumentID>#<Index>"`, so re-ingesting
an unchanged document replaces its records instead of duplicating them. A
document that shrinks leaves surplus trailing chunks behind; delete the previous
`IndexResult.ChunkIDs` that no longer appear.

Retrieval takes natural language — the retriever embeds the query itself, so
neither the caller nor the model handles vectors:

```go
retriever, _ := lebro.NewVectorRetriever(lebro.VectorRetrieverConfig{
    Embeddings: embeddings,
    Store:      store,
    Index:      "handbook",
    TopK:       5,
    Filter: lebro.VectorMetadataFilter{
        Match: map[string]json.RawMessage{"visibility": json.RawMessage(`"public"`)},
    },
})

hits, _ := retriever.Retrieve(ctx, lebro.RetrievalQuery{Query: "refund window"})
for _, hit := range hits {
    fmt.Printf("%s (%s) score=%.3f\n", hit.DocumentID, hit.Source, hit.Score)
}
```

Every hit carries stable source metadata — `DocumentID`, `Source`, and `Index` —
recorded at ingestion under the reserved metadata keys `document_id`, `source`,
and `chunk_index`. A document whose own metadata uses one of those keys is
rejected rather than silently overwritten, so reported provenance is always the
provenance the indexer wrote.

`NewRetrievalTool` exposes a `Retriever` as a `Tool`, which an agent selects
through ordinary model tool-calling inside the existing bounded loop:

```go
tool, _ := lebro.NewRetrievalTool(lebro.RetrievalToolConfig{
    ID:          "search_handbook",
    Retriever:   retriever,
    Description: "Search the customer handbook for relevant passages.",
    TopK:        3,
    MaxTopK:     5,
})

registry, _ := lebro.NewToolRegistry(lebrojsonschema.NewCompiler())
registry.Register(tool)

agent, _ := lebro.NewAgent(lebro.AgentConfig{
    Definition: lebro.AgentDefinition{
        ID:    "support",
        Tools: []lebro.ToolID{"search_handbook"},
    },
    Model: model,
    Tools: registry,
})
```

The tool's input schema accepts only `query` and an optional `top_k`, with
`additionalProperties: false`. Retrieval **scope is configuration, not a model's
choice**: the metadata filter is fixed at construction and is not model-settable,
a model-supplied `top_k` is clamped, and a caller filter that names an enforced
key loses to the configured value. A model therefore chooses what to search for
but not what it is allowed to read. The clamp falls back from `MaxTopK` to
`TopK` to `DefaultRetrievalTopK`, so it still bounds a tool configured with
neither. An empty result set is a success with an empty `chunks` array rather
than an error, so a model never branches on response shape.

`CharacterChunker` measures `Size` and `Overlap` in runes, so a multi-byte
character is never split across chunks. It is deliberately the simple initial
strategy: it assumes nothing about language, markup, or sentence structure.
Content that is not valid UTF-8 is rejected rather than silently rewritten,
since rune conversion would replace each invalid byte and index text that
differs from what was submitted; decode or sanitize upstream, where the right
substitution is known.

Stage failures are normalized as `*RAGError` naming the stage that failed, with
the underlying provider or store error preserved:

| Sentinel | Stage |
|---|---|
| `ErrRAGInvalidDocument` | Document failed the ingestion contract |
| `ErrRAGChunking` | Chunker rejected the document or emitted an invalid chunk |
| `ErrRAGEmbedding` | Embedding call failed, or returned a wrong count or dimension |
| `ErrRAGIndexing` | Vector store rejected the records |
| `ErrRAGRetrieval` | Query was invalid, or the search failed |

All are `errors.Is`-compatible, and `errors.As` reaches the wrapped
`*ModelError` or `ErrVector*` sentinel behind a stage failure, so one retry
policy can cover embedding calls and chat calls alike.

The `openai` package's `NewEmbedder` implements `EmbeddingModel` against any
OpenAI-compatible `/embeddings` endpoint, reusing the chat adapter's error
classification. It reorders the response by each item's declared index rather
than trusting wire order, and verifies the returned count and every vector's
width, so a provider that drops or truncates an item fails loudly instead of
writing a misaligned index.

## Typed linear workflow execution

`lebro.NewLinearWorkflow` composes ordered, named steps whose handlers are
ordinary Go functions. Each step may declare optional JSON Schemas for its
input and output; the executor compiles them once and validates every handoff
so a step only runs when its input matches the previous step's validated
output.

```go
wf, err := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
    Definition:     lebro.WorkflowDefinition{ID: "double-and-add", Name: "Double and Add One"},
    SchemaCompiler: lebrojsonschema.NewCompiler(),
    Steps: []lebro.Step{
        {
            Definition: lebro.StepDefinition{
                ID:           "double",
                InputSchema:  json.RawMessage(`{"type":"integer"}`),
                OutputSchema: json.RawMessage(`{"type":"integer"}`),
            },
            Handler: lebro.StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
                var n int
                if err := json.Unmarshal(input, &n); err != nil {
                    return nil, err
                }
                return json.Marshal(n * 2)
            }),
        },
        {
            Definition: lebro.StepDefinition{
                ID:           "add-one",
                InputSchema:  json.RawMessage(`{"type":"integer"}`),
                OutputSchema: json.RawMessage(`{"type":"integer"}`),
            },
            Handler: lebro.StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
                var n int
                if err := json.Unmarshal(input, &n); err != nil {
                    return nil, err
                }
                return json.Marshal(n + 1)
            }),
        },
    },
})

result, err := wf.Run(context.Background(), lebro.WorkflowRunInput{
    Input: json.RawMessage(`5`),
})

var final int
if err := result.DecodeOutput(&final); err != nil {
    panic(err) // final == 11
}
```

A failed step stops the workflow immediately and the returned
`*lebro.WorkflowError` identifies the failing step by its 1-indexed position and
declared ID. `errors.Is` works against the `lebro.ErrWorkflow*` sentinels:

| Kind | Cause |
|------|-------|
| `WorkflowErrorInvalidStepInput` | Step input schema rejected the handoff from the previous step |
| `WorkflowErrorInvalidStepOutput` | Step output schema rejected a handler result |
| `WorkflowErrorStepFailed` | Handler returned an error |
| `WorkflowErrorStepPanicked` | Handler panicked during invocation |
| `WorkflowErrorCancelled` | Run context cancelled before completion |
| `WorkflowErrorNoBranchMatched` | Branching step found no matching predicate and no default |
| `WorkflowErrorBranchConditionFailed` | A branch predicate returned an error during evaluation |
| `WorkflowErrorInvalidBranchInput` | Branching step input failed its InputSchema |
| `WorkflowErrorFanOutBranchFailed` | A fan-out child branch returned a terminal failure |
| `WorkflowErrorFanOutInputMapperFailed` | A fan-out branch InputMapper returned an error |
| `WorkflowErrorInvalidFanOutInput` | Fan-out step input failed its InputSchema |

When a `RunListener` is configured, the executor emits ordered lifecycle events
through the same run-event model as the agent loop: `run_started`, per-step
`step_started` / `step_finished`, and a terminal `run_succeeded` /
`run_failed` / `run_cancelled`. Events carry stable run IDs, monotonic sequence
numbers, and step identifiers, so the full execution is inspectable without an
observability backend. When the listener is nil, workflow behavior is unchanged.

Run the linear-workflow example (no network or API key required):

```sh
go run ./examples/workflow-linear
```

### Durable workflow runs

`LinearWorkflowConfig.Store` optionally binds a linear workflow to a `Store`
so run state survives at safe step boundaries. When set, the executor persists
the run record as `Running` before the first step and, after every successful
step, writes a `WorkflowSnapshotRecord` plus an updated `WorkflowRunRecord`
inside one `Store.Transaction` so the boundary is atomic. A persistence failure
fails the run with `WorkflowErrorStepFailed` wrapping the storage error, so a
process restart never observes a partially persisted step.

```go
store, _ := lebro.NewSQLiteStore("lebro.db")
defer store.Close()
_ = store.Migrate(context.Background())

wf, _ := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
    Definition: lebro.WorkflowDefinition{ID: "durable", Version: "v1"},
    Steps:      /* ... */,
    Store:      store,
})

result, _ := wf.Run(context.Background(), lebro.WorkflowRunInput{
    Input: json.RawMessage(`5`),
})

run, _ := store.WorkflowRuns().GetWorkflowRun(context.Background(), result.ID)
// run.Status, run.CurrentStep, run.StepOutputs, run.Failure are persisted.

page, _ := store.WorkflowRuns().ListWorkflowRuns(
    context.Background(),
    lebro.WorkflowRunFilter{Status: lebro.RunStatusSucceeded},
    lebro.PageRequest{Limit: 50},
)

snapshots, _ := store.WorkflowSnapshots().ListWorkflowSnapshots(
    context.Background(),
    result.ID,
    lebro.PageRequest{},
)
// snapshots.Records[i].State is a versioned envelope of the step boundary.
```

`WorkflowRunRecord` carries `CurrentStep`, `CurrentStepID`, `StepOutputs`
(ordered completed outputs), `Failure` (`*WorkflowFailureData` with kind,
step, step ID, and message), and `WorkflowVersion` (the opaque
definition/version reference from `WorkflowDefinition.Version`).
`WorkflowSnapshotRecord` carries `SchemaVersion`; the executor writes `1` and
readers tolerate `0` as legacy. When `Store` is nil, workflow behavior is
unchanged. SQLite and PostgreSQL add append-only migrations for the new
columns; the in-memory adapter handles them via struct copy.

Run the durable-workflow example (no network or API key required):

```sh
go run ./examples/workflow-durable
```

### Suspend and resume

A step handler can suspend the run with a typed resume contract so the workflow
can wait for an external decision or input. The handler returns a
`*SuspendError` wrapping a `SuspendSignal`; the executor detects it via
`errors.Is(err, lebro.ErrWorkflowSuspend)`, validates the signal's contract
against the step's `StepDefinition.SuspendSchema`, and persists a suspend
snapshot plus a `RunStatusSuspended` run record. The run's `WorkflowRunResult`
carries a non-nil `Suspend` with the validated contract and opaque payload.
A step that suspends without a `SuspendSchema` is rejected as an invalid step
output so a process restart never observes an unvalidated resume contract.

The contract is the **expected resume value**: `SuspendSchema` validates its
shape at suspend time, and `Resume` validates the resume input against the same
schema and additionally requires it to equal the persisted contract value, so
the suspending step's published expectation constrains resume.

```go
wf, _ := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
    Definition:     lebro.WorkflowDefinition{ID: "approval", Version: "v1"},
    SchemaCompiler: lebrojsonschema.NewCompiler(),
    Store:          store,
    Steps: []lebro.Step{
        {Definition: lebro.StepDefinition{
            ID:            "await-approval",
            SuspendSchema: json.RawMessage(`{"type":"object","required":["approved"],"properties":{"approved":{"const":true}}}`),
        }, Handler: lebro.StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
            return nil, &lebro.SuspendError{Signal: lebro.SuspendSignal{
                Contract: json.RawMessage(`{"approved":true}`),
                Payload:  json.RawMessage(`{"pending":"human"}`),
            }}
        })},
        // ...later steps run after Resume.
    },
})

suspended, _ := wf.Run(ctx, lebro.WorkflowRunInput{Input: json.RawMessage(`"start"`)})
// suspended.Status == lebro.RunStatusSuspended
// suspended.Suspend.Contract == {"approved":true}

// Invalid resume input is rejected before any step runs; the snapshot is
// left untouched.
_, err := wf.Resume(ctx, lebro.WorkflowResumeInput{RunID: suspended.ID, Input: json.RawMessage(`{"approved":false}`)})
// errors.Is(err, lebro.ErrInvalidResumeInput)

// Valid resume continues from the durable snapshot without re-executing
// completed steps.
resumed, _ := wf.Resume(ctx, lebro.WorkflowResumeInput{RunID: suspended.ID, Input: json.RawMessage(`{"approved":true}`)})
// resumed.Status == lebro.RunStatusSucceeded
```

`Resume` requires a bound `Store` and a `RunStatusSuspended` run whose
`WorkflowID` matches the workflow instance; resuming a run bound to a different
workflow returns a step failure so unrelated handlers are never executed. The
stored run record stays `Suspended` until the first resumed step persists, so
a process crash before any step commits leaves the run resumable rather than
orphaned in `Running`. Run history records
`RunEventSuspended` ("run_suspended") at the suspend boundary and
`RunEventResumed` ("run_resumed") at resume start; neither is terminal. The
suspend snapshot envelope version is `2` and adds the optional `suspend`
field; readers tolerate `0` and `1` as legacy.

Run the suspend/resume example (no network or API key required):

```sh
go run ./examples/workflow-suspend-resume
```

### Bounded parallel fan-out and join

A `StepDefinition` may declare a `FanOut` to run independent branches
concurrently within a configured `MaxParallel` bound and join their results in
declaration order. Each `FanOutBranch` has a `Name`, an optional
`InputMapper` (to derive a branch-specific input from the fan-out input), and
an ordered list of `Steps` that run sequentially within the branch. The joined
output is a JSON array of `{"name":"...","output":...}` objects in declaration
order, regardless of completion timing — so downstream steps receive a
deterministic result.

`FailurePolicy` controls sibling cancellation on failure: `FanOutFailFast`
(default) cancels remaining and in-flight siblings after the first child
failure; `FanOutCollectAll` lets every branch finish and returns the
lowest declared-index failure. External context cancellation cancels all
branches and returns a cancelled error.

A fan-out step must not declare a `Handler`, `OutputSchema`, `SuspendSchema`,
`Retry`, or conditional `Branches`. Branches must have unique non-empty names
and at least one step. `WorkflowRunResult.FanOut` exposes each child branch's
terminal state and the join outcome; the persisted snapshot carries the same
records so the join is durable and inspectable.

```go
wf, err := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
    Definition: lebro.WorkflowDefinition{ID: "parallel-enrich"},
    Steps: []lebro.Step{
        {
            Definition: lebro.StepDefinition{
                ID: "fanout",
                FanOut: &lebro.FanOut{
                    MaxParallel: 2,
                    Branches: []lebro.FanOutBranch{
                        {Name: "enrichment", Steps: []lebro.Step{
                            {Definition: lebro.StepDefinition{ID: "enrich"}, Handler: enrichHandler},
                        }},
                        {Name: "risk-check", Steps: []lebro.Step{
                            {Definition: lebro.StepDefinition{ID: "risk"}, Handler: riskHandler},
                        }},
                    },
                },
            },
        },
        {Definition: lebro.StepDefinition{ID: "summarize"}, Handler: summarizeHandler},
    },
})

result, err := wf.Run(ctx, lebro.WorkflowRunInput{Input: json.RawMessage(`{"id":"user-123"}`)})
// result.Output is the summarize step's output.
// result.FanOut[0].Branches exposes each branch's status and output.
```

Run the fan-out-join example (no network or API key required):

```sh
go run ./examples/workflow-fanout-join
```

## Durable schedules and recurring workflow runs

A `Scheduler` fires persisted schedules whose next fire time has arrived,
reusing `LinearWorkflow.Run` for each execution. Because due schedules are
reloaded from the `Store` on every tick rather than kept in memory, a schedule
persisted before a process restart resumes automatically — the acceptance test
constructs a fresh scheduler over an existing store and the first tick fires the
overdue schedule.

A `ScheduleRecord` carries the cron or `@every` spec, the target `WorkflowID`,
the run input, a `ConcurrencyPolicy`, and the persisted `NextFireAt`. Each fire
appends a `ScheduleExecutionRecord` to durable history with a `succeeded`,
`failed`, `skipped`, or `missed` status. `Scheduler.Tick(ctx, now)` is the
deterministic core (drive it with a fixed `Clock` in tests); `Scheduler.Start` /
`Scheduler.Stop` wrap it in a background loop for production.

```go
store, _ := lebro.NewSQLiteStore("lebro.db")
defer store.Close()
_ = store.Migrate(context.Background())

// Register the workflow the schedule triggers.
wf, _ := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
    Definition: lebro.WorkflowDefinition{ID: "nightly-digest"},
    Steps:      /* ... */,
    Store:      store,
})

// Persist a schedule: every day at 02:00, skip if the prior run is still going.
next, _ := lebro.ParseCronSpec("0 2 * * *")
fire, _ := next.Next(time.Now().UTC())
_ = store.Schedules().SaveSchedule(context.Background(), lebro.ScheduleRecord{
    ID:          "digest-daily",
    WorkflowID:  "nightly-digest",
    Spec:        "0 2 * * *",
    Concurrency: lebro.ConcurrencySkip,
    NextFireAt:  &fire,
    CreatedAt:   time.Now().UTC(),
    UpdatedAt:   time.Now().UTC(),
})

scheduler, _ := lebro.NewScheduler(lebro.SchedulerConfig{
    Store:    store,
    Resolver: lebro.WorkflowMap{"nightly-digest": wf},
})

// Production: run a background loop that ticks every minute.
_ = scheduler.Start(context.Background())
defer scheduler.Stop()

// Or drive it deterministically:
result, _ := scheduler.Tick(context.Background(), time.Now().UTC())
// result.Fired, result.Skipped, result.Missed, result.Executions.

history, _ := store.ScheduleExecutions().ListScheduleExecutions(
    context.Background(), "digest-daily", lebro.PageRequest{},
)
// history.Records[i].Status is succeeded / failed / skipped / missed.
```

`ParseCronSpec` accepts a five-field cron expression (`minute hour dom month
dow`, with `*`, lists, ranges, and steps) or a fixed interval `@every 30m`; it
is standard-library-only, so the core module gains no dependency. Under
`ConcurrencySkip` a fire that arrives while a prior run of the same schedule is
still in flight is recorded as `skipped` and the schedule still advances; under
`ConcurrencyAllow` (the default) it runs regardless. When a schedule is overdue
by more than one interval — for example after an outage — only the most recent
occurrence runs and the earlier ones are recorded as `missed`, bounded by
`SchedulerConfig.MaxCatchUp`. The `ScheduleRepository` and
`ScheduleExecutionRepository` are part of the `Store` contract across the
memory, SQLite, and PostgreSQL adapters and are enforced by `NewPolicyStore`.

Run the schedule example (no network or API key required):

```sh
go run ./examples/workflow-schedule
```

## Agent and tool workflow steps

`lebro.NewAgentStep` adapts an `Agent` (or another `Workflow`) to a typed
workflow step. The previous JSON value becomes a user message: JSON strings
are unquoted, while objects and arrays are passed as JSON text. The step returns
validated structured output when available; otherwise it returns the final
assistant content as a JSON string. The parent workflow's context, thread ID,
and metadata are forwarded to the agent run.

`lebro.NewToolStep` adapts a `RegisteredTool` resolved from a `ToolRegistry`.
It passes workflow input as tool arguments and returns the schema-validated tool
output. Registering first keeps both tool schema boundaries enforced.

```go
registered, _ := registry.Resolve("weather.lookup")
agentStep, _ := lebro.NewAgentStep(agent)
toolStep, _ := lebro.NewToolStep(registered)

wf, err := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
    Definition: lebro.WorkflowDefinition{ID: "weather-summary"},
    Steps: []lebro.Step{
        {Definition: lebro.StepDefinition{ID: "weather"}, Handler: toolStep},
        {Definition: lebro.StepDefinition{ID: "summarize"}, Handler: agentStep},
    },
})
```

When the workflow and nested agent share a `RunListener`, every nested agent
event carries `ParentRunID`, `ParentStepID`, and `ParentStep`. This preserves
the parent workflow and invoking step correlation while nested runs retain
their own run and step IDs.

Run the combined workflow example (no network or API key required):

```sh
go run ./examples/workflow-agents-tools
```

## Supervised agent delegation

`lebro.NewSubagent` exposes an `Agent` (or another `Workflow`) as a named,
schema-backed capability that a supervising agent can delegate focused work to.
A `Subagent` implements `Tool`, so registering one in a `ToolRegistry` and
listing its ID in the supervisor's definition is all that is required: the
supervisor selects it through ordinary model tool-calling, and the delegation
inherits the same execution boundary as any other tool. Arguments are
schema-validated before the child starts, results are validated on the way
back, handler panics are contained, and the supervisor's tool allow-list
governs which subagents it may reach.

```go
research, _ := lebro.NewSubagent(lebro.SubagentConfig{
    ID:          "delegate.research",
    Agent:       researcher,
    Description: "Delegate a factual research question to the researcher.",
    MaxSteps:    4,
    Deadline:    30 * time.Second,
})
_ = registry.Register(research)

supervisor, err := lebro.NewAgent(lebro.AgentConfig{
    Definition: lebro.AgentDefinition{
        ID:    "supervisor",
        Tools: []lebro.ToolID{"delegate.research"},
    },
    Model: model,
    Tools: registry,
})
```

The default delegation contract takes a required `task` and an optional
`context` string. Message roles stay application-controlled, so a supervisor
cannot inject a system prompt or a synthetic tool result into the child
transcript. The result reports the child's `agent_id`, `run_id`, `status`, and
`output`, so a supervisor can reason about a delegation without a second
lookup. Both schemas can be overridden with `InputSchema` and `OutputSchema`.

Delegated runs are bounded independently of the parent. `MaxSteps` narrows the
child's step budget for the duration of the delegation without mutating the
target agent, so concurrent delegations to the same agent keep their own
budgets. `Deadline` is layered on the parent context: a child that exhausts its
own deadline fails the delegation and returns control to the supervisor, while
the parent context stays live. A parent that is itself cancelled still cancels
the child.

Thread context is isolated by default. A delegated run receives a fresh
transcript containing only the delegated task; the parent's `ThreadID` and
metadata are withheld unless `ShareThread` or `ShareMetadata` opts in. Sharing
is configured per subagent rather than per call, so a supervisor cannot widen a
child's view of the parent thread by changing what it sends.

Parent and child runs stay correlated through the run event stream. Every child
event carries the parent's `ParentRunID`, `ParentStepID`, and `ParentStep`,
identifying the exact supervisor step that started the delegation, while the
child keeps its own run ID — namespaced under the parent run so the two are
never confused.

Nesting is permitted: a delegated agent may itself hold subagent tools, and
each level is bounded by its own `MaxSteps` and `Deadline`. There is no global
depth cap, so a recursive topology must be bounded by the deadlines its levels
declare.

Run the supervised delegation example (no network or API key required):

```sh
go run ./examples/supervised-delegation
```

## Agent networks

`lebro.NewNetwork` coordinates named specialist workflows through an explicit
`SpecialistRouter`. Every specialist handoff preserves the caller context, so
identity, policy, cancellation, and deadlines flow into child execution.
Networks require a non-empty user message, bound traversal with `MaxHops` and
`Deadline`, reject a repeated specialist ID, and return typed `NetworkError`
values for invalid input, selection failures, cycles, exhausted hops, failed
specialists, and authorization denial. A handoff requires a non-empty user
task and each specialist must return assistant text or structured output.

```go
network, err := lebro.NewNetwork(lebro.NetworkConfig{
    Definition: lebro.WorkflowDefinition{ID: "knowledge-network"},
    Router: router,
    Specialists: []lebro.NetworkSpecialist{
        {ID: "research", Workflow: researcher, Description: "Research facts."},
    },
    Store: store,
})
result, err := network.Run(ctx, lebro.RunInput{
    Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "What is Lebro?"}},
})
```

Each route records candidates, selected specialist, child run ID, status, and
failure in `WorkflowRunRecord.StepOutputs`; existing Memory, SQLite, and
Postgres stores persist those records without a migration. Configure a
`RunListener` to receive `RunEventRouteSelected` events. `RuleRouter` performs
one handoff then completes; a multi-hop router uses `RoutingRequest.Hops` and
returns `RoutingDecision{Complete: true}` when finished.

Run deterministic example:

```sh
go run ./examples/agent-network
```

## Token and event streaming with cancellation

`lebro.StreamingModel` extends the provider-neutral `Model` interface with
`Stream`, which returns a `StreamReader` over ordered `StreamDelta` values.
Each delta carries text, a tool call, structured output, or a terminal finish
reason and usage. `lebro.AsStreamingModel` adapts a `Model` into a
`StreamingModel` when the concrete value implements `Stream`, returning nil
otherwise so callers fall back to `Generate`.

`Agent.RunStream` runs the bounded agent loop against a `StreamingModel` and
returns a `StreamRun` handle. The caller drains `Deltas` (`<-chan StreamDelta`)
in real time, then calls `Wait` (or `Drain`) for the terminal `RunResult` and
error. The caller must `defer` `StreamRun.Cancel` so goroutine and channel
resources are released even when the stream is abandoned before completion;
closing the consumer does not leak goroutines.

Cancellation propagates through the provider stream, tool execution, and the
loop itself: cancelling the context (or calling `StreamRun.Cancel`) stops
active work, closes the delta channel, and returns a result with status
`RunStatusCancelled` wrapping `ErrAgentCancelled`. A `RunEventDelta`
(`"model_delta"`) run event is emitted for each delta and flows through the
existing `RunListener` / `RunRecorder` infrastructure. Streaming and
non-streaming runs produce equivalent final records: when the configured model
does not implement `StreamingModel`, `RunStream` falls back to `Generate` and
emits a single delta per step.

```go
run, err := agent.RunStream(ctx, lebro.RunInput{
    Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "stream me a poem"}},
})
if err != nil { return err }
defer run.Cancel()

for delta := range run.Deltas {
    if delta.Text != "" {
        fmt.Print(delta.Text)
    }
}
result, err := run.Wait()
```

The OpenAI-compatible adapter implements `StreamingModel` over Server-Sent
Events, mapping text deltas, usage, finish reasons, and error events into the
neutral protocol.

Run the streaming example (no network or API key required):

```sh
go run ./examples/streaming
```

## Embeddable HTTP server and OpenAPI contract

`httpapi` serves registered agents and workflows over HTTP and publishes the
resulting contract as an OpenAPI 3.1 document. It is optional and depends only
on the standard library, so an application that does not serve HTTP never
compiles it in — and one that does gains no third-party dependency.

Only explicitly registered primitives are routable. `NewServer` returns a server
with no agents and no workflows; each is registered with `ExposeAgent` or
`ExposeWorkflow`, so a primitive that was not registered cannot be reached by
guessing an identifier.

```go
server := httpapi.NewServer(httpapi.ServerConfig{
    Title:   "my-app",
    Version: "1.0.0",
    Store:   store, // optional: enables thread routes and thread-bound runs
})
_ = server.ExposeAgent(agent)
_ = server.ExposeWorkflow(workflow)

_ = http.ListenAndServe(":8080", server.Handler())
```

| Route | Purpose |
|---|---|
| `GET /health` | Readiness and exposed primitive counts |
| `GET /agents`, `GET /workflows` | Enumerate what is exposed |
| `POST /agents/{id}/runs` | Run an agent to completion |
| `POST /agents/{id}/runs/stream` | Run an agent, streaming Server-Sent Events |
| `POST /workflows/{id}/runs` | Run a workflow to completion |
| `GET /threads/{id}`, `GET /threads/{id}/messages` | Read durable conversations |
| `GET /openapi.json` | The generated contract |

Requests supply user text only. Message roles are fixed server-side, so a client
cannot inject a system prompt or forge an assistant turn: the agent's configured
instructions remain the only system message. Thread identifiers come from the
path or the `thread_id` query parameter, never from a request body, so one
caller cannot address another's conversation by naming it in a payload.

Failures are reported as stable `ErrorCode` values derived from the runtime's
normalized error kinds — `provider_failure` (502), `invalid_input` (400),
`step_limit_exhausted` (502), `cancelled` (499), and so on — with a fixed public
message. Internal error text never reaches the response body.

The stream route emits Server-Sent Events whose names reuse the `RunEventType`
vocabulary, and always terminates with exactly one terminal event, so a client
can distinguish a completed run from a dropped connection. The terminal event
carries the run's total token usage, summed across every model call. Closing the
connection cancels the run.

A request to a route that exists but not for that method is answered `405` with
an `Allow` header, rather than a `404` that would suggest the resource does not
exist. `HEAD` is served for every `GET` route.

Streamed deltas pass through a `Redactor` first. A nil `Redactor` selects
`DefaultRedactor`, which strips model-supplied tool-call arguments while passing
assistant text and structured output through — a zero-valued configuration
streams less rather than more. `PassthroughRedactor` opts out deliberately.

Authentication is deliberately absent: `ServerConfig.Middleware` wraps the
router, and the package stays neutral about the scheme. Workflow resume is not
exposed, matching the MCP server, because no durable atomic resume claim exists
yet.

`Server.OpenAPI` generates the document from the same route table that builds
the router, so a served route cannot be missing from it. Exposed agents and
workflows are named in their operations' descriptions, and each workflow's
declared input schema is embedded in its request body, so the published contract
is as precise as the runtime validation.

Run the HTTP server example (no network or API key required):

```sh
go run ./examples/http-server
```

## Typed Go client

`httpapi.Client` calls a lebro HTTP API with the same result and stream
contracts the in-process primitives use, so moving an agent out of process
changes how a call is constructed rather than how its answer is read. It ships
in the same package as the server and decodes the same wire types, so the
client's contract cannot drift from the server's.

```go
client, err := httpapi.NewClient(httpapi.ClientConfig{
    BaseURL: "https://api.example.com",
    Header: func(r *http.Request) {
        r.Header.Set("Authorization", "Bearer "+token)
    },
})

result, err := client.Run(ctx, "assistant", httpapi.RunRequest{
    Messages: []httpapi.MessageInput{{Content: "hello"}},
}, httpapi.WithThread("thread-1"))
```

Streamed runs mirror `lebro.StreamRun` — the same `Events`, `Cancel`, `Wait`,
and `Drain` — so remote and local streaming read identically:

```go
stream, err := client.RunStream(ctx, "assistant", request)
if err != nil {
    return err
}
defer stream.Cancel()

for event := range stream.Events {
    fmt.Print(event.Text)
}
result, err := stream.Wait()
```

`Cancel` closes the connection, which the server observes as a disconnect and
turns into a cancelled run; it also releases the reader goroutine when a caller
abandons the stream without draining it. The terminal event is consumed by the
stream and surfaces through `Wait`, so it cannot be mistaken for another delta
or missed by breaking out of the loop early. A stream that ends without one
reports `ErrStreamIncomplete` rather than an empty success — a dropped
connection and a failed run are different facts.

Failures arrive as `*APIError` carrying the server's `ErrorCode` and unwrapping
to the lebro sentinel that classifies it, so error handling does not change when
an agent moves behind HTTP:

```go
_, err := client.Run(ctx, "assistant", request)

if errors.Is(err, lebro.ErrAgentToolFailure) { /* same check as a local run */ }

var apiErr *httpapi.APIError
if errors.As(err, &apiErr) {
    log.Printf("code=%s status=%d", apiErr.Code, apiErr.StatusCode)
}
```

A run the caller's context ended reports the context error it observed, so an
elapsed deadline matches `context.DeadlineExceeded` and an explicit cancel
matches `context.Canceled` — the same distinction the in-process runtime makes.

A code maps to a sentinel only where every runtime error producing it shares
one. `invalid_input` and `invalid_output` are raised by both agent and workflow
failures, so they carry the code alone rather than a sentinel that would be
wrong half the time; the same applies to `invalid_request`,
`method_not_allowed`, and `internal_error`, which have no runtime counterpart.
Branch on `APIError.Code` for those.

`ContractVersion` names the wire contract both sides speak and is published in
the generated document as `info.x-lebro-contract-version`.
`Client.CheckCompatibility` compares major versions and reports
`ErrIncompatibleContract` on a mismatch. It is never called automatically, so a
run does not pay for a round trip it did not ask for; call it once at startup
when the server's version is not otherwise pinned.

Run the client example (no network or API key required):

```sh
go run ./examples/http-client
```

## Local Studio-style developer UI

The optional `studio` package serves a local developer UI for exercising
agents, tools, workflows, threads, and run traces — so iterating on a primitive
does not mean writing a one-off debugging program. It is off by default:
nothing in the root module imports it, and the UI is unreachable until a caller
builds its handler or calls `studio.Start`. UI state is never a runtime
requirement; the agents and workflows a program runs behave identically whether
or not a Studio is attached.

A Studio composes the surfaces lebro already has rather than reimplementing
them. Agent runs, workflow runs, streaming, and thread reads are served by
`httpapi`, mounted under `/api`. Ordered run events — the run, step, model, and
tool spans that record what happened and in what order, including tool calls,
their results, and the path a workflow took — are read from an observability
`Repository` through the small `studio.TraceLister` contract, which
`obsv.MemoryRepository` satisfies with no adapter. The web UI is served as a
static bundle; a build with no bundle serves a placeholder page so the API is
still usable.

```go
// QueueSize: -1 exports spans synchronously, so a run's trace is queryable the
// moment the run returns; the default buffers on a background goroutine.
observer, _ := obsv.New(obsv.Config{Repository: repository, QueueSize: -1})

agent, _ := lebro.NewAgent(lebro.AgentConfig{
    Definition: lebro.AgentDefinition{ID: "assistant", Name: "Assistant"},
    Model:      model,
    Listener:   observer, // feeds the trace views
    Store:      store,
})

studioServer, _ := studio.New(studio.Config{
    Agents:    []*lebro.Agent{agent},
    Workflows: []*lebro.LinearWorkflow{workflow},
    Store:     store,
    Traces:    repository,
})

// Serve for a browser, off until explicitly started:
_ = studio.Start(ctx, "127.0.0.1:4111", studio.Config{ /* ... */ })
```

The API mounted under `/api` exposes `GET /api/agents`, `POST
/api/agents/{id}/runs` (and `/runs/stream`), `GET /api/workflows`, `POST
/api/workflows/{id}/runs`, `GET /api/threads/{id}`, and Studio's read-only
`GET /api/studio/traces` and `GET /api/studio/traces/{id}`. The trace-detail
route returns a run's spans as a timeline ordered by start time, so a client
renders the events top to bottom.

The developer UI itself — a TanStack Start application replicating the feel of a
modern agent studio — lives in the separate `lebro-studio` project; its build
output is embedded into `studio/dist` to serve it from here. A from-source build
has an empty `studio/dist` and serves a placeholder page. To embed the real UI —
required before packaging a release — build it and copy the bundle in:

```sh
make studio-ui LEBRO_STUDIO_UI=/path/to/lebro-studio
```

Run the Studio example (no network or API key required):

```sh
go run ./examples/studio
```

## Dataset evaluation, scorers, and experiment runs

The optional `evals` package measures quality before a release: a versioned
dataset runs against an agent or workflow, scorers judge every case, and two
runs of the same dataset version are compared. Nothing in the root module
imports it, so an application that does not evaluate never compiles it in.

A dataset's `Version` is a content hash over its ordered, normalized cases
rather than a caller-supplied label, so "the same dataset version" is a
verifiable fact. Editing, adding, or reordering a case changes the version;
reformatting a case's JSON input does not, because inputs are canonicalized
before hashing.

```go
dataset := evals.Dataset{
	ID: "capitals",
	Cases: []evals.Case{
		{ID: "france", Input: json.RawMessage(`"What is the capital of France?"`), Expected: "Paris"},
		{ID: "japan", Input: json.RawMessage(`"What is the capital of Japan?"`), Expected: "Tokyo"},
	},
}

exact, err := evals.NewExactMatch(evals.ExactMatchConfig{TrimSpace: true})
graded, err := evals.NewModelScorer(evals.ModelScorerConfig{Model: grader})

experiment, err := evals.New(evals.ExperimentConfig{
	Name:       "release-candidate",
	Dataset:    dataset,
	Target:     evals.NewAgentTarget(agent),
	Scorers:    []evals.Scorer{exact, graded},
	Repository: evals.NewMemoryRepository(),
})
record, results, err := experiment.Run(ctx)
```

`Target` is what a dataset runs against. `NewAgentTarget` adapts any
`lebro.Workflow` — which `*lebro.Agent` implements — and `NewWorkflowTarget`
adapts a JSON-step workflow such as `*lebro.LinearWorkflow`, so one dataset
serves both without a caller-written adapter. A case carries both an `Input`
and `Messages`; each target reads the field it needs and reports
`ErrTargetUnsupportedCase` for a case it cannot invoke.

Deterministic rule scorers need no provider: `NewExactMatch`, `NewContains`,
`NewRegexp`, `NewJSONEquals`, and `NewNumericRange`. `NewModelScorer` grades
with a language model behind the existing model protocol — the caller supplies
the `lebro.Model`, so no provider dependency enters the module.

**A scorer failure is reported separately from the target's run outcome.** A
scorer that errors or panics lands in `CaseResult.ScorerFailures` and leaves the
target's `Status` and `Output` untouched; `ExperimentRecord` counts
`TargetFailures` and `ScorerFailures` apart for the same reason. "Did the agent
work?" and "could we measure it?" are different questions, and a broken judge
must not read as a broken agent. A panicking scorer is recovered, so one bad
scorer cannot abandon the run — the other scorers still measure that case.

Cases run across a bounded worker pool, but results are always ordered by
position in the dataset, so a stored record never depends on worker scheduling.

`Compare` reports the difference between two runs and refuses records whose
dataset ID or version disagree, because comparing two question sets would look
like a quality change while really being a change of subject:

```go
comparison, err := evals.Compare(baseline, candidate, baselineCases, candidateCases)
for _, regression := range comparison.Regressions {
	log.Printf("regressed: %s/%s — %s", regression.CaseID, regression.Scorer, regression.Reason)
}
```

Aggregate deltas alone can hide a real regression: two offsetting per-case
changes cancel out in the mean, which is why `Regressions` and `Improvements`
name the cases whose pass state moved.

Results persist through `evals.Repository`, deliberately separate from
`lebro.Store`, so evaluation data can live in a different database from threads
and workflow state and an evaluation write can never join the transaction that
persists a workflow step. The package ships `MemoryRepository`; a
database-backed implementation is an application concern.

Run the evaluation example (no network or API key required):

```sh
go run ./examples/evals-dataset
```

## Authentication and authorization hooks

lebro provides pluggable policy hooks so an application can enforce its own
tenant and user rules across agent runs, tool calls, workflow runs, and storage
operations. The library ships no identity provider and no concrete policy
engine: you authenticate a caller however you like, attach an `Identity` to the
run context, and supply a `Policy` that decides.

```go
type tenantPolicy struct{}

func (tenantPolicy) Authorize(_ context.Context, id lebro.Identity, action lebro.Action, res lebro.Resource) lebro.Decision {
    if action == lebro.ActionToolCall && !id.HasCapability("tools:call") {
        return lebro.Deny("missing tools:call capability")
    }
    return lebro.Allow()
}

agent, err := lebro.NewAgent(lebro.AgentConfig{
    Definition: lebro.AgentDefinition{ID: "weather-agent", Model: "gpt-4o", Tools: []lebro.ToolID{"weather.lookup"}},
    Model:      model,
    Tools:      registry,
    Policy:     tenantPolicy{},
})
if err != nil {
    panic(err)
}

ctx := lebro.WithIdentity(context.Background(), lebro.Identity{
    Subject:      "user-42",
    Tenant:       "acme",
    Capabilities: []lebro.Capability{"tools:call"},
})

result, err := agent.Run(ctx, lebro.RunInput{
    Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "Weather in Nairobi?"}},
})
if errors.Is(err, lebro.ErrPolicyDenied) {
    var denial *lebro.PolicyDenial
    errors.As(err, &denial)
    // denial.Action, denial.Resource, denial.Reason, denial.Subject
}
```

The `Policy` is consulted at four points, each with an `Action` and a
`Resource`:

| Action | When | Resource kind |
|--------|------|---------------|
| `ActionAgentRun` | Once at agent run start | `ResourceKindAgent` |
| `ActionToolCall` | Before each model-requested tool call, including subagent delegation | `ResourceKindTool` |
| `ActionWorkflowRun` | Once at `LinearWorkflow` run start | `ResourceKindWorkflow` |
| `ActionStorageRead` / `ActionStorageWrite` | Before each repository read/write on a `PolicyStore` | thread, message, workflow-run, snapshot |

Wrap any `Store` with `lebro.NewPolicyStore(store, policy)` to authorize storage
at method granularity; a denied read or write never reaches the underlying
store. Denials are typed and auditable: every denial is a `*lebro.PolicyDenial`
matched by `errors.Is(err, lebro.ErrPolicyDenied)`, agent and workflow denials
carry the `AgentErrorUnauthorized` / `WorkflowErrorUnauthorized` kinds and are
recorded in the run result and the terminal run event, and a denied tool call
reports the `ToolExecutionUnauthorized` state.

Identity flows through the run context, so subagent delegations and workflow
steps authorize against the same caller without extra wiring. A nil `Policy`
allows everything, so the core library is fully usable without one;
`lebro.AllowAllPolicy{}` is the explicit no-op.

## Messaging channel adapters

`channels` connects agents to conversational platforms. A platform delivers an
inbound message to a webhook, the agent runs through the ordinary streaming
pipeline, and the streamed reply is delivered back to the same conversation. The
package supplies the provider-neutral edges — an inbound message contract,
deterministic thread mapping, deduplication, and a streamed-reply relay — and
leaves the platform-specific edges to an `Adapter`. The root module gains no
provider dependency.

```go
agent, err := lebro.NewAgent(lebro.AgentConfig{
    Definition: lebro.AgentDefinition{ID: "assistant", Name: "Assistant"},
    Model:      model,
    Store:      store,
})
if err != nil {
    panic(err)
}

// The generic HMAC webhook adapter needs no platform SDK: a platform signs the
// raw body with a shared secret and sends the hex digest. ReplyURL is where the
// adapter POSTs each reply chunk; omit it only when the platform's reply channel
// is wired separately, since without it Send is a no-op and no reply is delivered.
adapter, err := channels.NewWebhookAdapter(channels.WebhookAdapterConfig{
    Platform: "webhook",
    Secret:   []byte(os.Getenv("CHANNEL_WEBHOOK_SECRET")),
    ReplyURL: os.Getenv("CHANNEL_REPLY_URL"),
})
if err != nil {
    panic(err)
}

server, err := channels.NewServer(channels.Config{
    Store:  store,
    Mapper: channels.NamespaceThreadMapper{Namespace: "prod"},
})
if err != nil {
    panic(err)
}
if err := server.ExposeAgent(agent, adapter); err != nil {
    panic(err)
}

// Serve the webhook route: /agents/{id}/channels/{platform}/webhook
http.ListenAndServe(":8080", server)
```

An inbound message maps deterministically to a durable thread: a `ThreadMapper`
derives a stable `ThreadID` from the conversation reference, so every message in
one platform conversation lands on one persisted transcript and a reply
continues it. The sender is mapped onto a `lebro.Identity` and carried on the
run context, so a configured `Policy` authorizes the run against the platform
user. Inbound content is always delivered as a user turn, so a platform payload
cannot forge a system or assistant turn.

Authentication is per adapter, in `Adapter.Verify`, before the request body is
trusted; the generic adapter recomputes an HMAC-SHA256 over the raw body bound
to a request timestamp, compares it in constant time, and rejects a timestamp
outside a configurable skew to prevent replay. Messaging platforms deliver at
least once, so a `Deduplicator` drops a redelivered message whose provider ID
was already processed; `StoreDeduplicator` persists the dedup window through a
`Store` (the default whenever a store is configured) so it survives a restart,
and `MemoryDeduplicator` keeps a bounded in-process window. The webhook handler
returns status codes a platform's retry logic can act on — 401 for a rejected
signature, 400 for an undecodable body, 200 for a handshake or an
already-processed delivery, and 500 for a run or delivery failure the platform
may safely redeliver.

## Voice

`voice` adds optional speech input and output around an agent without changing
core orchestration. A spoken utterance is transcribed to a canonical user turn,
the agent runs through the ordinary streaming pipeline, and the final reply is
synthesized back to audio. The package supplies the provider-neutral edges —
speech-recognition and synthesis interfaces, streaming with cancellation, and
translation to and from canonical messages — and leaves the provider-specific
work to optional adapters. The root module gains no provider dependency.

```go
agent, err := lebro.NewAgent(lebro.AgentConfig{
    Definition: lebro.AgentDefinition{ID: "assistant", Name: "Assistant"},
    Model:      model,
})
if err != nil {
    panic(err)
}

// recognizer and synthesizer are optional adapters implementing the voice
// interfaces; either half may be left nil for a listen-only or speak-only setup.
session, err := voice.NewSession(voice.SessionConfig{
    Voice: voice.Voice{Recognizer: recognizer, Synthesizer: synthesizer},
    Agent: agent,
})
if err != nil {
    panic(err)
}

// One voice turn: transcribe the audio, run the agent, synthesize the reply.
result, err := session.Turn(ctx, voice.TurnInput{Audio: audio}, func(chunk voice.AudioChunk) error {
    return play(chunk) // deliver each audio chunk as it arrives
})
```

Only a final transcript starts a run, and it always maps to a user turn through
`TranscriptMessage`, so a speaker's audio cannot forge a system or assistant
turn; the agent's final assistant turn is projected back to text with
`AssistantText`, so a spoken reply and a text reply speak the same words.
Recognition and synthesis stream through `RecognitionStream` and
`SynthesisStream`, following the agent runtime's stream contract: drain the
result channel, then `Wait`; `Cancel` (or cancelling the context) stops the
active transcription, run, or synthesis and joins its goroutine, so an abandoned
turn leaves nothing running.

Voice provider failures are kept distinct from agent failures: a recognition or
synthesis fault is a `*voice.VoiceError` (matchable with `ErrRecognition`,
`ErrSynthesis`, or `ErrUnsupported`), while an agent-run failure keeps its own
`*lebro.AgentError` and is never wrapped, so a caller separates a provider outage
from an agent fault with `errors.As`. The package ships no concrete provider; a
speech backend is an external adapter implementing `Recognizer` and/or
`Synthesizer`.

## Examples

Runnable examples live in [examples](examples/README.md), one directory per
feature set. The schema-validation example validates both tool input and output:

```sh
go run ./examples/schema-validation
```

The storage-memory example exercises the repository contracts and in-memory
adapter:

```sh
go run ./examples/storage-memory
```

The storage-sqlite example stores the same records in a file-backed SQLite
database, closes it, reopens it, and reads the records back:

```sh
go run ./examples/storage-sqlite
```

The model-fixtures example runs a deterministic tool-using model conversation,
stream, failure, and cancellation without making a network request:

```sh
go run ./examples/model-fixtures
```

The tools example registers and safely invokes a local schema-backed handler:

```sh
go run ./examples/tools-schema
```

The model-openai example runs the OpenAI-compatible text-generation adapter
against a recorded HTTP endpoint (no network or API key required):

```sh
go run ./examples/model-openai
```

The structured-output example runs an agent that requests a schema-constrained
JSON result, validates it locally, and decodes it into a typed Go value:

```sh
go run ./examples/structured-output
```

The workflow-agents-tools example combines ordinary Go work, a schema-backed
tool, and an agent in a single linear workflow:

```sh
go run ./examples/workflow-agents-tools
```

The supervised-delegation example runs a supervisor that selects a named
subagent, delegates a focused task to it under independent bounds, and reads
the correlated result:

```sh
go run ./examples/supervised-delegation
```

The agent-network example routes a request to a named specialist and loads its
durable route record:

```sh
go run ./examples/agent-network
```

The streaming example runs a bounded agent against a scripted streaming model
and emits text deltas in real time, then collects the final result:

```sh
go run ./examples/streaming
```

The rag-retrieval example chunks and indexes documents, retrieves them by
semantic query under a metadata filter, and lets an agent use retrieval as an
ordinary tool in the bounded loop:

```sh
go run ./examples/rag-retrieval
```

## Development

```sh
git clone https://github.com/tesh254/lebro.git
cd lebro
go test ./...
go vet ./...
```

Read [CONTRIBUTING.md](CONTRIBUTING.md) before opening a change. Maintainers
release tagged versions using [the release guide](docs/releasing.md).

## License

Apache-2.0. See [LICENSE](LICENSE).
