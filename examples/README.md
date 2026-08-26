# Examples

Each directory is a standalone, runnable program for one `lebro` feature set.
Keep examples small, provider-neutral, and focused on one public API or
repository test harness so they act as executable documentation while the
library grows.

Run the JSON Schema validation example from the repository root:

```sh
go run ./examples/schema-validation
```

Run the in-memory storage example:

```sh
go run ./examples/storage-memory
```

Run the durable run-timeline example, which persists model attempts, tool
executions, and run events for one agent run and then queries them back:

```sh
go run ./examples/run-timeline
```

Run the deterministic, network-free model protocol and fixture example:

```sh
go run ./examples/model-fixtures
```

Every example is self-contained: scripted models and other stand-ins live
in-file, so a build can be copied out of this repository and compiled against
the public module alone. Production packages do not depend on any example
helper.

Run the schema-backed local tool execution example:

```sh
go run ./examples/tools-schema
```

Run the OpenAI-compatible text-generation adapter example (no network or API key
required; it targets a recorded HTTP endpoint):

```sh
go run ./examples/model-openai
```

Run the production-shaped OpenRouter reasoning example (requires credentials):

```sh
OPENROUTER_API_KEY=... OPENROUTER_MODEL=... go run ./examples/reasoning
```

Run the bounded tool-using agent-loop example against deterministic fixtures and
a local schema-backed tool (no network or API key required):

```sh
go run ./examples/agent-loop
```

Run the supervised delegation example, with deterministic specialist routing
and a configured fallback (no network or API key required):

```sh
go run ./examples/supervised-delegation
```

Run the tenant policy example (a shared agent allows one tenant and rejects
another before any model call; no network or API key required):

```sh
go run ./examples/tenant-policy
```

Run the bounded agent-network example (routes to a named specialist and reads
durable route records; no network or API key required):

```sh
go run ./examples/agent-network
```

Run the schema-constrained structured-output example (agent requests a final
JSON value that conforms to a caller-supplied schema, validated locally; no
network or API key required):

```sh
go run ./examples/structured-output
```

Run the typed linear-workflow example (two-step workflow with schema-backed
handoffs; no network or API key required):

```sh
go run ./examples/workflow-linear
```

Run the workflow agent-and-tool steps example (ordinary Go work, a
schema-backed tool, and an agent in one workflow; no network or API key
required):

```sh
go run ./examples/workflow-agents-tools
```

Run the token and event streaming example (bounded agent against a scripted
streaming model; no network or API key required):

```sh
go run ./examples/streaming
```

As features are added, create sibling directories such as `model-*`,
`tools-*`, or `workflow-*` rather than extending an unrelated example.

Run the bounded parallel fan-out and join example (two concurrent branches
with deterministic joined output; no network or API key required):

```sh
go run ./examples/workflow-fanout-join
```

Run the vector search example (in-memory vector store with metadata-filtered
similarity search; no network or API key required):

```sh
go run ./examples/vector-search
```

Run recursive and sliding-window chunking examples (no network or API key is
required):

```sh
go run ./examples/rag-chunkers
```

Run Qdrant vector search after starting a local Qdrant gRPC server on port
6334:

```sh
docker run --rm -p 6334:6334 qdrant/qdrant
go run ./examples/vector-qdrant
```

Run the MCP server example (exposes lebro tools over stdio for any
MCP-compatible client; reads from stdin, so pipe requests or connect a client):

```sh
go run ./examples/mcp-server
```

Build that server, then run the command-client example to discover and invoke
it as an external MCP process:

```sh
go build -o ./bin/lebro-mcp-server ./examples/mcp-server
go run ./examples/mcp-client-command \
  -command ./bin/lebro-mcp-server \
  -server-name lebro \
  -tool weather.lookup \
  -arguments '{"city":"Nairobi"}'
```

Run the MCP client example (discovers tools on a remote MCP server and
registers them as validated lebro tools; the server runs in-process over an
in-memory transport, so no network or API key is required):

```sh
go run ./examples/mcp-client
```

Run the HTTP server example (serves an agent, a workflow, and durable threads
over HTTP, streams a run as Server-Sent Events, and prints the generated
OpenAPI contract; the server runs in-process through `httptest` on an ephemeral
loopback port, so no fixed port and no API key are required):

```sh
go run ./examples/http-server
```

Run the HTTP client example (calls that API with the typed client: a complete
run, a streamed run the caller cancels mid-flight, a workflow round trip, typed
error handling, and the contract-version handshake; the API is served
in-process through `httptest`, so no network or API key is required):

```sh
go run ./examples/http-client
```

Run the studio example (exposes an agent and a workflow on a local Studio,
runs them, and inspects the ordered events a run records; the Studio is served
in-process through `httptest`, so no network or API key is required):

```sh
go run ./examples/studio
```

Run the supervised delegation example (a supervisor agent selects a named
subagent and delegates a focused task to it; no network or API key required):

```sh
go run ./examples/supervised-delegation
```

Run the RAG retrieval example (chunks and indexes documents, retrieves by
semantic query, and lets an agent use retrieval as an ordinary tool; uses a
deterministic local embedder, so no network or API key is required):

```sh
go run ./examples/rag-retrieval
```

Run the evaluation example (runs a versioned dataset against a target, scores
each case with rule and model-graded scorers, and compares two experiment runs
of the same dataset version; the target and grader are deterministic local
stand-ins, so no network or API key is required):

```sh
go run ./examples/evals-dataset
```

Run the durable schedule example (persists a recurring schedule, reopens the
store to simulate a restart, ticks a fresh scheduler so the overdue schedule
fires, and prints the execution history; a fixed clock keeps it deterministic,
so no network or API key is required):

```sh
go run ./examples/workflow-schedule
```

Run the channels example (receives a signed platform webhook through the
generic HMAC channel adapter, runs an agent, streams the reply back to the
conversation, and shows the exchange persisted to one durable thread; the
webhook is signed and served in-process, so no network or API key is required):

```sh
go run ./examples/channels
```

Run the voice example (transcribes an audio utterance to a user turn through a
fake recognizer, runs an agent, and synthesizes the reply back to audio through
a fake synthesizer; the fakes stand in for optional provider adapters, so no
network, API key, or speech backend is required):

```sh
go run ./examples/voice
```

## Real-world starting points

Complete, product-shaped builds — each a standalone directory with its own
README and documented in [`docs/product-builds.md`](../docs/product-builds.md):

| Build | Directory |
| --- | --- |
| Docs support agent | `docs-support-agent` |
| Extraction service | `document-extraction` |
| Refund approver | `refund-approval` |
| Morning digest | `morning-digest` |
| Helpdesk front desk | `helpdesk-router` |
| Multi-tenant API | `multitenant-platform` |
| Voice booking line | `voice-booking` |
| CI-gated releases | `ci-gated-releases` |

The single-feature examples behind each build:

| Build | Start with |
| --- | --- |
| Docs support agent | `rag-retrieval`, `thread-history`, `channels` |
| Extraction service | `structured-output`, `http-server` |
| Refund approver | `workflow-suspend-resume` |
| Morning digest | `workflow-schedule`, `workflow-fanout-join` |
| Helpdesk front desk | `agent-network` |
| Multi-tenant API | `tenant-policy`, `request-resolvers`, `http-client` |
| Voice booking line | `voice`, `thread-history` |
| CI-gated releases | `evals-dataset`, `studio` |

Each example is deliberately deterministic unless its directory says otherwise.
Every directory is an independently runnable Go command in this repository's
module; keep it that way rather than adding nested modules, so examples run in
CI with the same dependency and API version users receive. To copy an example
into another project, create that project's module, `go get` Lebro, then retain
only the imports used by the command.
For production, set deadlines, bound agent steps and tool work, authenticate at
the service boundary, scope identities and tenants through `WithIdentity`, and
record provider usage for budget enforcement. See `docs/stability.md`,
`docs/migrations.md`, and `docs/wire-format.md` before deploying durable or
network-facing work.
