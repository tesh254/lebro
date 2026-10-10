// Package anthropic provides a native Anthropic Messages API adapter.
//
// It supports text, extended thinking, client tool calls, streamed text and tool calls, and JSON
// output schemas on Anthropic models that support structured outputs. It also
// targets any Anthropic-compatible endpoint, such as OpenRouter's Anthropic API
// or a gateway: set BaseURL, authenticate with APIKey (X-Api-Key) or AuthToken
// (Bearer), and add Headers as the endpoint requires. No ANTHROPIC_*
// environment variable is read.
// Provider schema restrictions remain enforced by Anthropic, while lebro keeps
// local validation of the returned structured value. ReasoningConfig maps to
// Anthropic's thinking-token budget. Thinking blocks, including opaque
// signatures and redacted blocks, are retained exactly for later replay; only
// displayable thinking text is exposed through the neutral message field.
// Unsigned thinking from a compatible provider is display-only and not
// replayed.
package anthropic
