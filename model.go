package lebro

import "context"

// ModelRequest is the neutral input sent to a language-model provider.
type ModelRequest struct {
	Messages []Message
	Model    string
}

// ModelUsage records provider-reported token usage when available.
type ModelUsage struct {
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
}

// ModelResponse is the neutral output returned from a language-model provider.
type ModelResponse struct {
	Message Message
	Usage   ModelUsage
}

// Model is implemented by language-model adapters. Implementations must honor
// context cancellation and preserve the Message contract.
type Model interface {
	Generate(context.Context, ModelRequest) (ModelResponse, error)
}
