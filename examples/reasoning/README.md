# Reasoning architecture review

This program sends an architecture-review prompt through OpenRouter with
`ReasoningHigh`, streams displayable reasoning to stderr, streams the answer to
stdout, and persists the assistant turn in a `MemoryStore` thread. A later turn
on the same thread replays opaque provider details unchanged.

Run it with a model that supports OpenRouter reasoning:

```sh
OPENROUTER_API_KEY=... OPENROUTER_MODEL=... go run ./examples/reasoning
```

Reasoning output can contain sensitive intermediate context. The example shows
it only to the local caller; do not forward opaque replay details to browsers,
logs, or untrusted clients.
