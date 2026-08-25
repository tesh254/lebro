package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ModelRequest is the provider-neutral input sent to a language-model adapter.
// Extension is opaque provider-specific JSON; the runtime carries it without
// interpreting it.
type ModelRequest struct {
	Messages     []Message
	Model        string
	Tools        []ToolDefinition
	OutputSchema *ModelOutputSchema
	Reasoning    ReasoningConfig
	Extension    json.RawMessage
}

// ReasoningEffort controls how much internal reasoning a reasoning-capable
// model should use. Providers support different subsets; adapters reject a
// value they cannot represent instead of silently lowering it.
type ReasoningEffort string

const (
	ReasoningOff     ReasoningEffort = "off"
	ReasoningMinimal ReasoningEffort = "minimal"
	ReasoningLow     ReasoningEffort = "low"
	ReasoningMedium  ReasoningEffort = "medium"
	ReasoningHigh    ReasoningEffort = "high"
	ReasoningXHigh   ReasoningEffort = "xhigh"
	ReasoningMax     ReasoningEffort = "max"
)

// ReasoningConfig is provider-neutral reasoning intent. Effort and
// BudgetTokens are mutually exclusive: effort selects a provider-supported
// tier, while BudgetTokens requests an exact maximum when a provider supports
// token-budget reasoning.
type ReasoningConfig struct {
	Effort       ReasoningEffort `json:"effort,omitempty"`
	BudgetTokens int64           `json:"budget_tokens,omitempty"`
}

// IsZero reports whether no reasoning preference was requested.
func (r ReasoningConfig) IsZero() bool { return r.Effort == "" && r.BudgetTokens == 0 }

// Validate checks that the neutral configuration is well formed. Adapters
// perform provider-specific capability validation.
func (r ReasoningConfig) Validate() error {
	if r.BudgetTokens < 0 {
		return errors.New("lebro: reasoning budget tokens must not be negative")
	}
	if r.Effort != "" && r.BudgetTokens != 0 {
		return errors.New("lebro: reasoning effort and budget tokens are mutually exclusive")
	}
	switch r.Effort {
	case "", ReasoningOff, ReasoningMinimal, ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMax:
		return nil
	default:
		return fmt.Errorf("lebro: unsupported reasoning effort %q", r.Effort)
	}
}

// ModelReasoningDetails stores opaque, provider-issued reasoning metadata
// needed to faithfully replay a prior assistant turn. For example, this holds
// OpenRouter reasoning_details or Anthropic thinking signatures. It is a
// string so Message stays comparable for existing SDK consumers.
type ModelReasoningDetails string

// NewModelReasoningDetails copies a valid JSON value for durable storage.
func NewModelReasoningDetails(value json.RawMessage) ModelReasoningDetails {
	var compact bytes.Buffer
	if json.Compact(&compact, value) == nil {
		return ModelReasoningDetails(compact.String())
	}
	return ModelReasoningDetails(string(value))
}

// Raw returns a caller-owned copy of provider metadata.
func (d ModelReasoningDetails) Raw() json.RawMessage {
	if d == "" {
		return nil
	}
	return append(json.RawMessage(nil), d...)
}

// ModelReasoning is first-class assistant reasoning. Text is presentation
// content; Details retains opaque provider state required for safe replay.
type ModelReasoning struct {
	Text    string                `json:"text,omitempty"`
	Details ModelReasoningDetails `json:"details,omitempty"`
}

// IsZero reports whether the value contains no reasoning data.
func (r ModelReasoning) IsZero() bool { return r.Text == "" && r.Details == "" }

// Validate checks the durable reasoning representation.
func (r ModelReasoning) Validate() error {
	if r.Details != "" && !json.Valid(r.Details.Raw()) {
		return errors.New("lebro: model reasoning details must be valid JSON")
	}
	return nil
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
	// Canonicalize arguments so semantically equivalent calls (differing only in
	// whitespace or object-key order) share one stable identity for persistence
	// and replay. Validation already guaranteed the input is valid JSON.
	canonical := make([]ModelToolCall, len(calls))
	for i, call := range calls {
		normalized, err := canonicalJSON(call.Arguments)
		if err != nil {
			return ModelToolCalls{}, fmt.Errorf("lebro: model tool call %d arguments: %w", i, err)
		}
		canonical[i] = ModelToolCall{ID: call.ID, ToolID: call.ToolID, Arguments: json.RawMessage(normalized)}
	}
	// Encode without HTML escaping so persisted bytes stay faithful to the
	// source arguments (json.Marshal would escape <, >, & inside RawMessage).
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(canonical); err != nil {
		return ModelToolCalls{}, fmt.Errorf("lebro: encode model tool calls: %w", err)
	}
	encoded := bytes.TrimSpace(buf.Bytes())
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
	if string(data) == "null" {
		*o = ""
		return nil
	}
	if !json.Valid(data) {
		return errors.New("lebro: model structured output must be valid JSON")
	}
	*o = NewModelStructuredOutput(data)
	return nil
}

// ModelUsage records provider-reported token usage when available.
type ModelUsage struct {
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	TotalTokens     int64
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
// A response may carry assistant text, requested tool calls, and structured
// JSON in any combination; Validate enforces internal consistency (for example,
// tool calls require a tool_calls finish reason) rather than exclusivity, so
// adapters can surface whatever the provider returns without ambiguity.
// Extension preserves opaque provider metadata without coupling callers to a
// vendor SDK.
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

// StreamingModel is implemented by adapters that can deliver generated text
// and tool requests as ordered deltas before the terminal response. Adapters
// that do not support streaming continue to satisfy Model alone; callers use
// AsStreamingModel to opt into streaming behavior.
//
// Stream returns a StreamReader whose Next method blocks until the next
// StreamDelta is available, the stream completes, or an error occurs. A nil
// StreamDelta with a nil error is never produced. When the stream completes
// normally the terminal StreamDelta carries a non-zero FinishReason and the
// reader returns io.EOF from subsequent Next calls. When the caller stops
// reading, Close unblocks any in-flight provider work and releases resources;
// it is safe to call after the stream has ended and must be idempotent.
type StreamingModel interface {
	Model
	Stream(context.Context, ModelRequest) (StreamReader, error)
}

// AsStreamingModel returns model as a StreamingModel when the concrete value
// implements Stream. It returns nil when the adapter only supports Generate,
// letting callers fall back to a non-streaming run without type assertions.
func AsStreamingModel(model Model) StreamingModel {
	if model == nil || isNilInterface(model) {
		return nil
	}
	if streaming, ok := model.(StreamingModel); ok {
		return streaming
	}
	return nil
}

// StreamDelta is one ordered item emitted by a streaming model adapter.
//
// Text carries a token (or token chunk) of assistant content; adapters append
// successive Text deltas to reconstruct the final message content. ToolCall
// carries one tool invocation requested by the model; multiple ToolCall deltas
// in a single stream are executed in the order they arrive. StructuredOutput
// carries the final structured payload when the terminal response includes
// one; a non-empty value is only present on the terminal delta.
//
// FinishReason is non-zero only on the terminal delta. Usage is populated on
// the terminal delta when the provider reports token usage. Err is non-nil
// when the adapter aborts the stream before completion; a terminal delta with
// a non-nil Err is the last delta produced.
type StreamDelta struct {
	Text             string
	Reasoning        ModelReasoning
	ToolCall         *ModelToolCall
	StructuredOutput ModelStructuredOutput
	FinishReason     FinishReason
	Usage            ModelUsage
	Err              error
}

// IsTerminal reports whether the delta terminates the stream. A terminal delta
// has a non-zero FinishReason, a non-nil Err, or both.
func (d StreamDelta) IsTerminal() bool {
	return d.FinishReason != "" || d.Err != nil
}

// Validate checks the invariants adapters must preserve for one delta. It is
// called by the agent runtime as deltas arrive so a malformed stream fails
// fast instead of corrupting the transcript.
func (d StreamDelta) Validate() error {
	if d.Text == "" && d.Reasoning.IsZero() && d.ToolCall == nil && d.StructuredOutput == "" && d.FinishReason == "" && d.Usage == (ModelUsage{}) && d.Err == nil {
		return errors.New("lebro: stream delta is empty")
	}
	if d.ToolCall != nil {
		if err := d.ToolCall.Validate(); err != nil {
			return fmt.Errorf("lebro: stream delta tool call: %w", err)
		}
	}
	if d.StructuredOutput != "" && !json.Valid(d.StructuredOutput.Raw()) {
		return errors.New("lebro: stream delta structured output must be valid JSON")
	}
	if err := d.Reasoning.Validate(); err != nil {
		return err
	}
	if d.FinishReason != "" && !validFinishReason(d.FinishReason) {
		return fmt.Errorf("lebro: invalid stream delta finish reason %q", d.FinishReason)
	}
	if d.Usage.InputTokens < 0 || d.Usage.OutputTokens < 0 || d.Usage.ReasoningTokens < 0 || d.Usage.TotalTokens < 0 {
		return errors.New("lebro: stream delta usage must not contain negative token counts")
	}
	return nil
}

// StreamReader is a pull-based iterator over ordered StreamDelta values. A
// reader is owned by a single goroutine; callers must not share one reader
// across goroutines. Next blocks until the next delta is available, the stream
// ends, or an error occurs. After Next returns a terminal delta or a non-nil
// error, subsequent Next calls return io.EOF without blocking. Close releases
// any resources held by the reader and is safe to call after the stream has
// ended.
type StreamReader interface {
	Next() (StreamDelta, error)
	Close() error
}

// StreamReaderFunc adapts a function into a StreamReader. The function is
// called for each Next invocation. A nil function returns io.EOF on Next and a
// nil error on Close. Once Next returns a terminal delta or a non-nil error,
// subsequent Next calls return io.EOF without invoking NextFn again. Close is
// idempotent and invokes CloseFn at most once.
type StreamReaderFunc struct {
	NextFn   func() (StreamDelta, error)
	CloseFn  func() error
	once     sync.Once
	terminal bool
}

// Next calls NextFn when set, otherwise returns io.EOF. After a terminal delta
// or non-nil error has been returned, subsequent calls return io.EOF without
// invoking NextFn.
func (r *StreamReaderFunc) Next() (StreamDelta, error) {
	if r.terminal {
		return StreamDelta{}, io.EOF
	}
	if r.NextFn == nil {
		r.terminal = true
		return StreamDelta{}, io.EOF
	}
	delta, err := r.NextFn()
	if delta.IsTerminal() || err != nil {
		r.terminal = true
	}
	return delta, err
}

// Close calls CloseFn when set and returns nil otherwise. It is idempotent:
// CloseFn is invoked at most once regardless of how many times Close is called.
func (r *StreamReaderFunc) Close() error {
	var err error
	r.once.Do(func() {
		if r.CloseFn != nil {
			err = r.CloseFn()
		}
	})
	return err
}

var _ StreamReader = (*StreamReaderFunc)(nil)

// Validate checks the provider-neutral invariants adapters can rely on.
func (r ModelRequest) Validate() error {
	if err := r.Reasoning.Validate(); err != nil {
		return err
	}
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
	if r.Usage.InputTokens < 0 || r.Usage.OutputTokens < 0 || r.Usage.ReasoningTokens < 0 || r.Usage.TotalTokens < 0 {
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

// canonicalJSON normalizes JSON so semantically equivalent values produce
// identical bytes: whitespace is removed and object keys are sorted, which
// makes the encoding suitable as a comparable identity for persisted/replayed
// tool-call arguments. Numbers are preserved verbatim via json.Number, and
// HTML-sensitive characters in string values are left untouched (no \u003c
// escaping) so the canonical bytes stay faithful to the source arguments.
func canonicalJSON(data []byte) ([]byte, error) {
	var value any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	// json.Encoder appends a trailing newline; drop it for a pure value encoding.
	return bytes.TrimSpace(buf.Bytes()), nil
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
