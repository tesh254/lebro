// Package acp exposes configured lebro agents to ACP-compatible editors over
// the Agent Client Protocol v1, built on github.com/coder/acp-go-sdk.
//
// The adapter is optional, and the root package never imports it.
//
// # What is supported
//
// The server implements ACP v1 initialization with protocol version
// and capability negotiation, session/new, session/prompt, session/cancel, and
// session/update notifications. Prompt content accepts text and resource_link
// blocks — the v1 baseline every agent must support. resource_link blocks are
// passed to the agent as descriptive text; the server never reads the
// referenced URI, so a link grants no file or network access by itself.
//
// Agent output streams through ACP updates in event order: agent text as
// agent_message_chunk, reasoning as agent_thought_chunk, tool calls as
// tool_call/tool_call_update with stable IDs
// derived from the model's tool-call IDs, and terminal outcomes as stop
// reasons: end_turn, refusal, and cancelled.
//
// # What is deliberately not advertised
//
// The server advertises no optional capabilities: loadSession is false, and no
// image, audio, embeddedContext, filesystem, terminal, MCP-transport, or
// session-resume capabilities are sent. Clients must not send image, audio, or
// resource blocks; they are rejected with an invalid-params error. Stdio MCP
// servers are started for the requesting session. HTTP and SSE MCP transports
// and additionalDirectories are rejected.
//
// # Sessions map to lebro threads
//
// Each session owns an opaque ACP session ID backed by a lebro ThreadID.
// Prompts run with RunInput.ThreadID set, so a store-backed agent loads prior
// turns and appends new messages, preserving context across turns within the
// process. Sessions live only for the connection: there is no session/load,
// and restarting the process starts fresh sessions (new session IDs; existing
// thread history remains in the store and is reachable by application code).
//
// One turn runs per session at a time. An overlapping session/prompt returns
// an invalid-params error; a client that wants a new turn cancels first.
//
// # Approval bridging
//
// When the agent's ToolApprovalPolicy returns require_approval, the server
// translates the durable lebro approval request into an ACP
// session/request_permission round trip. Selecting the allow option resumes
// the exact persisted tool call through Agent.ResumeToolApproval — arguments
// and identity come from the durable request, never from the protocol
// message. Every other path — reject options, cancelled outcomes, unknown
// options, cancellation, disconnect, or expiry — resumes with Approved=false
// or abandons the run, so no unapproved handler executes. Expired requests
// fail through the runtime's own expiry check.
//
// # Security considerations
//
// Everything arriving over the protocol is untrusted. Session IDs select only
// sessions this connection created; unknown IDs return resource-not-found.
// Client-provided MCP commands execute locally in the requested working
// directory with the supplied environment. Connect only trusted ACP clients.
// Discovered tools are scoped to their session and follow the configured
// tool approval policy. Approval decisions carry only an
// outcome; they cannot rewrite the reviewed request.
//
// The adapter uses github.com/coder/acp-go-sdk for ACP framing and types.
//
// # Quick start
//
// Build an agent, expose it, and serve stdio:
//
//	server, err := acp.NewServer(acp.ServerConfig{AgentConfig: &cfg})
//	if err := server.ExposeAgent(); err != nil { ... }
//	if err := server.RunStdio(ctx); err != nil { ... }
package acp
