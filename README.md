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
                _ = json.Unmarshal(input, &n)
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
                _ = json.Unmarshal(input, &n)
                return json.Marshal(n + 1)
            }),
        },
    },
})

result, err := wf.Run(context.Background(), lebro.WorkflowRunInput{
    Input: json.RawMessage(`5`),
})

var final int
_ = result.DecodeOutput(&final) // 11
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

## Examples

Runnable examples live in [examples](examples/README.md), one directory per
feature set. The schema-validation example validates both tool input and output:

```sh
go run ./examples/schema-validation
```

The storage example exercises the repository contracts and in-memory adapter:

```sh
go run ./examples/storage-memory
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
