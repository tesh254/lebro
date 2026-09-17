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
