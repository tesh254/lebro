// Package gemini adapts Gemini Developer API models to lebro.Model.
package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tesh254/lebro"
	genai "google.golang.org/genai"
)

const providerName = "gemini"

// Config configures a Gemini Developer API adapter. APIKey is required.
type Config struct {
	APIKey     string
	Model      string
	BaseURL    string
	HTTPClient *http.Client
	Timeout    time.Duration
}

// Model implements lebro.Model and lebro.StreamingModel.
type Model struct {
	client *genai.Client
	model  string
}

var _ lebro.Model = (*Model)(nil)
var _ lebro.StreamingModel = (*Model)(nil)

// New creates a Gemini Developer API adapter safe for concurrent use.
func New(config Config) (*Model, error) {
	if config.APIKey == "" {
		return nil, errors.New("lebro: API key is required")
	}
	options := genai.HTTPOptions{BaseURL: config.BaseURL}
	if config.Timeout > 0 {
		options.Timeout = &config.Timeout
	}
	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{APIKey: config.APIKey, Backend: genai.BackendGeminiAPI, HTTPClient: config.HTTPClient, HTTPOptions: options})
	if err != nil {
		return nil, err
	}
	return &Model{client: client, model: config.Model}, nil
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

// Stream uses Gemini's native streamGenerateContent endpoint.
func (m *Model) Stream(ctx context.Context, request lebro.ModelRequest) (lebro.StreamReader, error) {
	model, contents, config, err := m.params(request)
	if err != nil {
		return nil, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	r := &geminiStream{values: make(chan lebro.StreamDelta, 8), done: make(chan struct{}), cancel: cancel, errorFn: m.error}
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

func (m *Model) response(request lebro.ModelRequest, result *genai.GenerateContentResponse) (lebro.ModelResponse, error) {
	if result == nil || len(result.Candidates) == 0 {
		return lebro.ModelResponse{}, m.malformed(errors.New("lebro: Gemini response has no candidates"))
	}
	candidate := result.Candidates[0]
	message := lebro.Message{Role: lebro.RoleAssistant}
	calls := []lebro.ModelToolCall{}
	if candidate.Content != nil {
		for _, part := range candidate.Content.Parts {
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
		response.Usage = lebro.ModelUsage{InputTokens: int64(result.UsageMetadata.PromptTokenCount), OutputTokens: int64(result.UsageMetadata.CandidatesTokenCount), TotalTokens: int64(result.UsageMetadata.TotalTokenCount)}
	}
	if result.ResponseID != "" {
		response.Extension, _ = json.Marshal(map[string]string{"gemini_response_id": result.ResponseID, "gemini_model": result.ModelVersion})
	}
	if err := response.Validate(); err != nil {
		return lebro.ModelResponse{}, m.malformed(err)
	}
	return response, nil
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
	var apiErr genai.APIError
	if errors.As(err, &apiErr) {
		return &lebro.ModelError{Kind: statusToKind(apiErr.Code), Provider: providerName, StatusCode: apiErr.Code, Code: apiErr.Status, Message: apiErr.Message, Err: err}
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

type geminiStream struct {
	values  chan lebro.StreamDelta
	done    chan struct{}
	cancel  context.CancelFunc
	errorFn func(context.Context, error) error
	once    sync.Once
}

func (r *geminiStream) send(delta lebro.StreamDelta) bool {
	select {
	case r.values <- delta:
		return true
	case <-r.done:
		return false
	}
}

func (r *geminiStream) run(ctx context.Context, request lebro.ModelRequest, sequence func(func(*genai.GenerateContentResponse, error) bool)) {
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
			usage = lebro.ModelUsage{InputTokens: int64(result.UsageMetadata.PromptTokenCount), OutputTokens: int64(result.UsageMetadata.CandidatesTokenCount), TotalTokens: int64(result.UsageMetadata.TotalTokenCount)}
		}
		for _, candidate := range result.Candidates {
			if candidate.FinishReason != "" {
				finish = mapFinish(candidate.FinishReason)
			}
			if candidate.Content == nil {
				continue
			}
			for _, part := range candidate.Content.Parts {
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
						r.send(lebro.StreamDelta{Err: &lebro.ModelError{Kind: lebro.ModelErrorMalformedResponse, Provider: providerName, Message: err.Error(), Err: err}})
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
func (r *geminiStream) Next() (lebro.StreamDelta, error) {
	value, ok := <-r.values
	if !ok {
		return lebro.StreamDelta{}, io.EOF
	}
	return value, nil
}
func (r *geminiStream) Close() error {
	r.once.Do(func() {
		close(r.done)
		r.cancel()
	})
	return nil
}
