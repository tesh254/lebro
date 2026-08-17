package lebro

import (
	"encoding/json"

	"github.com/tesh254/lebro/internal/runtime"
)

func NewProviderRegistry() *ProviderRegistry { return runtime.NewProviderRegistry() }

func NewModelRouter(config ModelRouterConfig) (*ModelRouter, error) {
	return runtime.NewModelRouter(config)
}

func DefaultModelRetryable(err *ModelError) bool { return runtime.DefaultModelRetryable(err) }

func NewModelToolCalls(calls ...ModelToolCall) (ModelToolCalls, error) {
	return runtime.NewModelToolCalls(calls...)
}

func NewModelStructuredOutput(value json.RawMessage) ModelStructuredOutput {
	return runtime.NewModelStructuredOutput(value)
}

func AsStreamingModel(model Model) StreamingModel { return runtime.AsStreamingModel(model) }
