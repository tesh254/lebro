# ACP agent adapter

The `acp` package exposes a configured Lebro agent over ACP v1 stdio. Build the network-free example with `go build -o /tmp/lebro-acp ./examples/acp-agent` and configure an ACP-compatible editor to launch `/tmp/lebro-acp` with no arguments. The example uses a fixture model and an in-memory store. Its first model call requests a simulated refund and asks the editor for permission.

Create a `Server` with an `AgentConfig` containing a store, then call `ExposeAgent` and `RunStdio`. The adapter installs its own listener and approval policy wrapper, so the supplied config cannot set a listener or tool observer. The existing approval policy still determines whether each call is allowed, denied, or requires review. With no policy, every tool call requires approval.

Sessions use separate Lebro thread IDs and remain available while the ACP connection is open; disconnecting clears the session mappings. Text and resource links are supported. Resource links are passed as text and never read by the adapter. Client-supplied stdio MCP servers are launched for their ACP session, using the requested working directory, the launcher’s PATH and HOME, and explicitly supplied environment variables. Executable names are resolved on the launcher’s PATH. Their tools are namespaced by server name, validated, and subject to the configured tool approval policy. Closing the ACP connection closes the session-owned servers. Image, audio, embedded resources, HTTP and SSE MCP transports, additional directories, and session loading are unsupported. Stdout carries protocol traffic, while diagnostics use stderr.

For protected calls, the adapter presents the persisted tool identity and arguments to the client. Only the allow-once response approves the persisted request. The runtime controls expiry, idempotency, and execution. Tool arguments can contain sensitive data and are shared with the connected client for review. Streamed model reasoning text is also shared with the client. Configure the agent and model accordingly.

## SDK selection

The adapter uses `github.com/coder/acp-go-sdk` v0.13.5. Its generated types and connection support the methods used here, including session updates, permissions, cancellation and stdio. Its pinned module has no external dependencies and uses Apache-2.0. The upstream repository shows ongoing development and releases. See https://github.com/coder/acp-go-sdk.

The Go alternatives reviewed in November 2026 include [ironpark/acp-go](https://github.com/ironpark/acp-go), which offers stdio and network transports under MIT and whose current module requires Go 1.27 and `github.com/coder/websocket`, and [caelis-labs/acp-go-sdk](https://github.com/caelis-labs/acp-go-sdk), which offers a stdio server under Apache-2.0 and requires `golang.org/x/sys`. Both repositories showed recent activity. Their complete protocol coverage and cancellation behavior have not been independently verified. Lebro currently targets Go 1.26 and retains the Coder SDK, already integrated and exercised by the adapter tests.

## Verification and remaining work

On 13 November 2026 the experimental ACP TCK at commit `13c4f7aa2fcaaec32f194e502031717bf28ec700` ran against the example. Its report recorded 31 mandatory passes and one mandatory failure, 17 advisory passes and no advisory failures. A subsequent run after adding stdio MCP support passed all 32 mandatory requirements and 19 advisory checks. The final run passed all 32 mandatory checks, including cancellation. Thirty capability-dependent checks and seven advisory checks were skipped. The Go test suite separately exercises cancellation. The TCK is experimental and its result is not an official certification or complete conformance assessment. See https://github.com/agentclientprotocol/acp-tck.

The OpenRouter example was exercised through Zed during development, and a live ACP check with the Shopify MCP server completed a prompt and an approved tool call. The user reported the Zed setup working. An independently recorded Zed smoke test covering two turns and approval remains outstanding.
