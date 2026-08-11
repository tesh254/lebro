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

Run the deterministic, network-free model protocol and fixture example:

```sh
go run ./examples/model-fixtures
```

This example imports `internal/testkit`, which is deliberately available only
to this repository's tests and examples. Production packages do not depend on
the fixture harness.

Run the schema-backed local tool execution example:

```sh
go run ./examples/tools-schema
```

Run the OpenAI-compatible text-generation adapter example (no network or API key
required; it targets a recorded HTTP endpoint):

```sh
go run ./examples/model-openai
```

Run the bounded tool-using agent-loop example against deterministic fixtures and
a local schema-backed tool (no network or API key required):

```sh
go run ./examples/agent-loop
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

Run the MCP server example (exposes lebro tools over stdio for any
MCP-compatible client; reads from stdin, so pipe requests or connect a client):

```sh
go run ./examples/mcp-server
```

Run the MCP client example (discovers tools on a remote MCP server and
registers them as validated lebro tools; the server runs in-process over an
in-memory transport, so no network or API key is required):

```sh
go run ./examples/mcp-client
```
