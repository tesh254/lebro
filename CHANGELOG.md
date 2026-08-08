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
