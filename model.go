package lebro

import (
	"context"
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

func NewModelReasoningDetails(value json.RawMessage) ModelReasoningDetails {
	return runtime.NewModelReasoningDetails(value)
}

func NewTextPart(text string) (MessageContentPart, error) { return runtime.NewTextPart(text) }

func NewImagePart(mimeType, base64Data string) (MessageContentPart, error) {
	return runtime.NewImagePart(mimeType, base64Data)
}

func NewDocumentPart(filename, mimeType, base64Data string) (MessageContentPart, error) {
	return runtime.NewDocumentPart(filename, mimeType, base64Data)
}

func NewTextAttachmentPart(filename, mimeType, text string) (MessageContentPart, error) {
	return runtime.NewTextAttachmentPart(filename, mimeType, text)
}

func NewMessageContentParts(parts ...MessageContentPart) (MessageContentParts, error) {
	return runtime.NewMessageContentParts(parts...)
}

func AsStreamingModel(model Model) StreamingModel { return runtime.AsStreamingModel(model) }

func ParseDecimal(value string) (Decimal, error) { return runtime.ParseDecimal(value) }

func UnavailableModelCost(domain PricingDomain, reason CostUnavailableReason) ModelCost {
	return runtime.UnavailableModelCost(domain, reason)
}

func NewOfficialPricingResolver() CostResolver { return runtime.NewOfficialPricingResolver() }

func AggregateModelCosts(ctx context.Context, repo ModelAttemptRepository, filter ModelAttemptFilter) ([]ModelCost, error) {
	return runtime.AggregateModelCosts(ctx, repo, filter)
}
