// Package mcp exposes selected lebro tools, agents, and workflows through an
// MCP (Model Context Protocol) server built on the official Go SDK
// (github.com/modelcontextprotocol/go-sdk).
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
// authorization metadata into tool execution. Resume endpoints require
// ServerConfig.AuthorizeWorkflowResume, which must enforce ownership of the
// supplied workflow run ID. Runtime and handler failures are translated to
// stable public error messages rather than exposing internal error details.
//
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
package mcp
