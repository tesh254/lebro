package openai

import (
	"bufio"
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
	"sync"
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
	// HTTPClient issues requests. If it sets Timeout, that end-to-end deadline
	// also applies to streams and takes precedence over the idle timeout below.
	HTTPClient *http.Client
	// Timeout caps non-streaming requests and is the maximum period a stream may
	// remain idle. A zero value uses defaultTimeout when HTTPClient is also nil.
	// Stream lifetime is otherwise controlled by the caller's context deadline.
	Timeout time.Duration
	// UserAgent overrides the default User-Agent header.
	UserAgent string
	// Organization sets the optional OpenAI-Beta / Organization header.
	Organization string
	// IncludeReasoning sends include_reasoning alongside the reasoning object
	// so OpenRouter-style endpoints return reasoning text and details.
	// Endpoint detection is deliberately explicit: gateways and proxies in
	// front of such endpoints would otherwise silently lose reasoning output.
	IncludeReasoning bool
}

// Model is a [lebro.Model] backed by an OpenAI-compatible chat-completions
// endpoint. Use [New] to create instances.
type Model struct {
	baseURL          string
	apiKey           string
	model            string
	client           *http.Client
	timeout          time.Duration
	userAgent        string
	organization     string
	includeReasoning bool
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

	timeout := config.Timeout
	client := config.HTTPClient
	if client == nil {
		if timeout == 0 {
			timeout = defaultTimeout
		}
		// An http.Client timeout is an end-to-end deadline, including SSE body
		// reads. Keep it unset so active streams are bounded only by inactivity.
		client = &http.Client{}
	}

	userAgent := config.UserAgent
	if userAgent == "" {
		userAgent = defaultUserAgent
	}

	return &Model{
		baseURL:          strings.TrimRight(baseURL, "/"),
		apiKey:           config.APIKey,
		model:            config.Model,
		client:           client,
		timeout:          timeout,
		userAgent:        userAgent,
		organization:     config.Organization,
		includeReasoning: config.IncludeReasoning,
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
		return lebro.ModelResponse{}, m.classifyResponseError(reqCtx, resp)
	}

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return lebro.ModelResponse{}, m.classifyTransportError(reqCtx, err)
	}

	var parsed chatResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return lebro.ModelResponse{}, m.malformedResponse(fmt.Sprintf("lebro: decode response: %v", err), err)
	}

	response, err := m.mapResponse(request, parsed)
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
		mapped, err := m.mapMessage(message)
		if err != nil {
			return nil, m.invalidRequest(err.Error(), err)
		}
		messages = append(messages, mapped)
	}

	body := map[string]any{"model": model, "messages": messages}
	if len(request.Tools) > 0 {
		body["tools"] = chatTools(request.Tools)
	}
	if request.OutputSchema != nil {
		format, err := chatResponseFormat(request.OutputSchema)
		if err != nil {
			return nil, m.invalidRequest(err.Error(), err)
		}
		body["response_format"] = format
	}
	if err := m.applyReasoning(body, request); err != nil {
		return nil, m.invalidRequest(err.Error(), err)
	}
	if len(request.Extension) > 0 {
		var extension map[string]any
		if err := json.Unmarshal(request.Extension, &extension); err != nil {
			return nil, m.invalidRequest(fmt.Sprintf("lebro: request extension must be a JSON object: %v", err), err)
		}
		for key, value := range extension {
			// Keys the adapter derives from the neutral protocol are owned by
			// the mapping; keep callers from clobbering the wire representation.
			if reservedWireKey(key) {
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

// chatTools maps neutral tool definitions to chat-completions function tools.
// A definition without an input schema becomes a schemaless object parameter,
// matching what the provider accepts.
func chatTools(tools []lebro.ToolDefinition) []map[string]any {
	mapped := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		parameters := json.RawMessage(tool.InputSchema)
		if len(parameters) == 0 {
			parameters = json.RawMessage(`{"type":"object"}`)
		}
		function := map[string]any{"name": string(tool.ID), "parameters": parameters}
		if tool.Description != "" {
			function["description"] = tool.Description
		}
		mapped = append(mapped, map[string]any{"type": "function", "function": function})
	}
	return mapped
}

// chatResponseFormat maps a neutral output schema onto the JSON-schema
// response format. Strict mode and schema restrictions remain enforced by the
// endpoint; lebro keeps validating the returned value locally.
func chatResponseFormat(schema *lebro.ModelOutputSchema) (map[string]any, error) {
	name := schema.Name
	if name == "" {
		name = "response"
	}
	jsonSchema := map[string]any{
		"name":   name,
		"schema": json.RawMessage(schema.Schema),
		"strict": schema.Strict,
	}
	if schema.Description != "" {
		jsonSchema["description"] = schema.Description
	}
	return map[string]any{"type": "json_schema", "json_schema": jsonSchema}, nil
}

// applyReasoning maps neutral reasoning intent onto the wire body shared by
// generate and stream requests.
func (m *Model) applyReasoning(body map[string]any, request lebro.ModelRequest) error {
	if request.Reasoning.IsZero() {
		return nil
	}
	reasoning, err := chatReasoning(request.Reasoning)
	if err != nil {
		return err
	}
	body["reasoning"] = reasoning
	if m.includeReasoning && request.Reasoning.Effort != lebro.ReasoningOff {
		body["include_reasoning"] = true
	}
	return nil
}

// chatReasoning maps neutral reasoning intent to the OpenAI-compatible
// reasoning object used by OpenRouter and modern compatible endpoints. The
// endpoint remains responsible for model-specific capability checks.
func chatReasoning(config lebro.ReasoningConfig) (map[string]any, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if config.BudgetTokens > 0 {
		return map[string]any{"max_tokens": config.BudgetTokens}, nil
	}
	effort := string(config.Effort)
	if config.Effort == lebro.ReasoningOff {
		effort = "none"
	}
	return map[string]any{"effort": effort}, nil
}

func chatMessageReasoning(text string, details json.RawMessage) lebro.ModelReasoning {
	reasoning := lebro.ModelReasoning{Text: text}
	if len(details) > 0 && string(details) != "null" {
		reasoning.Details = lebro.NewModelReasoningDetails(details)
	}
	return reasoning
}

// reservedWireKeys are request-body keys derived from the neutral protocol.
// Extension fields may add anything else, but these stay owned by the mapping
// so validated intent cannot drift from the wire representation.
var reservedWireKeys = map[string]struct{}{
	"model":             {},
	"messages":          {},
	"stream":            {},
	"tools":             {},
	"response_format":   {},
	"reasoning":         {},
	"include_reasoning": {},
}

func reservedWireKey(key string) bool {
	_, reserved := reservedWireKeys[key]
	return reserved
}

func (m *Model) requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if m.timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, m.timeout)
}

func (m *Model) mapResponse(request lebro.ModelRequest, parsed chatResponse) (lebro.ModelResponse, error) {
	if len(parsed.Choices) == 0 {
		return lebro.ModelResponse{}, errors.New("lebro: response has no choices")
	}
	choice := parsed.Choices[0]
	content, contentErr := extractTextContent(choice.Message.Content)

	message := lebro.Message{Role: lebro.RoleAssistant, Content: content}
	message.Reasoning = chatMessageReasoning(choice.Message.Reasoning, choice.Message.ReasoningDetails)
	calls, err := mapToolCalls(choice.Message.ToolCalls)
	if err != nil {
		return lebro.ModelResponse{}, m.malformedResponse(err.Error(), nil)
	}
	if len(calls) > 0 {
		encoded, err := lebro.NewModelToolCalls(calls...)
		if err != nil {
			return lebro.ModelResponse{}, m.malformedResponse(err.Error(), err)
		}
		message.ToolCalls = encoded
	}
	if err := m.attachStructuredOutput(request, choice, &message, content, contentErr); err != nil {
		return lebro.ModelResponse{}, err
	}

	response := lebro.ModelResponse{
		Message: message,
		Usage: lebro.ModelUsage{
			InputTokens: parsed.Usage.PromptTokens, OutputTokens: parsed.Usage.CompletionTokens,
			ReasoningTokens: parsed.Usage.CompletionTokensDetails.ReasoningTokens, TotalTokens: parsed.Usage.TotalTokens,
		},
		FinishReason: mapFinishReason(choice.FinishReason),
	}
	if parsed.ID != "" || parsed.Model != "" {
		extension, _ := json.Marshal(map[string]string{"openai_id": parsed.ID, "openai_model": parsed.Model})
		response.Extension = extension
	}
	if err := response.Validate(); err != nil {
		return lebro.ModelResponse{}, m.malformedResponse(err.Error(), nil)
	}
	return response, nil
}

// attachStructuredOutput records the structured payload on a terminal message
// when the run requested an output schema and the model answered without tool
// calls. The standard chat-completions shape carries JSON inside a string
// content field; some OpenAI-compatible endpoints instead return a bare JSON
// value, which is accepted here because lebro validates the value locally
// regardless of how it arrived. Any other shape is a malformed response.
func (m *Model) attachStructuredOutput(request lebro.ModelRequest, choice chatChoice, message *lebro.Message, text string, textErr error) error {
	if request.OutputSchema == nil || len(message.ToolCalls.Values()) > 0 {
		if textErr != nil {
			return m.malformedResponse(textErr.Error(), textErr)
		}
		return nil
	}
	if textErr == nil && json.Valid([]byte(text)) {
		message.StructuredOutput = lebro.NewModelStructuredOutput(json.RawMessage(text))
		return nil
	}
	if textErr != nil && len(choice.Message.Content) > 0 && json.Valid(choice.Message.Content) {
		message.StructuredOutput = lebro.NewModelStructuredOutput(choice.Message.Content)
		return nil
	}
	return m.malformedResponse("lebro: structured output is not valid JSON", nil)
}

// mapToolCalls converts wire tool calls into neutral values. Arguments default
// to an empty object when the provider sends none; anything else must be valid
// JSON because the canonical ModelToolCalls encoding requires it.
func mapToolCalls(raw []chatToolCall) ([]lebro.ModelToolCall, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	calls := make([]lebro.ModelToolCall, 0, len(raw))
	for _, toolCall := range raw {
		if toolCall.ID == "" {
			return nil, errors.New("lebro: tool call is missing an id")
		}
		if toolCall.Function.Name == "" {
			return nil, fmt.Errorf("lebro: tool call %q is missing a function name", toolCall.ID)
		}
		arguments := strings.TrimSpace(toolCall.Function.Arguments)
		if arguments == "" {
			arguments = "{}"
		}
		if !json.Valid([]byte(arguments)) {
			return nil, fmt.Errorf("lebro: tool call %q arguments are not valid JSON", toolCall.ID)
		}
		calls = append(calls, lebro.ModelToolCall{ID: toolCall.ID, ToolID: lebro.ToolID(toolCall.Function.Name), Arguments: json.RawMessage(arguments)})
	}
	return calls, nil
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

// classifyResponseError maps an HTTP error response to a normalized
// *lebro.ModelError.
//
// A body read that fails because the request was canceled or its deadline
// elapsed is classified as cancellation or timeout rather than as the HTTP
// status. The status is real, but the caller asked to stop, so reporting a
// retryable server error would both break errors.Is(err, context.Canceled) and
// invite a retry of a request nobody is waiting for.
func (m *Model) classifyResponseError(ctx context.Context, resp *http.Response) error {
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil && (ctx.Err() != nil || errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded)) {
		return m.classifyTransportError(ctx, readErr)
	}
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
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	Name             string          `json:"name,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ToolCalls        []chatToolCall  `json:"tool_calls,omitempty"`
	Reasoning        string          `json:"reasoning,omitempty"`
	ReasoningDetails json.RawMessage `json:"reasoning_details,omitempty"`
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
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	ToolCalls        []chatToolCall  `json:"tool_calls,omitempty"`
	Reasoning        string          `json:"reasoning,omitempty"`
	ReasoningDetails json.RawMessage `json:"reasoning_details,omitempty"`
}

// chatToolCall is the shared function-call shape used by assistant request
// messages (id, type, function) and by response messages.
type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type,omitempty"`
	Function chatToolFunction `json:"function"`
}

type chatToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatUsageBody struct {
	PromptTokens            int64 `json:"prompt_tokens"`
	CompletionTokens        int64 `json:"completion_tokens"`
	TotalTokens             int64 `json:"total_tokens"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
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

func (m *Model) mapMessage(message lebro.Message) (chatMessage, error) {
	role, err := mapRole(message.Role)
	if err != nil {
		return chatMessage{}, err
	}
	if message.StructuredOutput != "" {
		return chatMessage{}, errors.New("lebro: assistant structured output is not representable in a chat-completions request")
	}
	// Assistant turns that only request tool calls carry a null content on the
	// wire; every other non-empty text becomes a JSON string.
	content := json.RawMessage(`null`)
	if message.Content != "" {
		encoded, err := json.Marshal(message.Content)
		if err != nil {
			return chatMessage{}, fmt.Errorf("lebro: encode message content: %w", err)
		}
		content = encoded
	}
	out := chatMessage{Role: role, Content: content, Name: message.Name}
	if m.includeReasoning {
		out.Reasoning = message.Reasoning.Text
		if details := openAIReasoningDetails(message.Reasoning.Details); len(details) > 0 {
			out.ReasoningDetails = details
		}
	}
	for _, call := range message.ToolCalls.Values() {
		out.ToolCalls = append(out.ToolCalls, chatToolCall{ID: call.ID, Type: "function", Function: chatToolFunction{Name: string(call.ToolID), Arguments: string(call.Arguments)}})
	}
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

// openAIReasoningDetails replays only the OpenRouter-compatible opaque format.
// A shared transcript may contain a prior Anthropic or Gemini turn; forwarding
// its provider state to a chat-completions endpoint can invalidate the next
// request. Compatible details retain their original bytes unchanged.
func openAIReasoningDetails(details lebro.ModelReasoningDetails) json.RawMessage {
	if details == "" {
		return nil
	}
	var values []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(details.Raw(), &values); err != nil || len(values) == 0 {
		return nil
	}
	for _, value := range values {
		if !strings.HasPrefix(value.Type, "reasoning.") {
			return nil
		}
	}
	return details.Raw()
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
	case "tool_calls", "function_call":
		return lebro.FinishReasonToolCalls
	default:
		// Unknown reasons are surfaced as unspecified so the neutral response
		// stays valid without inventing semantics the provider did not send.
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
				if err := json.Unmarshal(raw, &text); err != nil {
					return "", fmt.Errorf("lebro: invalid text content part: %w", err)
				}
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

// Stream sends a streaming chat-completions request and returns a StreamReader
// that delivers ordered text deltas as they arrive from the provider. The
// reader honors context cancellation and closes the underlying HTTP response
// body when Close is called. Failures are returned as [*lebro.ModelError] with
// normalized kinds, matching Generate.
func (m *Model) Stream(ctx context.Context, request lebro.ModelRequest) (lebro.StreamReader, error) {
	if err := request.Validate(); err != nil {
		return nil, m.invalidRequest(err.Error(), err)
	}

	body, err := m.buildStreamingRequestBody(request)
	if err != nil {
		return nil, err
	}

	// Streaming uses an inactivity watchdog instead of requestContext's total
	// deadline. The caller's context deadline remains an absolute ceiling.
	reqCtx, cancel := context.WithCancel(ctx)

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, m.baseURL+chatCompletions, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, m.invalidRequest(err.Error(), err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+m.apiKey)
	httpReq.Header.Set("User-Agent", m.userAgent)
	httpReq.Header.Set("Cache-Control", "no-store")
	if m.organization != "" {
		httpReq.Header.Set("OpenAI-Organization", m.organization)
	}

	type result struct {
		resp *http.Response
		err  error
	}
	resultCh := make(chan result)
	go func() {
		resp, err := m.client.Do(httpReq)
		select {
		case resultCh <- result{resp: resp, err: err}:
		case <-reqCtx.Done():
			if resp != nil {
				_ = resp.Body.Close()
			}
		}
	}()

	var response result
	if m.timeout > 0 {
		timer := time.NewTimer(m.timeout)
		defer timer.Stop()
		select {
		case response = <-resultCh:
		case <-timer.C:
			cancel()
			return nil, m.timeoutError("stream response header timeout exceeded", context.DeadlineExceeded)
		case <-ctx.Done():
			cancel()
			return nil, m.classifyTransportError(ctx, ctx.Err())
		}
	} else {
		select {
		case response = <-resultCh:
		case <-ctx.Done():
			cancel()
			return nil, m.classifyTransportError(ctx, ctx.Err())
		}
	}
	resp, err := response.resp, response.err
	if err != nil {
		cancel()
		return nil, m.classifyTransportError(reqCtx, err)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		classified := m.classifyResponseError(reqCtx, resp)
		_ = resp.Body.Close()
		cancel()
		return nil, classified
	}

	reader := newSSEStreamReader(resp, ctx, cancel, m, request.OutputSchema)
	return reader, nil
}

func (m *Model) buildStreamingRequestBody(request lebro.ModelRequest) ([]byte, error) {
	model := request.Model
	if model == "" {
		model = m.model
	}
	if model == "" {
		return nil, m.invalidRequest("lebro: model is required", nil)
	}

	messages := make([]chatMessage, 0, len(request.Messages))
	for _, message := range request.Messages {
		mapped, err := m.mapMessage(message)
		if err != nil {
			return nil, m.invalidRequest(err.Error(), err)
		}
		messages = append(messages, mapped)
	}

	body := map[string]any{"model": model, "messages": messages, "stream": true}
	if len(request.Tools) > 0 {
		body["tools"] = chatTools(request.Tools)
	}
	if request.OutputSchema != nil {
		format, err := chatResponseFormat(request.OutputSchema)
		if err != nil {
			return nil, m.invalidRequest(err.Error(), err)
		}
		body["response_format"] = format
	}
	if err := m.applyReasoning(body, request); err != nil {
		return nil, m.invalidRequest(err.Error(), err)
	}
	if len(request.Extension) > 0 {
		var extension map[string]any
		if err := json.Unmarshal(request.Extension, &extension); err != nil {
			return nil, m.invalidRequest(fmt.Sprintf("lebro: request extension must be a JSON object: %v", err), err)
		}
		for key, value := range extension {
			if reservedWireKey(key) {
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

// sseStreamReader parses the Server-Sent Events response from the OpenAI
// chat-completions streaming endpoint into ordered lebro.StreamDelta values.
// Text is delivered as it arrives. Streaming tool calls arrive as fragments
// spread across events, so they accumulate until the terminal finish reason
// and are then emitted as complete ToolCall deltas ahead of it, mirroring how
// the runtime consumes complete calls.
type sseStreamReader struct {
	model           *Model
	resp            *http.Response
	body            io.ReadCloser
	scanner         *bufio.Scanner
	callerContext   context.Context
	cancel          context.CancelFunc
	closed          chan struct{}
	once            sync.Once
	mu              sync.Mutex
	idleTimeout     time.Duration
	watchdogControl chan bool
	watchdogDone    chan struct{}
	watchdogOnce    sync.Once
	idleExpired     bool
	terminal        bool
	id              string
	modelName       string
	outputSchema    *lebro.ModelOutputSchema

	textBuf      strings.Builder
	usage        lebro.ModelUsage
	pendingTools map[int]*streamToolBuilder
	toolOrder    []int
	pending      []lebro.StreamDelta
}

// streamToolBuilder accumulates one streamed tool call from its wire
// fragments: the first fragment carries id and function name, later fragments
// append argument JSON by index.
type streamToolBuilder struct {
	id   string
	name string
	args strings.Builder
}

func newSSEStreamReader(resp *http.Response, callerContext context.Context, cancel context.CancelFunc, model *Model, outputSchema *lebro.ModelOutputSchema) *sseStreamReader {
	body := resp.Body
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	reader := &sseStreamReader{
		model:           model,
		resp:            resp,
		body:            body,
		scanner:         scanner,
		callerContext:   callerContext,
		cancel:          cancel,
		closed:          make(chan struct{}),
		idleTimeout:     model.timeout,
		watchdogControl: make(chan bool),
		watchdogDone:    make(chan struct{}),
		outputSchema:    outputSchema,
	}
	go reader.watchIdle()
	return reader
}

func (r *sseStreamReader) Next() (lebro.StreamDelta, error) {
	r.mu.Lock()
	if r.terminal {
		r.mu.Unlock()
		return lebro.StreamDelta{}, io.EOF
	}
	r.mu.Unlock()

	for {
		select {
		case <-r.closed:
			r.markTerminal()
			return lebro.StreamDelta{}, context.Canceled
		default:
		}
		if callerErr := r.callerContext.Err(); callerErr != nil {
			r.markTerminal()
			return lebro.StreamDelta{}, callerErr
		}
		if delta, ok := r.popPending(); ok {
			if delta.IsTerminal() {
				r.markTerminal()
			}
			return delta, nil
		}
		r.setWatchdog(true)
		scanned := r.scanner.Scan()
		r.setWatchdog(false)
		if callerErr := r.callerContext.Err(); callerErr != nil {
			r.markTerminal()
			return lebro.StreamDelta{}, callerErr
		}
		if !scanned {
			err := r.scanner.Err()
			if r.idleTimedOut() {
				r.markTerminal()
				return lebro.StreamDelta{}, r.model.timeoutError("stream idle timeout exceeded", err)
			}
			if err == nil {
				r.markTerminal()
				return lebro.StreamDelta{FinishReason: lebro.FinishReasonUnspecified}, nil
			}
			if errors.Is(err, context.Canceled) {
				r.markTerminal()
				return lebro.StreamDelta{}, err
			}
			if errors.Is(err, context.DeadlineExceeded) {
				r.markTerminal()
				return lebro.StreamDelta{}, r.model.timeoutError("stream deadline exceeded", err)
			}
			r.markTerminal()
			return lebro.StreamDelta{}, r.model.transportError("stream read failed", err)
		}
		line := r.scanner.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			r.markTerminal()
			return lebro.StreamDelta{}, io.EOF
		}
		var event chatStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			r.markTerminal()
			return lebro.StreamDelta{}, r.model.malformedResponse(fmt.Sprintf("lebro: decode stream event: %v", err), err)
		}
		delta, emit, err := r.handleEvent(event)
		if err != nil {
			r.markTerminal()
			return lebro.StreamDelta{}, err
		}
		if !emit {
			continue
		}
		if delta.IsTerminal() {
			r.markTerminal()
		}
		return delta, nil
	}
}

// popPending hands out queued deltas in order. The queue holds complete tool
// calls followed by their shared terminal delta after a "tool_calls" finish
// reason, so consumers see whole calls before the stream ends.
func (r *sseStreamReader) popPending() (lebro.StreamDelta, bool) {
	if len(r.pending) == 0 {
		return lebro.StreamDelta{}, false
	}
	delta := r.pending[0]
	r.pending = r.pending[1:]
	return delta, true
}

// handleEvent folds one wire event into the neutral stream. A false emit means
// the event produced no consumer-visible delta yet (fragment accumulation, or
// its deltas were queued); a true emit returns the next delta to deliver.
// Events that combine content with a finish reason — common on final chunks —
// queue the text ahead of the terminal delta so ordering is preserved.
func (r *sseStreamReader) handleEvent(event chatStreamEvent) (lebro.StreamDelta, bool, error) {
	if event.Error != nil {
		return lebro.StreamDelta{}, false, r.classifyStreamError(event.Error)
	}
	if event.ID != "" {
		r.id = event.ID
	}
	if event.Model != "" {
		r.modelName = event.Model
	}
	if event.Usage != (chatUsageBody{}) {
		r.usage = lebro.ModelUsage{
			InputTokens:     event.Usage.PromptTokens,
			OutputTokens:    event.Usage.CompletionTokens,
			ReasoningTokens: event.Usage.CompletionTokensDetails.ReasoningTokens,
			TotalTokens:     event.Usage.TotalTokens,
		}
	}
	if len(event.Choices) == 0 {
		if event.Usage != (chatUsageBody{}) {
			return lebro.StreamDelta{Usage: r.usage}, true, nil
		}
		return lebro.StreamDelta{}, false, nil
	}
	choice := event.Choices[0]
	if reasoning := chatMessageReasoning(choice.Delta.ReasoningText, choice.Delta.ReasoningDetails); !reasoning.IsZero() {
		r.pending = append(r.pending, lebro.StreamDelta{Reasoning: reasoning})
	}
	if choice.Delta.Content != "" {
		r.textBuf.WriteString(choice.Delta.Content)
		r.pending = append(r.pending, lebro.StreamDelta{Text: choice.Delta.Content})
	}
	r.accumulateToolFragments(choice.Delta.ToolCalls)
	if choice.FinishReason == "" {
		if len(r.pending) == 0 {
			return lebro.StreamDelta{}, false, nil
		}
		delta := r.pending[0]
		r.pending = r.pending[1:]
		return delta, true, nil
	}
	finish := mapFinishReason(choice.FinishReason)
	if finish == lebro.FinishReasonToolCalls {
		calls, err := r.completeToolCalls()
		if err != nil {
			return lebro.StreamDelta{}, false, r.model.malformedResponse(err.Error(), nil)
		}
		for i := range calls {
			call := calls[i]
			r.pending = append(r.pending, lebro.StreamDelta{ToolCall: &call})
		}
	}
	terminal := lebro.StreamDelta{FinishReason: finish, Usage: r.usage}
	if finish != lebro.FinishReasonToolCalls && r.outputSchema != nil {
		if text := r.textBuf.String(); text != "" && json.Valid([]byte(text)) {
			terminal.StructuredOutput = lebro.NewModelStructuredOutput(json.RawMessage(text))
		}
	}
	r.pending = append(r.pending, terminal)
	return lebro.StreamDelta{}, false, nil
}

func (r *sseStreamReader) accumulateToolFragments(fragments []chatStreamToolFragment) {
	for _, fragment := range fragments {
		builder := r.pendingTools[fragment.Index]
		if builder == nil {
			builder = &streamToolBuilder{}
			if r.pendingTools == nil {
				r.pendingTools = map[int]*streamToolBuilder{}
			}
			r.pendingTools[fragment.Index] = builder
			r.toolOrder = append(r.toolOrder, fragment.Index)
		}
		if fragment.ID != "" {
			builder.id = fragment.ID
		}
		if fragment.Function != nil {
			if fragment.Function.Name != "" {
				builder.name = fragment.Function.Name
			}
			builder.args.WriteString(fragment.Function.Arguments)
		}
	}
}

func (r *sseStreamReader) completeToolCalls() ([]lebro.ModelToolCall, error) {
	calls := make([]lebro.ModelToolCall, 0, len(r.toolOrder))
	for _, index := range r.toolOrder {
		builder := r.pendingTools[index]
		if builder.id == "" {
			return nil, fmt.Errorf("lebro: streamed tool call at index %d is missing an id", index)
		}
		if builder.name == "" {
			return nil, fmt.Errorf("lebro: streamed tool call %q is missing a function name", builder.id)
		}
		arguments := strings.TrimSpace(builder.args.String())
		if arguments == "" {
			arguments = "{}"
		}
		if !json.Valid([]byte(arguments)) {
			return nil, fmt.Errorf("lebro: streamed tool call %q arguments are not valid JSON", builder.id)
		}
		calls = append(calls, lebro.ModelToolCall{ID: builder.id, ToolID: lebro.ToolID(builder.name), Arguments: json.RawMessage(arguments)})
	}
	return calls, nil
}

func (r *sseStreamReader) Close() error {
	r.once.Do(func() {
		close(r.closed)
		r.stopWatchdog()
		r.cancel()
		_ = r.body.Close()
	})
	return nil
}

func (r *sseStreamReader) markTerminal() {
	r.mu.Lock()
	r.terminal = true
	r.mu.Unlock()
	r.stopWatchdog()
}

func (r *sseStreamReader) setWatchdog(active bool) {
	if r.idleTimeout <= 0 {
		return
	}
	select {
	case <-r.watchdogDone:
	case r.watchdogControl <- active:
	}
}

func (r *sseStreamReader) stopWatchdog() {
	r.watchdogOnce.Do(func() { close(r.watchdogDone) })
}

func (r *sseStreamReader) idleTimedOut() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.idleExpired
}

func (r *sseStreamReader) watchIdle() {
	if r.idleTimeout <= 0 {
		return
	}
	var timer *time.Timer
	var timerC <-chan time.Time
	for {
		select {
		case <-r.watchdogDone:
			if timer != nil {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}
			return
		case active := <-r.watchdogControl:
			if !active {
				if timer != nil {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
						if !r.expireIdle() {
							r.stopWatchdog()
							return
						}
						r.stopWatchdog()
						return
					}
				}
				timerC = nil
				continue
			}
			if timer == nil {
				timer = time.NewTimer(r.idleTimeout)
			} else {
				timer.Reset(r.idleTimeout)
			}
			timerC = timer.C
		case <-timerC:
			if !r.expireIdle() {
				r.stopWatchdog()
				return
			}
			r.stopWatchdog()
			return
		}
	}
}

func (r *sseStreamReader) expireIdle() bool {
	if r.callerContext.Err() != nil {
		return false
	}
	r.mu.Lock()
	r.idleExpired = true
	r.mu.Unlock()
	r.cancel()
	_ = r.body.Close()
	return true
}

func (r *sseStreamReader) classifyStreamError(errBody *chatError) error {
	modelErr := &lebro.ModelError{
		Kind:     streamErrorKind(errBody.Type),
		Provider: providerName,
		Code:     errBody.Code,
		Message:  errBody.Message,
	}
	if modelErr.Message == "" {
		modelErr.Message = "lebro: stream error"
	}
	if errBody.Type != "" || errBody.Param != "" {
		extension, _ := json.Marshal(map[string]string{"type": errBody.Type, "param": errBody.Param})
		modelErr.Extension = extension
	}
	return modelErr
}

func streamErrorKind(errType string) lebro.ModelErrorKind {
	switch errType {
	case "rate_limit_exceeded":
		return lebro.ModelErrorRateLimited
	case "invalid_request_error":
		return lebro.ModelErrorInvalidRequest
	case "authentication_error":
		return lebro.ModelErrorAuthentication
	case "permission_denied", "forbidden":
		return lebro.ModelErrorPermissionDenied
	case "not_found":
		return lebro.ModelErrorNotFound
	case "timeout":
		return lebro.ModelErrorTimeout
	case "server_error", "unavailable", "server_unavailable":
		return lebro.ModelErrorUnavailable
	default:
		return lebro.ModelErrorUnavailable
	}
}

type chatStreamEvent struct {
	ID      string             `json:"id"`
	Model   string             `json:"model"`
	Choices []chatStreamChoice `json:"choices"`
	Usage   chatUsageBody      `json:"usage"`
	Error   *chatError         `json:"error,omitempty"`
}

type chatStreamChoice struct {
	Index        int             `json:"index"`
	Delta        chatStreamDelta `json:"delta"`
	FinishReason string          `json:"finish_reason"`
}

type chatStreamDelta struct {
	Role             string                   `json:"role,omitempty"`
	Content          string                   `json:"content,omitempty"`
	ReasoningText    string                   `json:"reasoning,omitempty"`
	ReasoningDetails json.RawMessage          `json:"reasoning_details,omitempty"`
	ToolCalls        []chatStreamToolFragment `json:"tool_calls,omitempty"`
}

// chatStreamToolFragment is one incremental piece of a streamed tool call.
// Fragments for the same call share an Index; the first carries the id and
// function name, later fragments append argument JSON.
type chatStreamToolFragment struct {
	Index    int                              `json:"index"`
	ID       string                           `json:"id,omitempty"`
	Type     string                           `json:"type,omitempty"`
	Function *chatToolFunctionWithPartialArgs `json:"function,omitempty"`
}

type chatToolFunctionWithPartialArgs struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}
