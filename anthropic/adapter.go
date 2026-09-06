// Package anthropic adapts Anthropic's Messages API to lebro.Model.
package anthropic

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

	claude "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/tesh254/lebro"
)

const (
	providerName           = "anthropic"
	defaultMaxTokens int64 = 4096
)

// Config configures an Anthropic Messages adapter. APIKey is required.
type Config struct {
	APIKey     string
	Model      string
	BaseURL    string
	HTTPClient *http.Client
	MaxTokens  int64
}

// Model implements lebro.Model and lebro.StreamingModel.
type Model struct {
	client    *claude.Client
	model     string
	maxTokens int64
}

var _ lebro.Model = (*Model)(nil)
var _ lebro.StreamingModel = (*Model)(nil)

// New creates an Anthropic adapter safe for concurrent use.
func New(config Config) (*Model, error) {
	if config.APIKey == "" {
		return nil, errors.New("lebro: API key is required")
	}
	opts := []option.RequestOption{option.WithAPIKey(config.APIKey)}
	if config.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(config.BaseURL))
	}
	if config.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(config.HTTPClient))
	}
	maxTokens := config.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}
	if maxTokens < 1 {
		return nil, errors.New("lebro: max tokens must be positive")
	}
	client := claude.NewClient(opts...)
	return &Model{client: &client, model: config.Model, maxTokens: maxTokens}, nil
}

func (m *Model) Generate(ctx context.Context, request lebro.ModelRequest) (lebro.ModelResponse, error) {
	params, err := m.params(request)
	if err != nil {
		return lebro.ModelResponse{}, err
	}
	response, err := m.client.Messages.New(ctx, params)
	if err != nil {
		return lebro.ModelResponse{}, m.error(ctx, err)
	}
	return m.response(request, response)
}

// Stream delivers native Messages API text chunks and complete tool calls. It
// deliberately passes the caller context directly to the SDK: this adapter
// adds no total-request timeout, so stream lifetime is controlled by the
// caller context and any timeout configured on the supplied HTTP client.
func (m *Model) Stream(ctx context.Context, request lebro.ModelRequest) (lebro.StreamReader, error) {
	params, err := m.params(request)
	if err != nil {
		return nil, err
	}
	stream := m.client.Messages.NewStreaming(ctx, params)
	reader := &anthropicStream{stream: stream, values: make(chan lebro.StreamDelta, 8), done: make(chan struct{}), errorFn: m.error}
	reader.start(ctx, request)
	return reader, nil
}

func (m *Model) params(request lebro.ModelRequest) (claude.MessageNewParams, error) {
	if err := request.Validate(); err != nil {
		return claude.MessageNewParams{}, m.invalid(err)
	}
	model := request.Model
	if model == "" {
		model = m.model
	}
	if model == "" {
		return claude.MessageNewParams{}, m.invalid(errors.New("lebro: model is required"))
	}
	params := claude.MessageNewParams{Model: claude.Model(model), MaxTokens: m.maxTokens}
	replayThinking := false
	if thinking, err := m.reasoningParams(request.Reasoning); err != nil {
		return claude.MessageNewParams{}, m.invalid(err)
	} else if thinking != nil {
		params.Thinking = *thinking
		replayThinking = thinking.OfEnabled != nil
	}
	for _, message := range request.Messages {
		switch message.Role {
		case lebro.RoleSystem:
			params.System = append(params.System, claude.TextBlockParam{Text: message.Content})
		case lebro.RoleUser:
			params.Messages = append(params.Messages, claude.NewUserMessage(claude.NewTextBlock(message.Content)))
		case lebro.RoleAssistant:
			blocks := []claude.ContentBlockParamUnion{}
			if replayThinking {
				thinking, err := anthropicReasoningBlocks(message.Reasoning)
				if err != nil {
					return claude.MessageNewParams{}, m.invalid(err)
				}
				blocks = append(blocks, thinking...)
			}
			if message.Content != "" {
				blocks = append(blocks, claude.NewTextBlock(message.Content))
			}
			for _, call := range message.ToolCalls.Values() {
				var input any
				if err := json.Unmarshal(call.Arguments, &input); err != nil {
					return claude.MessageNewParams{}, m.invalid(err)
				}
				blocks = append(blocks, claude.NewToolUseBlock(call.ID, input, string(call.ToolID)))
			}
			params.Messages = append(params.Messages, claude.NewAssistantMessage(blocks...))
		case lebro.RoleTool:
			params.Messages = append(params.Messages, claude.NewUserMessage(claude.NewToolResultBlock(message.ToolCallID, message.Content, false)))
		}
	}
	for _, tool := range request.Tools {
		schema := map[string]any{}
		if len(tool.InputSchema) > 0 {
			if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
				return claude.MessageNewParams{}, m.invalid(err)
			}
		}
		params.Tools = append(params.Tools, claude.ToolUnionParam{OfTool: &claude.ToolParam{
			Name: string(tool.ID), Description: claude.String(tool.Description),
			InputSchema: claude.ToolInputSchemaParam{ExtraFields: schema},
		}})
	}
	if request.OutputSchema != nil {
		var schema map[string]any
		if err := json.Unmarshal(request.OutputSchema.Schema, &schema); err != nil {
			return claude.MessageNewParams{}, m.invalid(err)
		}
		params.OutputConfig = claude.OutputConfigParam{Format: claude.JSONOutputFormatParam{Schema: schema}}
	}
	return params, nil
}

// reasoningParams maps neutral effort to Anthropic's token-budget mechanism.
// Every enabled budget must be at least 1024 and strictly below max_tokens.
func (m *Model) reasoningParams(config lebro.ReasoningConfig) (*claude.ThinkingConfigParamUnion, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if config.IsZero() {
		return nil, nil
	}
	if config.Effort == lebro.ReasoningOff {
		disabled := claude.NewThinkingConfigDisabledParam()
		return &claude.ThinkingConfigParamUnion{OfDisabled: &disabled}, nil
	}
	if m.maxTokens <= 1024 {
		return nil, errors.New("lebro: Anthropic reasoning requires max tokens greater than 1024")
	}
	budget := config.BudgetTokens
	if budget == 0 {
		switch config.Effort {
		case lebro.ReasoningMinimal, lebro.ReasoningLow:
			budget = 1024
		case lebro.ReasoningMedium:
			budget = m.maxTokens / 2
		case lebro.ReasoningHigh:
			budget = (m.maxTokens * 3) / 4
		case lebro.ReasoningXHigh, lebro.ReasoningMax:
			budget = m.maxTokens - 1
		default:
			return nil, fmt.Errorf("lebro: Anthropic does not support reasoning effort %q", config.Effort)
		}
	}
	if budget < 1024 || budget >= m.maxTokens {
		return nil, fmt.Errorf("lebro: Anthropic reasoning budget %d must be at least 1024 and less than max tokens %d", budget, m.maxTokens)
	}
	return &claude.ThinkingConfigParamUnion{OfEnabled: &claude.ThinkingConfigEnabledParam{BudgetTokens: budget}}, nil
}

type anthropicReasoningDetail struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
}

func anthropicReasoningBlocks(reasoning lebro.ModelReasoning) ([]claude.ContentBlockParamUnion, error) {
	if reasoning.IsZero() || reasoning.Details == "" {
		return nil, nil
	}
	var details []anthropicReasoningDetail
	if err := json.Unmarshal(reasoning.Details.Raw(), &details); err != nil {
		return nil, fmt.Errorf("lebro: decode Anthropic reasoning details: %w", err)
	}
	blocks := make([]claude.ContentBlockParamUnion, 0, len(details))
	for _, detail := range details {
		switch detail.Type {
		case "thinking":
			if detail.Signature == "" || detail.Thinking == "" {
				return nil, errors.New("lebro: Anthropic thinking details require signature and thinking text")
			}
			blocks = append(blocks, claude.NewThinkingBlock(detail.Signature, detail.Thinking))
		case "redacted_thinking":
			if detail.Data == "" {
				return nil, errors.New("lebro: Anthropic redacted thinking details require data")
			}
			blocks = append(blocks, claude.NewRedactedThinkingBlock(detail.Data))
		}
	}
	return blocks, nil
}

func newAnthropicReasoning(text string, details []anthropicReasoningDetail) lebro.ModelReasoning {
	reasoning := lebro.ModelReasoning{Text: text}
	if len(details) > 0 {
		encoded, _ := json.Marshal(details)
		reasoning.Details = lebro.NewModelReasoningDetails(encoded)
	}
	return reasoning
}

func reasoningText(details []anthropicReasoningDetail) string {
	var text strings.Builder
	for _, detail := range details {
		text.WriteString(detail.Thinking)
	}
	return text.String()
}

func (m *Model) response(request lebro.ModelRequest, result *claude.Message) (lebro.ModelResponse, error) {
	if result == nil {
		return lebro.ModelResponse{}, m.malformed(errors.New("lebro: empty Anthropic response"))
	}
	var text strings.Builder
	calls := make([]lebro.ModelToolCall, 0)
	details := make([]anthropicReasoningDetail, 0)
	for _, block := range result.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			calls = append(calls, lebro.ModelToolCall{ID: block.ID, ToolID: lebro.ToolID(block.Name), Arguments: append(json.RawMessage(nil), block.Input...)})
		case "thinking":
			details = append(details, anthropicReasoningDetail{Type: block.Type, Thinking: block.Thinking, Signature: block.Signature})
		case "redacted_thinking":
			details = append(details, anthropicReasoningDetail{Type: block.Type, Data: block.Data})
		}
	}
	message := lebro.Message{Role: lebro.RoleAssistant, Content: text.String(), Reasoning: newAnthropicReasoning(reasoningText(details), details)}
	if len(calls) > 0 {
		encoded, err := lebro.NewModelToolCalls(calls...)
		if err != nil {
			return lebro.ModelResponse{}, m.malformed(err)
		}
		message.ToolCalls = encoded
	}
	if request.OutputSchema != nil && len(calls) == 0 {
		if !json.Valid([]byte(message.Content)) {
			return lebro.ModelResponse{}, m.malformed(errors.New("lebro: Anthropic structured output is not valid JSON"))
		}
		message.StructuredOutput = lebro.NewModelStructuredOutput(json.RawMessage(message.Content))
	}
	response := lebro.ModelResponse{Message: message, FinishReason: mapFinish(result.StopReason), Usage: lebro.ModelUsage{InputTokens: result.Usage.InputTokens, OutputTokens: result.Usage.OutputTokens, ReasoningTokens: result.Usage.OutputTokensDetails.ThinkingTokens, TotalTokens: result.Usage.InputTokens + result.Usage.OutputTokens}}
	if result.ID != "" {
		response.Extension, _ = json.Marshal(map[string]string{"anthropic_id": result.ID, "anthropic_model": string(result.Model)})
	}
	if err := response.Validate(); err != nil {
		return lebro.ModelResponse{}, m.malformed(err)
	}
	return response, nil
}

func mapFinish(reason claude.StopReason) lebro.FinishReason {
	switch reason {
	case "tool_use":
		return lebro.FinishReasonToolCalls
	case "max_tokens", "model_context_window_exceeded":
		return lebro.FinishReasonLength
	case "refusal":
		return lebro.FinishReasonContent
	default:
		return lebro.FinishReasonStop
	}
}
func (m *Model) invalid(err error) error {
	return &lebro.ModelError{Kind: lebro.ModelErrorInvalidRequest, Provider: providerName, Message: err.Error(), Err: err}
}
func (m *Model) malformed(err error) error {
	return &lebro.ModelError{Kind: lebro.ModelErrorMalformedResponse, Provider: providerName, Message: err.Error(), Err: err}
}
func (m *Model) error(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
		return context.DeadlineExceeded
	}
	var apiErr *claude.Error
	if errors.As(err, &apiErr) {
		status := apiErr.StatusCode
		return &lebro.ModelError{Kind: statusToKind(status), Provider: providerName, StatusCode: status, Message: err.Error(), Err: err}
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		kind := lebro.ModelErrorTransport
		if networkErr.Timeout() {
			kind = lebro.ModelErrorTimeout
		}
		return &lebro.ModelError{Kind: kind, Provider: providerName, Message: err.Error(), Err: err}
	}
	return &lebro.ModelError{Kind: lebro.ModelErrorUnavailable, Provider: providerName, Message: err.Error(), Err: err}
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

type anthropicStream struct {
	stream interface {
		Next() bool
		Current() claude.MessageStreamEventUnion
		Err() error
		Close() error
	}
	values  chan lebro.StreamDelta
	done    chan struct{}
	errorFn func(context.Context, error) error
	once    sync.Once
}

func (r *anthropicStream) send(delta lebro.StreamDelta) bool {
	select {
	case r.values <- delta:
		return true
	case <-r.done:
		return false
	}
}

func (r *anthropicStream) start(ctx context.Context, request lebro.ModelRequest) {
	go func() {
		defer close(r.values)
		tools := map[int64]*lebro.ModelToolCall{}
		thinking := map[int64]*strings.Builder{}
		thinkingSignatures := map[int64]string{}
		redacted := map[int64]string{}
		var text strings.Builder
		var finish = lebro.FinishReasonStop
		var usage lebro.ModelUsage
		for r.stream.Next() {
			event := r.stream.Current()
			switch event.Type {
			case "content_block_start":
				if event.ContentBlock.Type == "tool_use" {
					tools[event.Index] = &lebro.ModelToolCall{ID: event.ContentBlock.ID, ToolID: lebro.ToolID(event.ContentBlock.Name)}
				}
				if event.ContentBlock.Type == "thinking" {
					thinking[event.Index] = &strings.Builder{}
				}
				if event.ContentBlock.Type == "redacted_thinking" {
					redacted[event.Index] = event.ContentBlock.Data
				}
			case "content_block_delta":
				if event.Delta.Text != "" {
					text.WriteString(event.Delta.Text)
					if !r.send(lebro.StreamDelta{Text: event.Delta.Text}) {
						return
					}
				}
				if call := tools[event.Index]; call != nil {
					call.Arguments = append(call.Arguments, event.Delta.PartialJSON...)
				}
				if event.Delta.Thinking != "" {
					if block := thinking[event.Index]; block != nil {
						block.WriteString(event.Delta.Thinking)
					}
					if !r.send(lebro.StreamDelta{Reasoning: lebro.ModelReasoning{Text: event.Delta.Thinking}}) {
						return
					}
				}
				if event.Delta.Signature != "" {
					thinkingSignatures[event.Index] += event.Delta.Signature
				}
			case "content_block_stop":
				if call := tools[event.Index]; call != nil {
					if len(call.Arguments) == 0 {
						call.Arguments = json.RawMessage(`{}`)
					}
					if !json.Valid(call.Arguments) {
						r.send(lebro.StreamDelta{Err: &lebro.ModelError{Kind: lebro.ModelErrorMalformedResponse, Provider: providerName, Message: "lebro: Anthropic streamed tool arguments are not valid JSON"}})
						return
					}
					copy := *call
					if !r.send(lebro.StreamDelta{ToolCall: &copy}) {
						return
					}
					delete(tools, event.Index)
				}
				if block := thinking[event.Index]; block != nil {
					detail := anthropicReasoningDetail{Type: "thinking", Thinking: block.String(), Signature: thinkingSignatures[event.Index]}
					if detail.Signature == "" {
						r.send(lebro.StreamDelta{Err: &lebro.ModelError{Kind: lebro.ModelErrorMalformedResponse, Provider: providerName, Message: "lebro: Anthropic thinking block has no signature"}})
						return
					}
					if !r.send(lebro.StreamDelta{Reasoning: newAnthropicReasoning("", []anthropicReasoningDetail{detail})}) {
						return
					}
					delete(thinking, event.Index)
					delete(thinkingSignatures, event.Index)
				}
				if data, ok := redacted[event.Index]; ok {
					if !r.send(lebro.StreamDelta{Reasoning: newAnthropicReasoning("", []anthropicReasoningDetail{{Type: "redacted_thinking", Data: data}})}) {
						return
					}
					delete(redacted, event.Index)
				}
			case "message_delta":
				finish = mapFinish(event.Delta.StopReason)
				usage = lebro.ModelUsage{InputTokens: event.Usage.InputTokens, OutputTokens: event.Usage.OutputTokens, ReasoningTokens: event.Usage.OutputTokensDetails.ThinkingTokens, TotalTokens: event.Usage.InputTokens + event.Usage.OutputTokens}
			}
		}
		if err := r.stream.Err(); err != nil {
			r.send(lebro.StreamDelta{Err: r.errorFn(ctx, err)})
			return
		}
		terminal := lebro.StreamDelta{FinishReason: finish, Usage: usage}
		if request.OutputSchema != nil && json.Valid([]byte(text.String())) {
			terminal.StructuredOutput = lebro.NewModelStructuredOutput(json.RawMessage(text.String()))
		}
		r.send(terminal)
	}()
}
func (r *anthropicStream) Next() (lebro.StreamDelta, error) {
	delta, ok := <-r.values
	if !ok {
		return lebro.StreamDelta{}, io.EOF
	}
	return delta, nil
}
func (r *anthropicStream) Close() error {
	var err error
	r.once.Do(func() {
		close(r.done)
		err = r.stream.Close()
	})
	return err
}
