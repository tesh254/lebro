// Package geminiapi shares Gemini-protocol request and response mapping
// between the Gemini Developer API and Vertex AI adapters. It exists only for
// adapters in this repository; it is not a public contract.
package geminiapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/tesh254/lebro"
	genai "google.golang.org/genai"
)

// Config configures a shared Gemini-protocol adapter.
type Config struct {
	// Provider labels ModelError values reported by the adapter, for example
	// "gemini" or "vertexai".
	Provider string
	// Client is the genai client bound to a Gemini-protocol backend. It must
	// not be nil.
	Client *genai.Client
	// Model is used when a request does not name a model.
	Model string
}

// Model implements lebro.Model and lebro.StreamingModel against any
// Gemini-protocol genai client. Authentication and endpoint selection belong
// to the client; this type only maps the neutral protocol.
type Model struct {
	client   *genai.Client
	model    string
	provider string
}

var _ lebro.Model = (*Model)(nil)
var _ lebro.StreamingModel = (*Model)(nil)

// New creates a shared adapter safe for concurrent use.
func New(config Config) *Model {
	return &Model{client: config.Client, model: config.Model, provider: config.Provider}
}

func (m *Model) Generate(ctx context.Context, request lebro.ModelRequest) (lebro.ModelResponse, error) {
	model, contents, config, err := m.params(request)
	if err != nil {
		return lebro.ModelResponse{}, err
	}
	result, err := m.client.Models.GenerateContent(ctx, model, contents, config)
	if err != nil {
		return lebro.ModelResponse{}, m.error(ctx, err)
	}
	return m.response(request, result)
}

// Stream uses the backend's native streamGenerateContent endpoint.
func (m *Model) Stream(ctx context.Context, request lebro.ModelRequest) (lebro.StreamReader, error) {
	model, contents, config, err := m.params(request)
	if err != nil {
		return nil, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	r := &stream{values: make(chan lebro.StreamDelta, 8), done: make(chan struct{}), cancel: cancel, errorFn: m.error, provider: m.provider}
	go r.run(streamCtx, request, m.client.Models.GenerateContentStream(streamCtx, model, contents, config))
	return r, nil
}

func (m *Model) params(request lebro.ModelRequest) (string, []*genai.Content, *genai.GenerateContentConfig, error) {
	if err := request.Validate(); err != nil {
		return "", nil, nil, m.invalid(err)
	}
	model := request.Model
	if model == "" {
		model = m.model
	}
	if model == "" {
		return "", nil, nil, m.invalid(errors.New("lebro: model is required"))
	}
	config := &genai.GenerateContentConfig{}
	if thinking, err := geminiThinkingConfig(model, request.Reasoning); err != nil {
		return "", nil, nil, m.invalid(err)
	} else {
		config.ThinkingConfig = thinking
	}
	contents := make([]*genai.Content, 0, len(request.Messages))
	callNames := map[string]string{}
	for _, message := range request.Messages {
		switch message.Role {
		case lebro.RoleSystem:
			if config.SystemInstruction == nil {
				config.SystemInstruction = genai.NewContentFromText(message.Content, genai.RoleUser)
			} else {
				config.SystemInstruction.Parts = append(config.SystemInstruction.Parts, genai.NewPartFromText(message.Content))
			}
		case lebro.RoleUser:
			contents = append(contents, genai.NewContentFromText(message.Content, genai.RoleUser))
		case lebro.RoleAssistant:
			parts := []*genai.Part{}
			reasoning, err := geminiReasoningParts(message.Reasoning)
			if err != nil {
				return "", nil, nil, m.invalid(err)
			}
			parts = append(parts, reasoning...)
			if message.Content != "" {
				parts = append(parts, genai.NewPartFromText(message.Content))
			}
			for _, call := range message.ToolCalls.Values() {
				var args map[string]any
				if err := json.Unmarshal(call.Arguments, &args); err != nil {
					return "", nil, nil, m.invalid(err)
				}
				callNames[call.ID] = string(call.ToolID)
				parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{ID: call.ID, Name: string(call.ToolID), Args: args}})
			}
			contents = append(contents, genai.NewContentFromParts(parts, genai.RoleModel))
		case lebro.RoleTool:
			name := callNames[message.ToolCallID]
			if name == "" {
				return "", nil, nil, m.invalid(errors.New("lebro: Gemini tool result has no matching tool call"))
			}
			contents = append(contents, genai.NewContentFromParts([]*genai.Part{
				{FunctionResponse: &genai.FunctionResponse{ID: message.ToolCallID, Name: name, Response: map[string]any{"result": message.Content}}},
			}, genai.RoleUser))
		}
	}
	if len(request.Tools) > 0 {
		declarations := make([]*genai.FunctionDeclaration, 0, len(request.Tools))
		for _, tool := range request.Tools {
			var schema any = map[string]any{"type": "object"}
			if len(tool.InputSchema) > 0 {
				if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
					return "", nil, nil, m.invalid(err)
				}
			}
			declarations = append(declarations, &genai.FunctionDeclaration{Name: string(tool.ID), Description: tool.Description, ParametersJsonSchema: schema})
		}
		config.Tools = []*genai.Tool{{FunctionDeclarations: declarations}}
	}
	if request.OutputSchema != nil {
		var schema any
		if err := json.Unmarshal(request.OutputSchema.Schema, &schema); err != nil {
			return "", nil, nil, m.invalid(err)
		}
		config.ResponseMIMEType = "application/json"
		config.ResponseJsonSchema = schema
	}
	return model, contents, config, nil
}

// geminiThinkingConfig maps effort to the Gemini generations each model
// family supports. Gemini 2.5 uses token budgets and can disable thinking
// with a zero budget; Gemini 3 uses thinking levels and cannot disable
// thinking, so off requests the lowest level instead. Unsupported neutral
// tiers fail instead of becoming a lower tier.
func geminiThinkingConfig(model string, reasoning lebro.ReasoningConfig) (*genai.ThinkingConfig, error) {
	if err := reasoning.Validate(); err != nil {
		return nil, err
	}
	if reasoning.IsZero() {
		return nil, nil
	}
	if reasoning.BudgetTokens > int64(^uint32(0)>>1) {
		return nil, errors.New("lebro: Gemini reasoning budget exceeds int32")
	}
	if strings.Contains(strings.ToLower(model), "gemini-2.5") {
		budget := reasoning.BudgetTokens
		if budget == 0 {
			switch reasoning.Effort {
			case lebro.ReasoningOff:
				return &genai.ThinkingConfig{ThinkingBudget: genai.Ptr[int32](0)}, nil
			case lebro.ReasoningMinimal, lebro.ReasoningLow:
				budget = 1024
			case lebro.ReasoningMedium:
				budget = 4096
			case lebro.ReasoningHigh:
				budget = 8192
			default:
				return nil, fmt.Errorf("lebro: Gemini 2.5 does not support reasoning effort %q", reasoning.Effort)
			}
		}
		return &genai.ThinkingConfig{IncludeThoughts: true, ThinkingBudget: genai.Ptr(int32(budget))}, nil
	}
	if reasoning.BudgetTokens != 0 {
		return nil, errors.New("lebro: Gemini thinking budgets are only supported by Gemini 2.5 models")
	}
	if reasoning.Effort == lebro.ReasoningOff {
		if strings.Contains(strings.ToLower(model), "gemini-3") {
			// Gemini 3 cannot disable thinking; the lowest level is the
			// closest representable setting and thoughts stay unsurfaced.
			return &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevelMinimal}, nil
		}
		// Older non-thinking model families reject ThinkingConfig. Omitting it
		// is the only representable off setting.
		return nil, nil
	}
	var level genai.ThinkingLevel
	switch reasoning.Effort {
	case lebro.ReasoningMinimal:
		level = genai.ThinkingLevelMinimal
	case lebro.ReasoningLow:
		level = genai.ThinkingLevelLow
	case lebro.ReasoningMedium:
		level = genai.ThinkingLevelMedium
	case lebro.ReasoningHigh:
		level = genai.ThinkingLevelHigh
	default:
		return nil, fmt.Errorf("lebro: Gemini does not support reasoning effort %q", reasoning.Effort)
	}
	return &genai.ThinkingConfig{IncludeThoughts: true, ThinkingLevel: level}, nil
}

type geminiReasoningDetail struct {
	Text             string `json:"text"`
	ThoughtSignature []byte `json:"thought_signature,omitempty"`
}

func geminiReasoningParts(reasoning lebro.ModelReasoning) ([]*genai.Part, error) {
	if reasoning.IsZero() || reasoning.Details == "" {
		return nil, nil
	}
	var details []geminiReasoningDetail
	if err := json.Unmarshal(reasoning.Details.Raw(), &details); err != nil {
		return nil, fmt.Errorf("lebro: decode Gemini reasoning details: %w", err)
	}
	parts := make([]*genai.Part, 0, len(details))
	for _, detail := range details {
		if detail.Text == "" && len(detail.ThoughtSignature) == 0 {
			// The transcript belongs to another adapter. Opaque state is never
			// meaningful across providers, so omit it rather than corrupting a
			// Gemini continuation with foreign metadata.
			continue
		}
		parts = append(parts, &genai.Part{Text: detail.Text, Thought: true, ThoughtSignature: append([]byte(nil), detail.ThoughtSignature...)})
	}
	return parts, nil
}

func newGeminiReasoning(text string, details []geminiReasoningDetail) lebro.ModelReasoning {
	reasoning := lebro.ModelReasoning{Text: text}
	if len(details) > 0 {
		encoded, _ := json.Marshal(details)
		reasoning.Details = lebro.NewModelReasoningDetails(encoded)
	}
	return reasoning
}

func (m *Model) response(request lebro.ModelRequest, result *genai.GenerateContentResponse) (lebro.ModelResponse, error) {
	if result == nil || len(result.Candidates) == 0 {
		return lebro.ModelResponse{}, m.malformed(errors.New("lebro: Gemini response has no candidates"))
	}
	candidate := result.Candidates[0]
	message := lebro.Message{Role: lebro.RoleAssistant}
	calls := []lebro.ModelToolCall{}
	details := []geminiReasoningDetail{}
	if candidate.Content != nil {
		for _, part := range candidate.Content.Parts {
			if part.Thought {
				details = append(details, geminiReasoningDetail{Text: part.Text, ThoughtSignature: append([]byte(nil), part.ThoughtSignature...)})
				continue
			}
			if part.Text != "" {
				message.Content += part.Text
			}
			if call := part.FunctionCall; call != nil {
				args, err := json.Marshal(call.Args)
				if err != nil {
					return lebro.ModelResponse{}, m.malformed(err)
				}
				calls = append(calls, lebro.ModelToolCall{ID: call.ID, ToolID: lebro.ToolID(call.Name), Arguments: args})
			}
		}
	}
	message.Reasoning = newGeminiReasoning(geminiReasoningText(details), details)
	if len(calls) > 0 {
		encoded, err := lebro.NewModelToolCalls(calls...)
		if err != nil {
			return lebro.ModelResponse{}, m.malformed(err)
		}
		message.ToolCalls = encoded
	}
	if request.OutputSchema != nil && len(calls) == 0 {
		if !json.Valid([]byte(message.Content)) {
			return lebro.ModelResponse{}, m.malformed(errors.New("lebro: Gemini structured output is not valid JSON"))
		}
		message.StructuredOutput = lebro.NewModelStructuredOutput(json.RawMessage(message.Content))
	}
	finish := mapFinish(candidate.FinishReason)
	if len(calls) > 0 {
		finish = lebro.FinishReasonToolCalls
	}
	response := lebro.ModelResponse{Message: message, FinishReason: finish}
	if result.UsageMetadata != nil {
		response.Usage = lebro.ModelUsage{InputTokens: int64(result.UsageMetadata.PromptTokenCount), OutputTokens: int64(result.UsageMetadata.CandidatesTokenCount), ReasoningTokens: int64(result.UsageMetadata.ThoughtsTokenCount), TotalTokens: int64(result.UsageMetadata.TotalTokenCount)}
	}
	if result.ResponseID != "" {
		response.Extension, _ = json.Marshal(map[string]string{"gemini_response_id": result.ResponseID, "gemini_model": result.ModelVersion})
	}
	if err := response.Validate(); err != nil {
		return lebro.ModelResponse{}, m.malformed(err)
	}
	return response, nil
}

func geminiReasoningText(details []geminiReasoningDetail) string {
	var text strings.Builder
	for _, detail := range details {
		text.WriteString(detail.Text)
	}
	return text.String()
}

func mapFinish(reason genai.FinishReason) lebro.FinishReason {
	switch reason {
	case genai.FinishReasonMaxTokens:
		return lebro.FinishReasonLength
	case genai.FinishReasonSafety, genai.FinishReasonRecitation, genai.FinishReasonBlocklist, genai.FinishReasonProhibitedContent:
		return lebro.FinishReasonContent
	default:
		return lebro.FinishReasonStop
	}
}
func (m *Model) invalid(err error) error {
	return &lebro.ModelError{Kind: lebro.ModelErrorInvalidRequest, Provider: m.provider, Message: err.Error(), Err: err}
}
func (m *Model) malformed(err error) error {
	return &lebro.ModelError{Kind: lebro.ModelErrorMalformedResponse, Provider: m.provider, Message: err.Error(), Err: err}
}
func (m *Model) error(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
		return context.DeadlineExceeded
	}
	var apiErr genai.APIError
	if errors.As(err, &apiErr) {
		return &lebro.ModelError{Kind: statusToKind(apiErr.Code), Provider: m.provider, StatusCode: apiErr.Code, Code: apiErr.Status, Message: apiErr.Message, Err: err}
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		kind := lebro.ModelErrorTransport
		if networkErr.Timeout() {
			kind = lebro.ModelErrorTimeout
		}
		return &lebro.ModelError{Kind: kind, Provider: m.provider, Message: err.Error(), Err: err}
	}
	return &lebro.ModelError{Kind: lebro.ModelErrorUnavailable, Provider: m.provider, Message: err.Error(), Err: err}
}

func statusToKind(status int) lebro.ModelErrorKind {
	switch {
	case status == http.StatusUnauthorized:
		return lebro.ModelErrorAuthentication
	case status == http.StatusForbidden:
		return lebro.ModelErrorPermissionDenied
	case status == http.StatusNotFound:
		return lebro.ModelErrorNotFound
	case status == http.StatusTooManyRequests:
		return lebro.ModelErrorRateLimited
	case status >= 400 && status < 500:
		return lebro.ModelErrorInvalidRequest
	case status >= 500 && status < 600:
		return lebro.ModelErrorUnavailable
	default:
		return lebro.ModelErrorUnknown
	}
}

type stream struct {
	values   chan lebro.StreamDelta
	done     chan struct{}
	cancel   context.CancelFunc
	errorFn  func(context.Context, error) error
	provider string
	once     sync.Once
}

func (r *stream) send(delta lebro.StreamDelta) bool {
	select {
	case r.values <- delta:
		return true
	case <-r.done:
		return false
	}
}

func (r *stream) run(ctx context.Context, request lebro.ModelRequest, sequence func(func(*genai.GenerateContentResponse, error) bool)) {
	defer close(r.values)
	var text strings.Builder
	failed := false
	hasToolCalls := false
	finish := lebro.FinishReasonStop
	var usage lebro.ModelUsage
	sequence(func(result *genai.GenerateContentResponse, err error) bool {
		if err != nil {
			r.send(lebro.StreamDelta{Err: r.errorFn(ctx, err)})
			failed = true
			return false
		}
		if result == nil {
			return true
		}
		if result.UsageMetadata != nil {
			usage = lebro.ModelUsage{InputTokens: int64(result.UsageMetadata.PromptTokenCount), OutputTokens: int64(result.UsageMetadata.CandidatesTokenCount), ReasoningTokens: int64(result.UsageMetadata.ThoughtsTokenCount), TotalTokens: int64(result.UsageMetadata.TotalTokenCount)}
		}
		for _, candidate := range result.Candidates {
			if candidate.FinishReason != "" {
				finish = mapFinish(candidate.FinishReason)
			}
			if candidate.Content == nil {
				continue
			}
			for _, part := range candidate.Content.Parts {
				if part.Thought {
					detail := geminiReasoningDetail{Text: part.Text, ThoughtSignature: append([]byte(nil), part.ThoughtSignature...)}
					if !r.send(lebro.StreamDelta{Reasoning: newGeminiReasoning(part.Text, []geminiReasoningDetail{detail})}) {
						return false
					}
					continue
				}
				if part.Text != "" {
					text.WriteString(part.Text)
					if !r.send(lebro.StreamDelta{Text: part.Text}) {
						return false
					}
				}
				if call := part.FunctionCall; call != nil {
					hasToolCalls = true
					args, err := json.Marshal(call.Args)
					if err != nil {
						r.send(lebro.StreamDelta{Err: &lebro.ModelError{Kind: lebro.ModelErrorMalformedResponse, Provider: r.provider, Message: err.Error(), Err: err}})
						failed = true
						return false
					}
					if !r.send(lebro.StreamDelta{ToolCall: &lebro.ModelToolCall{ID: call.ID, ToolID: lebro.ToolID(call.Name), Arguments: args}}) {
						return false
					}
				}
			}
		}
		return ctx.Err() == nil
	})
	if failed {
		return
	}
	terminal := lebro.StreamDelta{FinishReason: finish, Usage: usage}
	if hasToolCalls {
		terminal.FinishReason = lebro.FinishReasonToolCalls
	}
	if request.OutputSchema != nil && json.Valid([]byte(text.String())) {
		terminal.StructuredOutput = lebro.NewModelStructuredOutput(json.RawMessage(text.String()))
	}
	r.send(terminal)
}
func (r *stream) Next() (lebro.StreamDelta, error) {
	value, ok := <-r.values
	if !ok {
		return lebro.StreamDelta{}, io.EOF
	}
	return value, nil
}
func (r *stream) Close() error {
	r.once.Do(func() {
		close(r.done)
		r.cancel()
	})
	return nil
}
