# OpenRouter ACP smoke harness

This example connects a Lebro agent to OpenRouter over ACP stdio. Its session history lives in memory. It exposes `inspect_text` as a small approval-protected tool and accepts client-provided stdio MCP servers through the ACP adapter. It does not provide its own file editing or terminal tools.

Build from the repository root:

```sh
mkdir -p bin
go build -o bin/lebro-acp-openrouter ./examples/acp-openrouter
```

The harness requires `OPENROUTER_API_KEY` and `OPENROUTER_MODEL`. Use a model that supports tool calling, for example `openai/gpt-4o-mini`. `OPENROUTER_BASE_URL` optionally overrides the API root. The harness writes ACP messages to stdout and diagnostics to stderr.

For Zed, add this to the existing `agent_servers` object in Zed's settings. Replace `/path/to/lebro` with the absolute path to your checkout.

```json
"agent_servers": {
  "Lebro OpenRouter": {
    "type": "custom",
    "command": "/bin/sh",
    "args": ["/path/to/lebro/examples/acp-openrouter/zed-launch.sh"],
    "env": {"OPENROUTER_MODEL": "openai/gpt-4o-mini"}
  }
}
```

The launcher runs an interactive zsh so `~/.zshrc` can supply `OPENROUTER_API_KEY`. It exports that variable to the harness and redirects shell startup output to stderr so it cannot corrupt ACP messages. This executes the shell startup files each time Zed launches the agent. Keep the API key out of Zed settings and the repository.

In Zed, open the Agent Panel, click the **+** in its top bar, and choose **Lebro OpenRouter** to start an external-agent thread. The model picker in a Zed Agent thread lists Zed’s configured models. Send `Remember the word cobalt.` Follow with `What word did I ask you to remember?` to check session history. Then send `Use inspect_text to count the words in "hello from Zed".` Review the displayed tool arguments and select **Allow once**. The result should show three words. Requests use your OpenRouter account and chosen model.

Run the local tests with `go test ./examples/acp-openrouter`. They use a local fake provider and do not spend OpenRouter credits.
