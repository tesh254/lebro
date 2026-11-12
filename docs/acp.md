# ACP agent adapter

The `acp` package exposes a configured Lebro agent over ACP v1 stdio. Build the network-free example with `go build -o /tmp/lebro-acp ./examples/acp-agent` and configure an ACP-compatible editor to launch `/tmp/lebro-acp` with no arguments. The example uses a fixture model and an in-memory store. Its first model call requests a simulated refund and asks the editor for permission.

Create a `Server` with an `AgentConfig` containing a store, then call `ExposeAgent` and `RunStdio`. The adapter installs its own listener and approval policy wrapper, so the supplied config cannot set a listener or tool observer. The existing approval policy still determines whether each call is allowed, denied, or requires review. With no policy, every tool call requires approval.

Sessions use separate Lebro thread IDs and remain available for the lifetime of the process. Text and resource links are supported. Resource links are passed as text and never read by the adapter. Image, audio, embedded resources, client MCP servers, additional directories, and session loading are unsupported. The working directory does not grant file access. Stdout carries protocol traffic, while diagnostics use stderr.

For protected calls, the adapter presents the persisted tool identity and arguments to the client. Only the allow-once response approves the persisted request. The runtime controls expiry, idempotency, and execution. Tool arguments can contain sensitive data and are shared with the connected client for review. Streamed model reasoning text is also shared with the client. Configure the agent and model accordingly.

The adapter uses `github.com/coder/acp-go-sdk` v0.13.5, which provides Go protocol types and stdio JSON-RPC transport under Apache-2.0. The selection uses the existing SDK rather than implementing framing in Lebro. Other Go SDKs include `github.com/caelis-labs/acp-go-sdk` and `github.com/ironpark/acp-go`; their suitability and maintenance have not been assessed in depth. The selected module's own `go.mod` lists no dependencies. Sources are https://github.com/coder/acp-go-sdk, https://github.com/caelis-labs/acp-go-sdk and https://github.com/ironpark/acp-go.

The focused Go tests include an example subprocess smoke test. An ACP compatibility test kit and real editor smoke test have not yet been run.

## Remaining work for MAD-155

- Verify ordering of resumed assistant text against subsequent tool events. The adapter currently emits resumed transcript text after `ResumeToolApproval` returns. Implement ordered delivery during continuation if needed, and cover multiple approvals and interleaved text/tool activity.
- Run the ACP compatibility test kit and record its results. Run and document a real-editor smoke test covering two turns and approval.
- Complete the SDK evaluation, including protocol coverage, maintenance, license, and dependency footprint of alternatives.
- Review the adapter against every ticket acceptance criterion, including cancellation and disconnect cleanup, malformed input, approval expiry and duplicate decisions, session isolation, security boundaries, and provider/tool errors. Add focused regression coverage for gaps.
- Add release notes and complete the mandatory full-diff review and remediation. Rerun focused and broader tests, race checks, and `golangci-lint run --fix` after the remaining changes.

The ACP tests and lint passed during this work. The full Go suite passed before the final adapter changes, and the ACP race suite passed before the final example change. These results do not establish completion of the remaining checks above.
