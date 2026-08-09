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
lebro/                   public API façade and module documentation
internal/runtime/        model, tools, schema, workflow, and storage runtime
jsonschema/              optional JSON Schema compiler implementation
internal/testkit/        deterministic provider fixtures for tests
examples/                runnable feature-focused examples
docs/                    installation and release guides
```

Keep runtime implementation out of module root. Add optional integrations as
their own packages, never as dependencies of the root API.

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

## OpenAI-compatible text-generation adapter

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

`Resume` requires a bound `Store` and a `RunStatusSuspended` run; resuming a
run in any other status returns `ErrNotSuspended`, and resuming without a
store returns `ErrWorkflowResumeRequiresStore`. Run history records
`RunEventSuspended` ("run_suspended") at the suspend boundary and
`RunEventResumed` ("run_resumed") at resume start; neither is terminal. The
suspend snapshot envelope version is `2` and adds the optional `suspend`
field; readers tolerate `0` and `1` as legacy.

Run the suspend/resume example (no network or API key required):

```sh
go run ./examples/workflow-suspend-resume
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

The streaming example runs a bounded agent against a scripted streaming model
and emits text deltas in real time, then collects the final result:

```sh
go run ./examples/streaming
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
