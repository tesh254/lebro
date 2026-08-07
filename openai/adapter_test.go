package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tesh254/lebro"
)

func TestNewValidatesConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		config Config
		want   string
	}{
		{name: "missing api key", config: Config{Model: "gpt-4o"}, want: "API key is required"},
		{name: "non-absolute base url", config: Config{APIKey: "k", BaseURL: "/v1"}, want: "absolute"},
		{name: "invalid base url", config: Config{APIKey: "k", BaseURL: "://x"}, want: "invalid base URL"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(test.config)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("New(%#v) error = %v, want %q", test.config, err, test.want)
			}
		})
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	t.Parallel()
	model, err := New(Config{APIKey: "k", Model: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	if model.baseURL != defaultBaseURL {
		t.Fatalf("baseURL = %q, want %q", model.baseURL, defaultBaseURL)
	}
	if model.client == nil || model.client.Timeout != defaultTimeout {
		t.Fatalf("client timeout = %v, want %v", model.client.Timeout, defaultTimeout)
	}
	if model.userAgent != defaultUserAgent {
		t.Fatalf("userAgent = %q, want %q", model.userAgent, defaultUserAgent)
	}
}

func TestGenerateMapsTextCompletion(t *testing.T) {
	t.Parallel()
	var observed observeRequest
	server := newRecordedServer(t, &observed, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, chatResponse{
			ID: "chatcmpl-1", Model: "gpt-4o",
			Choices: []chatChoice{{Index: 0, Message: chatChoiceMessage{Role: "assistant", Content: json.RawMessage(`"hello back"`)}, FinishReason: "stop"}},
			Usage:   chatUsageBody{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
		})
	})
	model := newAdapter(t, server, Config{APIKey: "secret", Model: "gpt-4o"})

	response, err := model.Generate(context.Background(), lebro.ModelRequest{
		Model:    "contract-model",
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Role != lebro.RoleAssistant || response.Message.Content != "hello back" {
		t.Fatalf("response message = %#v", response.Message)
	}
	if response.FinishReason != lebro.FinishReasonStop {
		t.Fatalf("finish reason = %q, want stop", response.FinishReason)
	}
	if response.Usage != (lebro.ModelUsage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}) {
		t.Fatalf("usage = %#v", response.Usage)
	}
	if string(response.Extension) != `{"openai_id":"chatcmpl-1","openai_model":"gpt-4o"}` {
		t.Fatalf("extension = %s", response.Extension)
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("response validate = %v", err)
	}

	body := observed.body(t)
	if got := body["model"]; got != "contract-model" {
		t.Fatalf("wire model = %v, want contract-model", got)
	}
	if auth := observed.headers.Get("Authorization"); auth != "Bearer secret" {
		t.Fatalf("Authorization = %q, want Bearer secret", auth)
	}
	if ct := observed.headers.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	messages, _ := body["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("wire messages = %#v", body["messages"])
	}
	first, _ := messages[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "hello" {
		t.Fatalf("wire message[0] = %#v", first)
	}
}

func TestGenerateUsesDefaultModelAndOrganizationHeader(t *testing.T) {
	t.Parallel()
	var observed observeRequest
	server := newRecordedServer(t, &observed, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, chatResponse{
			Choices: []chatChoice{{Message: chatChoiceMessage{Role: "assistant", Content: json.RawMessage(`"ok"`)}, FinishReason: "stop"}},
		})
	})
	model := newAdapter(t, server, Config{APIKey: "k", Model: "default-model", Organization: "org-1"})

	_, err := model.Generate(context.Background(), lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := observed.body(t)["model"]; got != "default-model" {
		t.Fatalf("wire model = %v, want default-model", got)
	}
	if org := observed.headers.Get("OpenAI-Organization"); org != "org-1" {
		t.Fatalf("OpenAI-Organization = %q, want org-1", org)
	}
}

func TestGenerateMergesExtensionFields(t *testing.T) {
	t.Parallel()
	var observed observeRequest
	server := newRecordedServer(t, &observed, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, chatResponse{
			Choices: []chatChoice{{Message: chatChoiceMessage{Role: "assistant", Content: json.RawMessage(`"ok"`)}, FinishReason: "stop"}},
		})
	})
	model := newAdapter(t, server, Config{APIKey: "k", Model: "gpt-4o"})

	_, err := model.Generate(context.Background(), lebro.ModelRequest{
		Model:     "gpt-4o",
		Messages:  []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
		Extension: json.RawMessage(`{"temperature":0.5,"max_tokens":128,"seed":7,"model":"ignored"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	body := observed.body(t)
	if got := body["temperature"]; got != 0.5 {
		t.Fatalf("temperature = %v, want 0.5", got)
	}
	if got := body["max_tokens"]; got != float64(128) {
		t.Fatalf("max_tokens = %v, want 128", got)
	}
	if got := body["seed"]; got != float64(7) {
		t.Fatalf("seed = %v, want 7", got)
	}
	if got := body["model"]; got != "gpt-4o" {
		t.Fatalf("model override = %v, want gpt-4o", got)
	}
}

func TestGenerateRejectsUnsupportedRequests(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("unsupported request reached the server")
	}))
	t.Cleanup(server.Close)
	model := newAdapter(t, server, Config{APIKey: "k", Model: "gpt-4o"})
	modelWithoutDefault := newAdapter(t, server, Config{APIKey: "k"})

	tools := lebro.ModelRequest{Model: "gpt-4o", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}, Tools: []lebro.ToolDefinition{{ID: "lookup"}}}
	structured := lebro.ModelRequest{Model: "gpt-4o", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}, OutputSchema: &lebro.ModelOutputSchema{Name: "result", Schema: json.RawMessage(`{"type":"object"}`)}}
	noModel := lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}}
	badExtension := lebro.ModelRequest{Model: "gpt-4o", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}, Extension: json.RawMessage(`[1]`)}

	tests := []struct {
		name    string
		model   *Model
		request lebro.ModelRequest
	}{
		{"tools", model, tools},
		{"structured output", model, structured},
		{"no model", modelWithoutDefault, noModel},
		{"bad extension", model, badExtension},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.model.Generate(context.Background(), test.request)
			var modelErr *lebro.ModelError
			if !errors.As(err, &modelErr) || modelErr.Kind != lebro.ModelErrorInvalidRequest {
				t.Fatalf("error = %v, want invalid_request", err)
			}
		})
	}
}

func TestGenerateMapsFinishReasonsAndContentShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		reason  string
		content json.RawMessage
		want    lebro.FinishReason
		text    string
	}{
		{name: "stop", reason: "stop", content: json.RawMessage(`"done"`), want: lebro.FinishReasonStop, text: "done"},
		{name: "length", reason: "length", content: json.RawMessage(`"cut"`), want: lebro.FinishReasonLength, text: "cut"},
		{name: "content_filter", reason: "content_filter", content: json.RawMessage(`"filtered"`), want: lebro.FinishReasonContent, text: "filtered"},
		{name: "unknown", reason: "vendor_reason", content: json.RawMessage(`"x"`), want: lebro.FinishReasonUnspecified, text: "x"},
		{name: "tool_calls coerced", reason: "tool_calls", content: json.RawMessage(`"text"`), want: lebro.FinishReasonUnspecified, text: "text"},
		{name: "null content", reason: "stop", content: json.RawMessage(`null`), want: lebro.FinishReasonStop, text: ""},
		{name: "multimodal text parts", reason: "stop", content: json.RawMessage(`[{"type":"text","text":"alpha"},{"type":"image_url"},{"type":"text","text":"beta"}]`), want: lebro.FinishReasonStop, text: "alphabeta"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusOK, chatResponse{
					Choices: []chatChoice{{Message: chatChoiceMessage{Role: "assistant", Content: test.content}, FinishReason: test.reason}},
				})
			}))
			t.Cleanup(server.Close)
			model := newAdapter(t, server, Config{APIKey: "k", Model: "gpt-4o"})
			response, err := model.Generate(context.Background(), lebro.ModelRequest{Model: "gpt-4o", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
			if err != nil {
				t.Fatal(err)
			}
			if response.FinishReason != test.want {
				t.Fatalf("finish reason = %q, want %q", response.FinishReason, test.want)
			}
			if response.Message.Content != test.text {
				t.Fatalf("content = %q, want %q", response.Message.Content, test.text)
			}
			if err := response.Validate(); err != nil {
				t.Fatalf("validate = %v", err)
			}
		})
	}
}

func TestGenerateTranslatesHTTPFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		status     int
		headers    map[string]string
		body       string
		wantKind   lebro.ModelErrorKind
		wantCode   string
		wantStatus int
	}{
		{name: "authentication", status: http.StatusUnauthorized, body: `{"error":{"message":"bad key","type":"invalid_request_error"}}`, wantKind: lebro.ModelErrorAuthentication, wantStatus: 401},
		{name: "permission denied", status: http.StatusForbidden, body: `{"error":{"message":"forbidden","type":"forbidden"}}`, wantKind: lebro.ModelErrorPermissionDenied, wantStatus: 403},
		{name: "not found", status: http.StatusNotFound, body: `{"error":{"message":"no model","type":"not_found"}}`, wantKind: lebro.ModelErrorNotFound, wantStatus: 404},
		{name: "invalid request", status: http.StatusBadRequest, body: `{"error":{"message":"bad","type":"invalid_request_error","code":"bad_value"}}`, wantKind: lebro.ModelErrorInvalidRequest, wantCode: "bad_value", wantStatus: 400},
		{name: "rate limited", status: http.StatusTooManyRequests, headers: map[string]string{"Retry-After": "12"}, body: `{"error":{"message":"slow down","type":"rate_limit"}}`, wantKind: lebro.ModelErrorRateLimited, wantStatus: 429},
		{name: "unavailable 500", status: http.StatusInternalServerError, body: `{"error":{"message":"boom"}}`, wantKind: lebro.ModelErrorUnavailable, wantStatus: 500},
		{name: "unavailable 503", status: http.StatusServiceUnavailable, body: ``, wantKind: lebro.ModelErrorUnavailable, wantStatus: 503},
		{name: "unknown status", status: 600, body: `{"error":{"message":"teapot"}}`, wantKind: lebro.ModelErrorUnknown, wantStatus: 600},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for key, value := range test.headers {
					w.Header().Set(key, value)
				}
				if test.body == "" {
					w.WriteHeader(test.status)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			t.Cleanup(server.Close)
			model := newAdapter(t, server, Config{APIKey: "k", Model: "gpt-4o"})

			_, err := model.Generate(context.Background(), lebro.ModelRequest{Model: "gpt-4o", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
			var modelErr *lebro.ModelError
			if !errors.As(err, &modelErr) {
				t.Fatalf("error = %v, want *lebro.ModelError", err)
			}
			if modelErr.Kind != test.wantKind {
				t.Fatalf("kind = %q, want %q", modelErr.Kind, test.wantKind)
			}
			if modelErr.StatusCode != test.wantStatus {
				t.Fatalf("status = %d, want %d", modelErr.StatusCode, test.wantStatus)
			}
			if test.wantCode != "" && modelErr.Code != test.wantCode {
				t.Fatalf("code = %q, want %q", modelErr.Code, test.wantCode)
			}
			if test.name == "rate limited" && modelErr.RetryAfter != 12*time.Second {
				t.Fatalf("RetryAfter = %v, want 12s", modelErr.RetryAfter)
			}
			if test.wantKind == lebro.ModelErrorAuthentication && !errors.Is(err, lebro.ErrModelAuthentication) {
				t.Fatalf("errors.Is(ErrModelAuthentication) = false")
			}
			if test.wantKind == lebro.ModelErrorRateLimited {
				if !errors.Is(err, lebro.ErrModelRateLimited) {
					t.Fatalf("errors.Is(ErrModelRateLimited) = false")
				}
				if !modelErr.Retryable() {
					t.Fatalf("rate limited should be retryable")
				}
			}
			if test.wantKind == lebro.ModelErrorUnavailable && !modelErr.Retryable() {
				t.Fatalf("unavailable should be retryable")
			}
		})
	}
}

func TestGenerateTranslatesMalformedResponses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{name: "invalid json", body: `{not json`},
		{name: "no choices", body: `{"id":"x","choices":[]}`},
		{name: "unsupported content", body: `{"choices":[{"message":{"role":"assistant","content":42},"finish_reason":"stop"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, test.body)
			}))
			t.Cleanup(server.Close)
			model := newAdapter(t, server, Config{APIKey: "k", Model: "gpt-4o"})

			_, err := model.Generate(context.Background(), lebro.ModelRequest{Model: "gpt-4o", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
			var modelErr *lebro.ModelError
			if !errors.As(err, &modelErr) || modelErr.Kind != lebro.ModelErrorMalformedResponse {
				t.Fatalf("error = %v, want malformed_response", err)
			}
			if !errors.Is(err, lebro.ErrModelMalformedResponse) {
				t.Fatalf("errors.Is(ErrModelMalformedResponse) = false")
			}
		})
	}
}

func TestGenerateTranslatesTransportFailure(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("transport case should not reach the server")
	}))
	server.CloseClientConnections()
	server.Close()
	t.Cleanup(func() {})
	model := newAdapter(t, server, Config{APIKey: "k", Model: "gpt-4o", HTTPClient: server.Client()})

	_, err := model.Generate(context.Background(), lebro.ModelRequest{Model: "gpt-4o", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	var modelErr *lebro.ModelError
	if !errors.As(err, &modelErr) || modelErr.Kind != lebro.ModelErrorTransport {
		t.Fatalf("error = %v, want transport", err)
	}
	if !errors.Is(err, lebro.ErrModelTransport) {
		t.Fatalf("errors.Is(ErrModelTransport) = false")
	}
}

func TestGenerateTranslatesTimeout(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
	}))
	t.Cleanup(server.Close)
	model := newAdapter(t, server, Config{APIKey: "k", Model: "gpt-4o", Timeout: 5 * time.Millisecond})

	_, err := model.Generate(context.Background(), lebro.ModelRequest{Model: "gpt-4o", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	var modelErr *lebro.ModelError
	if !errors.As(err, &modelErr) || modelErr.Kind != lebro.ModelErrorTimeout {
		t.Fatalf("error = %v, want timeout", err)
	}
	if !errors.Is(err, lebro.ErrModelTimeout) {
		t.Fatalf("errors.Is(ErrModelTimeout) = false")
	}
}

func TestGenerateRespectsContextCancellation(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
	}))
	t.Cleanup(server.Close)
	model := newAdapter(t, server, Config{APIKey: "k", Model: "gpt-4o"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := model.Generate(ctx, lebro.ModelRequest{Model: "gpt-4o", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "empty", value: "", want: 0},
		{name: "seconds", value: "30", want: 30 * time.Second},
		{name: "whitespace seconds", value: "  12 ", want: 12 * time.Second},
		{name: "invalid", value: "soon", want: 0},
		{name: "past date", value: "Mon, 01 Jan 0001 00:00:00 GMT", want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := parseRetryAfter(test.value); got != test.want {
				t.Fatalf("parseRetryAfter(%q) = %v, want %v", test.value, got, test.want)
			}
		})
	}
}

type observeRequest struct {
	mu      sync.Mutex
	headers http.Header
	raw     []byte
}

func (o *observeRequest) capture(r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.headers = r.Header.Clone()
	body, _ := io.ReadAll(r.Body)
	o.raw = append([]byte(nil), body...)
	_ = r.Body.Close()
}

func (o *observeRequest) body(t *testing.T) map[string]any {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	var parsed map[string]any
	if err := json.Unmarshal(o.raw, &parsed); err != nil {
		t.Fatalf("unmarshal observed body %q: %v", o.raw, err)
	}
	return parsed
}

func newRecordedServer(t *testing.T, observed *observeRequest, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	wrapped := func(w http.ResponseWriter, r *http.Request) {
		observed.capture(r)
		handler(w, r)
	}
	server := httptest.NewServer(http.HandlerFunc(wrapped))
	t.Cleanup(server.Close)
	return server
}

func newAdapter(t *testing.T, server *httptest.Server, config Config) *Model {
	t.Helper()
	if config.HTTPClient == nil {
		config.HTTPClient = server.Client()
	}
	config.BaseURL = server.URL
	model, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
