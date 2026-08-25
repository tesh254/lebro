// Package openai provides an optional lebro model adapter for OpenAI-compatible
// chat-completions endpoints.
//
// The adapter maps the provider-neutral
// [github.com/tesh254/lebro.ModelRequest] conversation onto a chat-completions
// request: assistant text, reasoning and replay details, function tool definitions, assistant tool-call turns
// with their tool results, streamed text and streamed tool calls, and
// JSON-schema structured output through response_format. Structured responses
// are accepted in the standard string-content shape and as bare JSON values;
// either way lebro validates the returned value locally against the requested
// schema. Provider-side schema restrictions (for example strict-mode field
// requirements or model support for json_schema output) remain enforced by the
// endpoint and surface as normalized [github.com/tesh254/lebro.ModelError]
// values.
//
// Request-body keys derived from the neutral protocol (model, messages,
// stream, tools, response_format, reasoning, include_reasoning) are owned by the mapping. Opaque
// [github.com/tesh254/lebro.ModelRequest].Extension fields merge into the body
// for everything else, so callers can pass vendor knobs such as temperature,
// max_tokens, seed, or tool_choice without coupling the neutral protocol to a
// vendor.
//
// ReasoningConfig maps to the compatible reasoning object. OpenRouter endpoints
// also receive include_reasoning for enabled reasoning. Response reasoning text,
// reasoning_details, and reasoning token usage map back onto the neutral
// protocol. Opaque reasoning details are replayed unchanged on later assistant
// turns.
//
// OpenAI-specific wire types live only in this package so callers depend on
// the neutral protocol, not a vendor SDK. Configure the adapter once and reuse
// it across goroutines; the underlying [net/http.Client] is shared and never
// mutated.
package openai
