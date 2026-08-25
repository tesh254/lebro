# Wire formats

`httpapi` owns Lebro's HTTP wire contract. The generated OpenAPI 3.1 document
at `GET /openapi.json` is normative: routes, request/response schemas, public
error codes, and exposed agent/workflow definitions come from the same route
table as the server.

## HTTP

- `GET /health`, `/agents`, and `/workflows` return JSON.
- `POST /agents/{id}/runs` and `POST /workflows/{id}/runs` accept JSON and
  return JSON. Agent requests accept only user message content; configured
  instructions never cross this boundary.
- `POST /agents/{id}/runs/stream` returns `text/event-stream`. Each `data:`
  value is a JSON `StreamEvent`; event names are `model_delta` then exactly one
  of `run_succeeded`, `run_failed`, or `run_cancelled`.
- `POST /agents/{id}/runs/ai-sdk/stream?version=v4|v5` uses the explicitly
  selected AI SDK protocol. `v4` is legacy data-stream framing; `v5` is UI
  message SSE framing. This is distinct from native `StreamEvent` SSE.

`thread_id` is a query parameter, not request JSON. It selects a durable
conversation and requires the server to have a store. Percent-encode agent and
workflow IDs as path segments. Closing a stream cancels its run.

Agent-run requests may include an optional `reasoning` object with either
`effort` (`off`, `minimal`, `low`, `medium`, `high`, `xhigh`, or `max`) or a
non-zero `budget_tokens`. The response, thread-message list, and native stream
events contain displayable `reasoning` text only when the configured redactor
permits it. Usage adds `reasoning_tokens`. Opaque provider replay metadata is
deliberately excluded from all HTTP and AI SDK streams, even when the server
persists it in its thread transcript. The default redactor suppresses reasoning
to avoid exposing raw provider chain-of-thought; use a deliberate trusted-client
policy to expose it.

The contract version is OpenAPI's `x-lebro-contract-version`. Use the typed
client's `CheckCompatibility` before an independently deployed client sends
production traffic; different major versions must not interoperate.

## MCP

`mcp` speaks protocol version `2026-07-28`. Servers expose no capability until
an application explicitly registers a tool, agent, or workflow. Remote tool
arguments are validated locally, and adapted results always follow the declared
output schema. MCP callers cannot set durable thread IDs or runtime metadata.

Tool IDs, input schemas, output schemas, and public HTTP error codes are wire
contracts. Add optional fields; do not change required fields, meanings, or
error-code classification without versioning and migration notes.
