// Package openai provides an optional lebro model adapter for OpenAI-compatible
// chat-completions endpoints.
//
// The adapter is intentionally text-only: it maps the provider-neutral
// [github.com/tesh254/lebro.ModelRequest] conversation to a chat-completions
// request and maps a plain-text completion and token usage back to a neutral
// [github.com/tesh254/lebro.ModelResponse]. Tool definitions and structured
// output are rejected here; those capabilities belong to richer adapters built
// on top of the same protocol. OpenAI-specific wire types live only in this
// package so callers depend on the neutral protocol, not a vendor SDK.
//
// Configure the adapter once and reuse it across goroutines; the underlying
// [net/http.Client] is shared and never mutated.
package openai
