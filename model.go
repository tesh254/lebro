package lebro

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ModelRequest is the provider-neutral input sent to a language-model adapter.
// Extension is opaque provider-specific JSON; the runtime carries it without
// interpreting it.
type ModelRequest struct {
	Messages     []Message
	Model        string
	Tools        []ToolDefinition
	OutputSchema *ModelOutputSchema
	Extension    json.RawMessage
}

// ModelOutputSchema requests a final JSON value that conforms to Schema. Name
// and Description are provider-facing hints and do not change local validation.
type ModelOutputSchema struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Strict      bool
}

// ModelToolCall is one tool invocation requested by a model.
type ModelToolCall struct {
	ID        string          `json:"id"`
	ToolID    ToolID          `json:"tool_id"`
	Arguments json.RawMessage `json:"arguments"`
}

// ModelToolCalls is an immutable, canonical encoding of ordered tool calls.
// Its opaque representation keeps Message comparable and ensures values can
// only be created through validation or JSON decoding.
type ModelToolCalls struct {
	encoded string
}

// NewModelToolCalls validates and canonically encodes tool calls.
func NewModelToolCalls(calls ...ModelToolCall) (ModelToolCalls, error) {
	if len(calls) == 0 {
		return ModelToolCalls{}, nil
	}
	seen := make(map[string]struct{}, len(calls))
	for i, call := range calls {
		if err := call.Validate(); err != nil {
			return ModelToolCalls{}, fmt.Errorf("lebro: model tool call %d: %w", i, err)
		}
		if _, exists := seen[call.ID]; exists {
			return ModelToolCalls{}, fmt.Errorf("lebro: duplicate model tool call ID %q", call.ID)
		}
		seen[call.ID] = struct{}{}
	}
	// Validation guarantees RawMessage values are valid, so encoding cannot fail.
	encoded, _ := json.Marshal(calls)
	return ModelToolCalls{encoded: string(encoded)}, nil
}

// Values decodes ordered tool calls into a caller-owned slice.
func (c ModelToolCalls) Values() []ModelToolCall {
	if c.IsZero() {
		return nil
	}
	var calls []ModelToolCall
	// encoded is private and every construction path validates it first.
	_ = json.Unmarshal([]byte(c.encoded), &calls)
	return calls
}

// IsZero reports whether the collection contains no calls.
func (c ModelToolCalls) IsZero() bool { return c.encoded == "" }

// MarshalJSON writes tool calls as an array rather than a quoted string.
func (c ModelToolCalls) MarshalJSON() ([]byte, error) {
	if c.IsZero() {
		return []byte("[]"), nil
	}
	return []byte(c.encoded), nil
}

// UnmarshalJSON validates and canonicalizes an array of tool calls.
func (c *ModelToolCalls) UnmarshalJSON(data []byte) error {
	if c == nil {
		return errors.New("lebro: unmarshal model tool calls into nil receiver")
	}
	if string(data) == "null" {
		*c = ModelToolCalls{}
		return nil
	}
	var calls []ModelToolCall
	if err := json.Unmarshal(data, &calls); err != nil {
		return fmt.Errorf("lebro: decode model tool calls: %w", err)
	}
	encoded, err := NewModelToolCalls(calls...)
	if err != nil {
		return err
	}
	*c = encoded
	return nil
}

// ModelStructuredOutput stores raw JSON as a comparable transcript value.
type ModelStructuredOutput string

// NewModelStructuredOutput copies raw JSON into a transcript value.
func NewModelStructuredOutput(value json.RawMessage) ModelStructuredOutput {
	var compact bytes.Buffer
	if json.Compact(&compact, value) == nil {
		return ModelStructuredOutput(compact.String())
	}
	return ModelStructuredOutput(string(value))
}

// Raw returns a caller-owned JSON value.
func (o ModelStructuredOutput) Raw() json.RawMessage {
	return json.RawMessage(string(o))
}

// MarshalJSON writes the structured value as raw JSON.
func (o ModelStructuredOutput) MarshalJSON() ([]byte, error) {
	if o == "" {
		return []byte("null"), nil
	}
	if !json.Valid([]byte(o)) {
		return nil, errors.New("lebro: model structured output must be valid JSON")
	}
	return []byte(o), nil
}

// UnmarshalJSON retains the raw structured value.
func (o *ModelStructuredOutput) UnmarshalJSON(data []byte) error {
	if o == nil {
		return errors.New("lebro: unmarshal model structured output into nil receiver")
	}
	if !json.Valid(data) {
		return errors.New("lebro: model structured output must be valid JSON")
	}
	*o = NewModelStructuredOutput(data)
	return nil
}

// ModelUsage records provider-reported token usage when available.
type ModelUsage struct {
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
}

// FinishReason describes why a model stopped producing a response.
type FinishReason string

const (
	FinishReasonStop        FinishReason = "stop"
	FinishReasonLength      FinishReason = "length"
	FinishReasonToolCalls   FinishReason = "tool_calls"
	FinishReasonContent     FinishReason = "content_filter"
	FinishReasonCancelled   FinishReason = "cancelled"
	FinishReasonUnspecified FinishReason = "unspecified"
)

// ModelResponse is the provider-neutral output returned from a model adapter.
// A response can contain assistant text, requested tool calls, or structured
// JSON. Extension preserves opaque provider metadata without coupling callers
// to a vendor SDK.
type ModelResponse struct {
	Message      Message
	Usage        ModelUsage
	FinishReason FinishReason
	Extension    json.RawMessage
}

// Model is implemented by language-model adapters. Implementations must honor
// context cancellation, validate provider responses, and map failures to
// ModelError.
type Model interface {
	Generate(context.Context, ModelRequest) (ModelResponse, error)
}

// Validate checks the provider-neutral invariants adapters can rely on.
func (r ModelRequest) Validate() error {
	for i, message := range r.Messages {
		if err := message.Validate(); err != nil {
			return fmt.Errorf("lebro: model request message %d: %w", i, err)
		}
	}

	seenTools := make(map[ToolID]struct{}, len(r.Tools))
	for i, tool := range r.Tools {
		if tool.ID == "" {
			return fmt.Errorf("lebro: model request tool %d requires an ID", i)
		}
		if _, exists := seenTools[tool.ID]; exists {
			return fmt.Errorf("lebro: model request contains duplicate tool ID %q", tool.ID)
		}
		seenTools[tool.ID] = struct{}{}
		if err := validateOptionalJSON("input schema", tool.InputSchema); err != nil {
			return fmt.Errorf("lebro: model request tool %q: %w", tool.ID, err)
		}
		if err := validateOptionalJSON("output schema", tool.OutputSchema); err != nil {
			return fmt.Errorf("lebro: model request tool %q: %w", tool.ID, err)
		}
	}

	if r.OutputSchema != nil {
		if len(r.OutputSchema.Schema) == 0 {
			return errors.New("lebro: model output schema must not be empty")
		}
		if !json.Valid(r.OutputSchema.Schema) {
			return errors.New("lebro: model output schema must be valid JSON")
		}
	}
	return validateOptionalJSON("model request extension", r.Extension)
}

// Validate checks that a provider response can be consumed consistently by an
// agent runtime.
func (r ModelResponse) Validate() error {
	if err := r.Message.Validate(); err != nil {
		return fmt.Errorf("lebro: model response: %w", err)
	}
	if r.Message.Role != RoleAssistant {
		return fmt.Errorf("lebro: model response message role must be %q", RoleAssistant)
	}
	if !validFinishReason(r.FinishReason) {
		return fmt.Errorf("lebro: invalid model finish reason %q", r.FinishReason)
	}
	if r.Usage.InputTokens < 0 || r.Usage.OutputTokens < 0 || r.Usage.TotalTokens < 0 {
		return errors.New("lebro: model usage must not contain negative token counts")
	}

	toolCalls := r.Message.ToolCalls.Values()
	if len(toolCalls) > 0 && r.FinishReason != FinishReasonToolCalls {
		return errors.New("lebro: model response with tool calls requires tool_calls finish reason")
	}
	if r.FinishReason == FinishReasonToolCalls && len(toolCalls) == 0 {
		return errors.New("lebro: tool_calls finish reason requires at least one tool call")
	}
	return validateOptionalJSON("model response extension", r.Extension)
}

// Validate checks the required identity and JSON arguments of a tool call.
func (c ModelToolCall) Validate() error {
	if c.ID == "" {
		return errors.New("tool call requires an ID")
	}
	if c.ToolID == "" {
		return errors.New("tool call requires a tool ID")
	}
	if len(c.Arguments) == 0 || !json.Valid(c.Arguments) {
		return errors.New("tool call arguments must be valid JSON")
	}
	return nil
}

func validateOptionalJSON(name string, value json.RawMessage) error {
	if len(value) > 0 && !json.Valid(value) {
		return fmt.Errorf("%s must be valid JSON", name)
	}
	return nil
}

func validFinishReason(reason FinishReason) bool {
	switch reason {
	case FinishReasonStop, FinishReasonLength, FinishReasonToolCalls, FinishReasonContent,
		FinishReasonCancelled, FinishReasonUnspecified:
		return true
	default:
		return false
	}
}
