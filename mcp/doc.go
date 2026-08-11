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
// This package is a protocol bridge, not a security boundary. When the MCP
// server is reachable by untrusted clients, the application operator is
// responsible for:
//
//   - Access control on thread IDs and workflow run IDs. The handlers forward
//     caller-supplied identifiers directly to Agent.Run and Workflow.Resume.
//     An MCP client that learns another tenant's thread ID or run ID can read
//     or write that tenant's data. Wrap the transport or add middleware to
//     enforce ownership checks before dispatching to lebro.
//   - Error content. Tool handler errors and panic messages are returned to
//     the MCP client via CallToolResult.IsError so the LLM can self-correct.
//     If those errors contain sensitive backend details, sanitize them before
//     exposing the server to untrusted networks.
//
// System messages supplied by MCP clients are filtered out before the agent
// run begins; the agent's configured instructions are the only system prompt.
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
