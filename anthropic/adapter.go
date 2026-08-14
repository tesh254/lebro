// Package anthropic adapts Anthropic's Messages API to lebro.Model.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
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

// Stream delivers native Messages API text chunks and complete tool calls.
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
	for _, message := range request.Messages {
		switch message.Role {
		case lebro.RoleSystem:
			params.System = append(params.System, claude.TextBlockParam{Text: message.Content})
		case lebro.RoleUser:
			params.Messages = append(params.Messages, claude.NewUserMessage(claude.NewTextBlock(message.Content)))
		case lebro.RoleAssistant:
			blocks := []claude.ContentBlockParamUnion{}
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

func (m *Model) response(request lebro.ModelRequest, result *claude.Message) (lebro.ModelResponse, error) {
	if result == nil {
		return lebro.ModelResponse{}, m.malformed(errors.New("lebro: empty Anthropic response"))
	}
	var text strings.Builder
	calls := make([]lebro.ModelToolCall, 0)
	for _, block := range result.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			calls = append(calls, lebro.ModelToolCall{ID: block.ID, ToolID: lebro.ToolID(block.Name), Arguments: append(json.RawMessage(nil), block.Input...)})
		}
	}
	message := lebro.Message{Role: lebro.RoleAssistant, Content: text.String()}
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
	response := lebro.ModelResponse{Message: message, FinishReason: mapFinish(result.StopReason), Usage: lebro.ModelUsage{InputTokens: result.Usage.InputTokens, OutputTokens: result.Usage.OutputTokens, TotalTokens: result.Usage.InputTokens + result.Usage.OutputTokens}}
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
			case "message_delta":
				finish = mapFinish(event.Delta.StopReason)
				usage = lebro.ModelUsage{InputTokens: event.Usage.InputTokens, OutputTokens: event.Usage.OutputTokens, TotalTokens: event.Usage.InputTokens + event.Usage.OutputTokens}
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
