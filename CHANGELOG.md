# Changelog

All notable changes to this project are documented in this file.

## Unreleased

### Added

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
