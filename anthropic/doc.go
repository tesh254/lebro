// Package anthropic provides a native Anthropic Messages API adapter.
//
// It supports text, client tool calls, streamed text and tool calls, and JSON
// output schemas on Anthropic models that support structured outputs. Configure
// it with an API key; BaseURL and HTTPClient are available for proxies and tests.
// Provider schema restrictions remain enforced by Anthropic, while lebro keeps
// local validation of the returned structured value.
package anthropic
