# MCP usage

Use `github.com/tesh254/lebro/mcp` when Lebro is only the MCP boundary. The
root runtime stays optional: an MCP server can expose schema-validated tools
without creating an agent, workflow, HTTP API, or storage adapter.

```sh
go get github.com/tesh254/lebro@v0.1.0
```

## Server over stdio

Create normal Lebro tools, register them once, then explicitly expose only the
tools callers may use. Stdio is suitable for desktop MCP hosts that launch the
server as a child process; write logs to stderr, never stdout.

```go
registry, err := lebro.NewToolRegistry(lebrojsonschema.NewCompiler())
if err != nil { return err }
if err := registry.Register(weatherTool{}); err != nil { return err }

tool, _ := registry.Resolve("weather.lookup")
server := mcp.NewServer(mcp.ServerConfig{
	Implementation: &mcpsdk.Implementation{Name: "weather", Version: "1.0.0"},
})
if err := server.ExposeTool(tool); err != nil { return err }
return server.Run(ctx, &mcpsdk.StdioTransport{})
```

Build and register the repository example:

```sh
go build -o ./bin/lebro-mcp-server ./examples/mcp-server
```

```json
{
  "mcpServers": {
    "lebro-weather": { "command": "/absolute/path/to/bin/lebro-mcp-server" }
  }
}
```

For a multi-session HTTP deployment, keep authentication, tenant policy, rate
limits, and request-size limits in middleware owned by the application:

```go
handler := server.StreamableHTTPHandler(nil)
return http.ListenAndServe(":8080", handler)
```

`StreamableHTTPHandler(nil)` uses a stateless handler with request-cancellation
propagation. Do not expose every registry tool: `ExposeTool`, `ExposeAgent`, and
`ExposeWorkflow` are an allow-list.

### Durable agent and workflow tasks

Use MCP Tasks for an entrypoint that may outlive one HTTP request. The
application owns `TaskStore`: persist the opaque caller identity with every
task, reconstruct a runtime context in `Context`, and authorize every poll or
cancellation in `Authorize`. Do not store bearer credentials in `Identity`.

```go
server := mcp.NewServer(mcp.ServerConfig{
	Implementation: &mcpsdk.Implementation{Name: "jobs", Version: "1.0.0"},
	Tasks: &mcp.TaskConfig{
		Store:        taskStore, // durable application implementation
		TTL:          time.Hour,
		PollInterval: 2 * time.Second,
		Identity: func(ctx context.Context) (json.RawMessage, error) {
			return callerIdentity(ctx) // stable subject/tenant reference only
		},
		Context: func(ctx context.Context, task mcp.TaskRecord) (context.Context, error) {
			return contextForIdentity(ctx, task.Identity)
		},
		Authorize: func(ctx context.Context, task mcp.TaskRecord) error {
			return mayAccessTask(ctx, task)
		},
	},
})
if err := server.ExposeWorkflowAsync(workflow, mcp.AsyncEntryOptions{
	RequireTasks: true,
}); err != nil { return err }
```

The server advertises `io.modelcontextprotocol/tasks`. A client that declares
that extension receives `resultType: "task"`, then uses `tasks/get` and
`tasks/update` (to acknowledge task input when supported), and `tasks/cancel`;
terminal tool output and `isError` results remain available
until TTL expiry. With `RequireTasks`, clients lacking the extension receive
MCP error `-32021`. Optional entries retain synchronous fallback; set
`TaskConfig.SyncTimeout` to bound it.

### Restart recovery

A crash between task creation and completion leaves a durable record in the
`working` state. After the process restarts, expose the async entries again and
call `RecoverTasks` to re-launch execution for those records:

```go
if err := server.RecoverTasks(ctx); err != nil { return err }
```

The `TaskStore` must implement `mcp.WorkingTaskLister` (an optional interface
listing `working` records); otherwise recovery is a no-op. Records whose entry
is not exposed in the restarted process are left until TTL expiry. Concurrent
server instances may recover the same record; the version conflict retry in
the store elects a single terminal result. If completion still cannot be
persisted, `TaskConfig.OnConflict` observes the record so the application can
reconcile its durable run.

## Client for an external server

`mcp.Client` discovers remote tools and adapts each to `lebro.Tool`. Register
those adapters in a normal `ToolRegistry` so JSON arguments and results are
checked before and after the remote call.

```go
client := mcp.NewClient(mcp.ClientConfig{
	Implementation: &mcpsdk.Implementation{Name: "my-client", Version: "1.0.0"},
	ServerName:     "weather",
})
if err := client.Connect(ctx, transport); err != nil { return err }
defer client.Close()

tools, err := client.DiscoverTools(ctx)
if err != nil { return err }
for _, tool := range tools {
	if err := registry.Register(tool); err != nil { return err }
}
```

### Streamable HTTP connections

For remote Streamable HTTP servers, use `ConnectStreamableHTTP`. The client
first probes the 2026-07-28 stateless protocol and the SDK falls back to the
legacy `initialize` negotiation when required. `ConnectionHealth` tells an
application which mode was negotiated without exposing an MCP session ID.

```go
client := mcp.NewClient(mcp.ClientConfig{
	Implementation: &mcpsdk.Implementation{Name: "my-client", Version: "1.0.0"},
	ServerName:     "weather",
})
if err := client.ConnectStreamableHTTP(ctx, mcp.StreamableHTTPConfig{
	Endpoint:    "https://mcp.example.com/mcp",
	IdleTimeout: 5 * time.Minute, // optional bound for a retained legacy session
}); err != nil { return err }
defer client.Close()

health := client.ConnectionHealth()
// health.Mode is "stateless" or "stateful".
tools, err := client.DiscoverTools(ctx)
```

One `Client` owns one connection. Applications must create and partition
clients using their own tenant, principal, connection, and server identity;
an MCP session is never an authorization boundary. Calls through a stateful
client are ordered and retain the SDK-managed session for the related sequence.
`Close` releases it, and `IdleTimeout` can release an unused session
automatically. A 404/missing session returns an error matching
`mcp.ErrRemoteSessionLost`; the failed call is never replayed. Call
`Reconnect` before starting a new independently safe sequence.

Legacy `/sse` plus `/message` servers are not supported by this API. This
client supports Streamable HTTP endpoints only.

Remote IDs are namespaced as `ServerName.remote-name`; for example, remote
`lookup` becomes `weather.lookup`. This prevents collisions between servers.

Use the runnable command-client example against a subprocess:

```sh
go build -o ./bin/lebro-mcp-server ./examples/mcp-server
go run ./examples/mcp-client-command \
  -command ./bin/lebro-mcp-server \
  -server-name lebro \
  -tool weather.lookup \
  -arguments '{"city":"Nairobi"}'
```

The in-memory `examples/mcp-client` remains the deterministic test fixture. It
shows the same discovery and validation path without requiring a second binary.

## Operational limits

Set a deadline on each connect, discovery, and invocation context. Treat remote
tools as untrusted integrations: restrict their allow-list, validate their
schemas, bound arguments/results at the transport boundary, and never let a
remote caller provide thread IDs or authorization metadata. `mcp` prefers
protocol version `2026-07-28` and transparently negotiates its supported legacy
Streamable HTTP fallback; preserve tool IDs and schemas during rolling upgrades.
