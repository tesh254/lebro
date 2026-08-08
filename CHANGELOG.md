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
  IDs, monotonic sequence numbers, timestamps, durations, model usage, and
  error summaries. A RunListener interface and RunRecorder collector capture
  events without requiring an observability backend. Injectable Clock and
  IDSource make event streams reproducible across runs with the same fixture.
