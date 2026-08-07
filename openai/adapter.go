package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tesh254/lebro"
)

const (
	providerName     = "openai"
	defaultBaseURL   = "https://api.openai.com/v1"
	defaultTimeout   = 60 * time.Second
	defaultUserAgent = "lebro-openai"
	chatCompletions  = "/chat/completions"
)

// Config describes an OpenAI-compatible chat-completions endpoint. BaseURL,
// HTTPClient, Timeout, and UserAgent default to sensible values when zero.
type Config struct {
	// BaseURL is the API root, for example "https://api.openai.com/v1". It
	// defaults to the public OpenAI endpoint.
	BaseURL string
	// APIKey is sent as the Bearer token. Required.
	APIKey string
	// Model is the default model id used when a request omits ModelRequest.Model.
	Model string
	// HTTPClient issues requests. If nil a client with Timeout is used.
	HTTPClient *http.Client
	// Timeout caps each request when no earlier deadline is set on the context.
	// A zero value uses defaultTimeout when HTTPClient is also nil.
	Timeout time.Duration
	// UserAgent overrides the default User-Agent header.
	UserAgent string
	// Organization sets the optional OpenAI-Beta / Organization header.
	Organization string
}

// Model is a [lebro.Model] backed by an OpenAI-compatible chat-completions
// endpoint. Use [New] to create instances.
type Model struct {
	baseURL      string
	apiKey       string
	model        string
	client       *http.Client
	timeout      time.Duration
	userAgent    string
	organization string
}

var _ lebro.Model = (*Model)(nil)

// New builds a text-generation adapter for an OpenAI-compatible endpoint.
// The returned model is safe for concurrent use.
func New(config Config) (*Model, error) {
	if config.APIKey == "" {
		return nil, errors.New("lebro: API key is required")
	}
	baseURL := config.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("lebro: invalid base URL: %w", err)
	}
	if !parsed.IsAbs() {
		return nil, fmt.Errorf("lebro: base URL %q must be absolute", baseURL)
	}

	client := config.HTTPClient
	if client == nil {
		timeout := config.Timeout
		if timeout == 0 {
			timeout = defaultTimeout
		}
		client = &http.Client{Timeout: timeout}
	}

	userAgent := config.UserAgent
	if userAgent == "" {
		userAgent = defaultUserAgent
	}

	return &Model{
		baseURL:      strings.TrimRight(baseURL, "/"),
		apiKey:       config.APIKey,
		model:        config.Model,
		client:       client,
		timeout:      config.Timeout,
		userAgent:    userAgent,
		organization: config.Organization,
	}, nil
}

// Generate sends a text completion request to the chat-completions endpoint
// and maps the response to the neutral model protocol. Failures are returned
// as [*lebro.ModelError] with a normalized kind; context cancellation is
// returned directly so errors.Is(err, context.Canceled) holds.
func (m *Model) Generate(ctx context.Context, request lebro.ModelRequest) (lebro.ModelResponse, error) {
	if err := request.Validate(); err != nil {
		return lebro.ModelResponse{}, m.invalidRequest(err.Error(), err)
	}
	if len(request.Tools) > 0 || request.OutputSchema != nil {
		return lebro.ModelResponse{}, m.invalidRequest("lebro: text-generation adapter does not support tools or structured output", nil)
	}

	body, err := m.buildRequestBody(request)
	if err != nil {
		return lebro.ModelResponse{}, err
	}

	reqCtx, cancel := m.requestContext(ctx)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, m.baseURL+chatCompletions, bytes.NewReader(body))
	if err != nil {
		return lebro.ModelResponse{}, m.invalidRequest(err.Error(), err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+m.apiKey)
	httpReq.Header.Set("User-Agent", m.userAgent)
	if m.organization != "" {
		httpReq.Header.Set("OpenAI-Organization", m.organization)
	}

	resp, err := m.client.Do(httpReq)
	if err != nil {
		return lebro.ModelResponse{}, m.classifyTransportError(reqCtx, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusBadRequest {
		return lebro.ModelResponse{}, m.classifyResponseError(resp)
	}

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return lebro.ModelResponse{}, m.transportError("read response body", err)
	}

	var parsed chatResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return lebro.ModelResponse{}, m.malformedResponse(fmt.Sprintf("lebro: decode response: %v", err), err)
	}

	response, err := m.mapResponse(parsed)
	if err != nil {
		return lebro.ModelResponse{}, m.malformedResponse(err.Error(), err)
	}
	if err := response.Validate(); err != nil {
		return lebro.ModelResponse{}, m.malformedResponse(err.Error(), err)
	}
	return response, nil
}

func (m *Model) buildRequestBody(request lebro.ModelRequest) ([]byte, error) {
	model := request.Model
	if model == "" {
		model = m.model
	}
	if model == "" {
		return nil, m.invalidRequest("lebro: model is required", nil)
	}

	messages := make([]chatMessage, 0, len(request.Messages))
	for _, message := range request.Messages {
		mapped, err := mapMessage(message)
		if err != nil {
			return nil, m.invalidRequest(err.Error(), err)
		}
		messages = append(messages, mapped)
	}

	body := map[string]any{"model": model, "messages": messages}
	if len(request.Extension) > 0 {
		var extension map[string]any
		if err := json.Unmarshal(request.Extension, &extension); err != nil {
			return nil, m.invalidRequest(fmt.Sprintf("lebro: request extension must be a JSON object: %v", err), err)
		}
		for key, value := range extension {
			// model and messages are owned by the neutral protocol; keep
			// callers from clobbering the wire representation.
			if key == "model" || key == "messages" {
				continue
			}
			body[key] = value
		}
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, m.invalidRequest(fmt.Sprintf("lebro: encode request: %v", err), err)
	}
	return encoded, nil
}

func (m *Model) requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if m.timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, m.timeout)
}

func (m *Model) mapResponse(parsed chatResponse) (lebro.ModelResponse, error) {
	if len(parsed.Choices) == 0 {
		return lebro.ModelResponse{}, errors.New("lebro: response has no choices")
	}
	choice := parsed.Choices[0]
	content, err := extractTextContent(choice.Message.Content)
	if err != nil {
		return lebro.ModelResponse{}, err
	}

	response := lebro.ModelResponse{
		Message:      lebro.Message{Role: lebro.RoleAssistant, Content: content},
		Usage:        lebro.ModelUsage{InputTokens: parsed.Usage.PromptTokens, OutputTokens: parsed.Usage.CompletionTokens, TotalTokens: parsed.Usage.TotalTokens},
		FinishReason: mapFinishReason(choice.FinishReason),
	}
	if parsed.ID != "" || parsed.Model != "" {
		extension, _ := json.Marshal(map[string]string{"openai_id": parsed.ID, "openai_model": parsed.Model})
		response.Extension = extension
	}
	return response, nil
}

func (m *Model) classifyTransportError(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
		return m.timeoutError("request deadline exceeded", err)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return m.timeoutError("request timed out", err)
	}
	return m.transportError("HTTP request failed", err)
}

func (m *Model) classifyResponseError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	var parsed chatErrorBody
	_ = json.Unmarshal(body, &parsed)

	modelErr := &lebro.ModelError{
		Kind:       statusToKind(resp.StatusCode),
		Provider:   providerName,
		Code:       parsed.Error.Code,
		StatusCode: resp.StatusCode,
		Message:    parsed.Error.Message,
		Err:        nil,
	}
	if modelErr.Message == "" {
		modelErr.Message = fmt.Sprintf("lebro: HTTP %d", resp.StatusCode)
	}
	if parsed.Error.Type != "" || parsed.Error.Param != "" {
		extension, _ := json.Marshal(map[string]string{"type": parsed.Error.Type, "param": parsed.Error.Param})
		modelErr.Extension = extension
	}
	if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After")); retryAfter > 0 {
		modelErr.RetryAfter = retryAfter
	}
	return modelErr
}

func (m *Model) invalidRequest(message string, err error) error {
	return &lebro.ModelError{Kind: lebro.ModelErrorInvalidRequest, Provider: providerName, Message: message, Err: err}
}

func (m *Model) malformedResponse(message string, err error) error {
	return &lebro.ModelError{Kind: lebro.ModelErrorMalformedResponse, Provider: providerName, Message: message, Err: err}
}

func (m *Model) transportError(message string, err error) error {
	return &lebro.ModelError{Kind: lebro.ModelErrorTransport, Provider: providerName, Message: message, Err: err}
}

func (m *Model) timeoutError(message string, err error) error {
	return &lebro.ModelError{Kind: lebro.ModelErrorTimeout, Provider: providerName, Message: message, Err: err}
}

type chatMessage struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	Name       string `json:"name,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type chatResponse struct {
	ID      string        `json:"id"`
	Model   string        `json:"model"`
	Choices []chatChoice  `json:"choices"`
	Usage   chatUsageBody `json:"usage"`
}

type chatChoice struct {
	Index        int               `json:"index"`
	Message      chatChoiceMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

type chatChoiceMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type chatUsageBody struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type chatErrorBody struct {
	Error chatError `json:"error"`
}

type chatError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
	Param   string `json:"param"`
}

func mapMessage(message lebro.Message) (chatMessage, error) {
	role, err := mapRole(message.Role)
	if err != nil {
		return chatMessage{}, err
	}
	out := chatMessage{Role: role, Content: message.Content, Name: message.Name}
	if message.Role == lebro.RoleTool {
		if message.ToolCallID == "" {
			return chatMessage{}, errors.New("lebro: tool message requires a tool call id")
		}
		out.ToolCallID = message.ToolCallID
	} else if message.ToolCallID != "" {
		return chatMessage{}, errors.New("lebro: only tool messages may carry a tool call id")
	}
	return out, nil
}

func mapRole(role lebro.Role) (string, error) {
	switch role {
	case lebro.RoleSystem:
		return "system", nil
	case lebro.RoleUser:
		return "user", nil
	case lebro.RoleAssistant:
		return "assistant", nil
	case lebro.RoleTool:
		return "tool", nil
	default:
		return "", fmt.Errorf("lebro: unsupported message role %q", role)
	}
}

func mapFinishReason(reason string) lebro.FinishReason {
	switch reason {
	case "stop":
		return lebro.FinishReasonStop
	case "length":
		return lebro.FinishReasonLength
	case "content_filter":
		return lebro.FinishReasonContent
	default:
		// tool_calls / function_call and unknown reasons are not surfaced as
		// tool calls by this text adapter; treat them as unspecified so the
		// neutral response stays valid without inventing tool calls.
		return lebro.FinishReasonUnspecified
	}
}

func extractTextContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString, nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, part := range parts {
			var kind string
			if raw, ok := part["type"]; ok {
				_ = json.Unmarshal(raw, &kind)
			}
			if kind != "text" {
				continue
			}
			if raw, ok := part["text"]; ok {
				var text string
				_ = json.Unmarshal(raw, &text)
				b.WriteString(text)
			}
		}
		return b.String(), nil
	}
	return "", errors.New("lebro: unsupported content shape")
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

func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return time.Duration(seconds) * time.Second
	}
	if moment, err := http.ParseTime(value); err == nil {
		if d := time.Until(moment); d > 0 {
			return d
		}
	}
	return 0
}
