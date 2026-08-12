package httpapi

import "github.com/tesh254/lebro"

// Redactor rewrites a stream delta before it is serialized to a client. It is
// applied to every delta on the stream route, including the terminal one.
//
// A Redactor must not retain the delta it is given or the slices reachable from
// it: the runtime owns those and reuses the surrounding buffers after the call
// returns. Return a delta built from values the redactor owns, or the argument
// with fields cleared.
//
// Returning the zero StreamDelta suppresses the delta entirely; it is not sent.
type Redactor func(lebro.StreamDelta) lebro.StreamDelta

// DefaultRedactor removes model-supplied tool-call arguments while passing
// assistant text and structured output through.
//
// Tool-call arguments are the field most likely to carry data a client should
// not see: they are composed by the model from the whole transcript, which can
// include instructions, retrieved documents, and prior tool results. Text and
// structured output are what the caller asked for, so removing them would make
// streaming useless.
//
// This is the default precisely because it is not the empty policy. A nil
// Redactor selects it, so a zero-valued ServerConfig streams less rather than
// more. Pass PassthroughRedactor to opt out deliberately.
func DefaultRedactor(delta lebro.StreamDelta) lebro.StreamDelta {
	if delta.ToolCall == nil {
		return delta
	}
	// Copy the call before clearing: the pointer targets a value the runtime
	// still holds, so mutating it in place would strip arguments from the
	// transcript the run is assembling, not just from the wire.
	redacted := *delta.ToolCall
	redacted.Arguments = nil
	delta.ToolCall = &redacted
	return delta
}

// PassthroughRedactor sends every delta unchanged, including tool-call
// arguments. Use it only when the client is as trusted as the process serving
// it.
func PassthroughRedactor(delta lebro.StreamDelta) lebro.StreamDelta { return delta }
