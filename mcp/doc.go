// Package mcp bridges lebro and the Model Context Protocol in both directions,
// built on the official Go SDK (github.com/modelcontextprotocol/go-sdk).
//
// Server exposes selected lebro tools, agents, and workflows to MCP clients.
// Client consumes tools from a remote MCP server and adapts them to the lebro
// Tool contract.
//
// Only explicitly registered primitives are visible to MCP clients. The server
// does not expose a registry by default; every tool, agent, or workflow must be
// individually registered with ExposeTool, ExposeAgent, or ExposeWorkflow.
//
// The server speaks protocol version 2026-07-28, which uses a stateless
// protocol core: no initialize handshake is required, each request is
// self-describing, and any request can land on any server instance.
//
// # Security considerations
//
// MCP client arguments cannot set thread IDs or runtime metadata, preventing
// them from selecting another caller's durable conversation or injecting
// authorization metadata into tool execution. Workflow resume is intentionally
// not exposed until lebro provides a durable atomic resume claim. Runtime and
// handler failures are translated to stable public error messages rather than
// exposing internal error details. Keep agent and workflow integrations
// panic-free, or recover uncaught panics at the application boundary.
// MCP agent inputs accept only user text; the agent's configured instructions
// are the only system prompt.
//
// # Quick start
//
// Create a server, expose a tool, and run over stdio:
//
//	server := mcp.NewServer(mcp.ServerConfig{
//	    Implementation: &mcpsdk.Implementation{Name: "my-app", Version: "1.0.0"},
//	})
//	_ = server.ExposeTool(registeredTool)
//	_ = server.Run(context.Background(), &mcpsdk.StdioTransport{})
//
// Agents and workflows are exposed as MCP tools so clients can list and invoke
// them through the standard tools/list and tools/call methods.
//
// # Consuming a remote MCP server
//
// Client discovers tools on a remote server and adapts them to lebro.Tool, so
// they register in a ToolRegistry alongside local tools and gain the same
// schema-checked execution boundary:
//
//	client := mcp.NewClient(mcp.ClientConfig{
//	    Implementation: &mcpsdk.Implementation{Name: "my-app", Version: "1.0.0"},
//	    ServerName:     "weather",
//	})
//	_ = client.Connect(ctx, transport)
//	defer client.Close()
//
//	tools, _ := client.DiscoverTools(ctx)
//	for _, tool := range tools {
//	    _ = registry.Register(tool)
//	}
//
// Discovered tools are named "<ServerName>.<remote name>", so two servers that
// both advertise a "search" tool stay distinguishable in one registry.
//
// Arguments are validated locally before they reach the wire, and every
// adapted tool declares an output schema so its results are validated too. A
// server that advertises its own output schema keeps it. One that does not
// gets a fixed text envelope, {"text": ..., "skipped_content_types": [...]},
// naming any content with no textual form rather than dropping it silently.
// Both fields are always present, so callers never branch on which shape a
// particular response took.
//
// Output shape is therefore a property of the tool definition, not of an
// individual response: a tool always returns the same shape, so its advertised
// schema stays meaningful across calls.
//
// # Distinguishing remote failures
//
// Discovery failures wrap ErrRemoteDiscovery and occur before a tool is
// registered, so they never reach a run record. Failures during a call are
// recorded as ToolExecutionHandlerError and separate into two classes that
// errors.Is tells apart: ErrRemoteInvocation for transport and protocol
// failures, where no tool result was produced, and ErrRemoteToolError for a
// tool that ran and reported failure. Cancellation is reported as
// ToolExecutionCancelled rather than either error class.
//
// ErrRemoteInputRequired covers a server that asks for further input before it
// can answer. How such a call resolves depends on ClientConfig.Options: set an
// elicitation or sampling handler there and the SDK fulfills the request and
// retries, so only the final result reaches the adapter. Without a handler the
// request cannot be satisfied, and the call is reported rather than mistaken
// for an empty success.
package mcp
