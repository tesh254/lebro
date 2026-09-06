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
	"sync/atomic"
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
	if model.client == nil || model.client.Timeout != 0 {
		t.Fatalf("client timeout = %v, want 0", model.client.Timeout)
	}
	if model.timeout != defaultTimeout {
		t.Fatalf("model timeout = %v, want %v", model.timeout, defaultTimeout)
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

func TestGenerateMapsReasoningAndProtectsReasoningWireFields(t *testing.T) {
	t.Parallel()
	var observed observeRequest
	server := newRecordedServer(t, &observed, func(w http.ResponseWriter, r *http.Request) {
		usage := chatUsageBody{PromptTokens: 3, CompletionTokens: 8, TotalTokens: 11}
		usage.CompletionTokensDetails.ReasoningTokens = 5
		writeJSON(t, w, http.StatusOK, chatResponse{
			Choices: []chatChoice{{Message: chatChoiceMessage{
				Role:             "assistant",
				Content:          json.RawMessage(`"answer"`),
				Reasoning:        "checked constraints",
				ReasoningDetails: json.RawMessage(`[{"type":"reasoning.encrypted","data":"opaque"}]`),
			}, FinishReason: "stop"}},
			Usage: usage,
		})
	})
	model := newAdapter(t, server, Config{APIKey: "secret", Model: "gpt-4o"})

	response, err := model.Generate(context.Background(), lebro.ModelRequest{
		Messages:  []lebro.Message{{Role: lebro.RoleUser, Content: "solve"}},
		Reasoning: lebro.ReasoningConfig{Effort: lebro.ReasoningMedium},
		Extension: json.RawMessage(`{"reasoning":{"effort":"low"},"include_reasoning":false}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Reasoning.Text != "checked constraints" || string(response.Message.Reasoning.Details.Raw()) != `[{"type":"reasoning.encrypted","data":"opaque"}]` {
		t.Fatalf("reasoning = %#v", response.Message.Reasoning)
	}
	if response.Usage.ReasoningTokens != 5 {
		t.Fatalf("usage = %#v", response.Usage)
	}
	body := observed.body(t)
	reasoning, _ := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "medium" {
		t.Fatalf("wire reasoning = %#v", reasoning)
	}
	if _, exists := body["include_reasoning"]; exists {
		t.Fatalf("extension overrode reserved include_reasoning: %#v", body)
	}
}

func TestReasoningRequestsIncludeReasoningOnlyWhenConfigured(t *testing.T) {
	t.Parallel()
	model, err := New(Config{APIKey: "key", Model: "openai/gpt-5", BaseURL: "https://openrouter.ai/api/v1", IncludeReasoning: true})
	if err != nil {
		t.Fatal(err)
	}
	body, err := model.buildRequestBody(lebro.ModelRequest{
		Messages:  []lebro.Message{{Role: lebro.RoleUser, Content: "solve"}},
		Reasoning: lebro.ReasoningConfig{Effort: lebro.ReasoningHigh},
	})
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["include_reasoning"] != true {
		t.Fatalf("configured request = %#v", wire)
	}

	streamBody, err := model.buildStreamingRequestBody(lebro.ModelRequest{
		Messages:  []lebro.Message{{Role: lebro.RoleUser, Content: "solve"}},
		Reasoning: lebro.ReasoningConfig{Effort: lebro.ReasoningHigh},
	})
	if err != nil {
		t.Fatal(err)
	}
	wire = nil
	if err := json.Unmarshal(streamBody, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["include_reasoning"] != true {
		t.Fatalf("configured stream request = %#v", wire)
	}

	// Endpoint detection is explicit: an OpenRouter URL alone must not opt in.
	defaultModel, err := New(Config{APIKey: "key", Model: "openai/gpt-5", BaseURL: "https://openrouter.ai/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	body, err = defaultModel.buildRequestBody(lebro.ModelRequest{
		Messages:  []lebro.Message{{Role: lebro.RoleUser, Content: "solve"}},
		Reasoning: lebro.ReasoningConfig{Effort: lebro.ReasoningHigh},
	})
	if err != nil {
		t.Fatal(err)
	}
	wire = nil
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if _, exists := wire["include_reasoning"]; exists {
		t.Fatalf("unconfigured request must not send include_reasoning: %#v", wire)
	}
}

func TestReasoningReplayRequiresIncludeReasoning(t *testing.T) {
	message := lebro.Message{
		Role:      lebro.RoleAssistant,
		Content:   "answer",
		Reasoning: lebro.ModelReasoning{Text: "check", Details: lebro.NewModelReasoningDetails(json.RawMessage(`[{"type":"reasoning.encrypted","data":"opaque"}]`))},
	}
	for _, test := range []struct {
		name             string
		includeReasoning bool
		wantReasoning    bool
	}{
		{name: "disabled"},
		{name: "enabled", includeReasoning: true, wantReasoning: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			model, err := New(Config{APIKey: "key", Model: "openai/gpt-5", IncludeReasoning: test.includeReasoning})
			if err != nil {
				t.Fatal(err)
			}
			body, err := model.buildRequestBody(lebro.ModelRequest{Messages: []lebro.Message{message}})
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				Messages []map[string]any `json:"messages"`
			}
			if err := json.Unmarshal(body, &wire); err != nil {
				t.Fatal(err)
			}
			_, hasReasoning := wire.Messages[0]["reasoning"]
			_, hasDetails := wire.Messages[0]["reasoning_details"]
			if hasReasoning != test.wantReasoning || hasDetails != test.wantReasoning {
				t.Fatalf("reasoning fields = %#v, want present=%t", wire.Messages[0], test.wantReasoning)
			}
		})
	}
}

func TestOpenAIReasoningReplayIgnoresForeignProviderDetails(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		details lebro.ModelReasoningDetails
		want    string
	}{
		{name: "OpenRouter", details: lebro.NewModelReasoningDetails(json.RawMessage(`[{"type":"reasoning.encrypted","data":"opaque"}]`)), want: `[{"type":"reasoning.encrypted","data":"opaque"}]`},
		{name: "Anthropic", details: lebro.NewModelReasoningDetails(json.RawMessage(`[{"type":"thinking","signature":"opaque"}]`))},
		{name: "Gemini", details: lebro.NewModelReasoningDetails(json.RawMessage(`[{"text":"thought","thought_signature":"b3BhcXVl"}]`))},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := string(openAIReasoningDetails(test.details)); got != test.want {
				t.Fatalf("replay details = %s, want %s", got, test.want)
			}
		})
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
	var serverReached atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		serverReached.Store(true)
	}))
	t.Cleanup(server.Close)
	model := newAdapter(t, server, Config{APIKey: "k", Model: "gpt-4o"})
	modelWithoutDefault := newAdapter(t, server, Config{APIKey: "k"})

	noModel := lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}}
	badExtension := lebro.ModelRequest{Model: "gpt-4o", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}, Extension: json.RawMessage(`[1]`)}

	tests := []struct {
		name    string
		model   *Model
		request lebro.ModelRequest
	}{
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
	if serverReached.Load() {
		t.Fatal("unsupported request reached the server")
	}
}

// TestGenerateRejectsAssistantStructuredOutputHistory covers the one message
// shape chat-completions cannot represent on the wire: an assistant turn whose
// payload was structured output rather than text or tool calls.
func TestGenerateRejectsAssistantStructuredOutputHistory(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, chatResponse{
			Choices: []chatChoice{{Message: chatChoiceMessage{Role: "assistant", Content: json.RawMessage(`"ok"`)}, FinishReason: "stop"}},
		})
	}))
	t.Cleanup(server.Close)
	model := newAdapter(t, server, Config{APIKey: "k", Model: "gpt-4o"})

	tests := []struct {
		name     string
		messages []lebro.Message
	}{
		{"structured output", []lebro.Message{
			{Role: lebro.RoleUser, Content: "hi"},
			{Role: lebro.RoleAssistant, StructuredOutput: lebro.NewModelStructuredOutput(json.RawMessage(`{"ok":true}`))},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := model.Generate(context.Background(), lebro.ModelRequest{Model: "gpt-4o", Messages: test.messages})
			var modelErr *lebro.ModelError
			if !errors.As(err, &modelErr) || modelErr.Kind != lebro.ModelErrorInvalidRequest {
				t.Fatalf("error = %v, want invalid_request", err)
			}
		})
	}
}

func TestGenerateMapsToolCallHistoryToWire(t *testing.T) {
	t.Parallel()
	var observed observeRequest
	server := newRecordedServer(t, &observed, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, chatResponse{
			ID: "chatcmpl-2", Model: "gpt-4o",
			Choices: []chatChoice{{Index: 0, Message: chatChoiceMessage{Role: "assistant", Content: json.RawMessage(`"done"`)}, FinishReason: "stop"}},
		})
	})
	model := newAdapter(t, server, Config{APIKey: "k", Model: "gpt-4o"})

	calls, err := lebro.NewModelToolCalls(
		lebro.ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{"id":"42"}`)},
		lebro.ModelToolCall{ID: "call-2", ToolID: "enrich", Arguments: json.RawMessage(`{"depth":2}`)},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = model.Generate(context.Background(), lebro.ModelRequest{
		Model: "gpt-4o",
		Messages: []lebro.Message{
			{Role: lebro.RoleUser, Content: "look it up"},
			{Role: lebro.RoleAssistant, ToolCalls: calls},
			{Role: lebro.RoleTool, Content: `{"name":"Ada"}`, ToolCallID: "call-1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	body := observed.body(t)
	messages, _ := body["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("wire messages = %#v", body["messages"])
	}
	assistant, _ := messages[1].(map[string]any)
	content, hasContent := assistant["content"]
	if !hasContent || content != nil {
		t.Fatalf("assistant content = %#v, want null", assistant["content"])
	}
	wireCalls, _ := assistant["tool_calls"].([]any)
	if len(wireCalls) != 2 {
		t.Fatalf("assistant tool_calls = %#v", assistant["tool_calls"])
	}
	first, _ := wireCalls[0].(map[string]any)
	if first["id"] != "call-1" || first["type"] != "function" {
		t.Fatalf("tool call[0] = %#v", first)
	}
	function, _ := first["function"].(map[string]any)
	if function["name"] != "lookup" || function["arguments"] != `{"id":"42"}` {
		t.Fatalf("tool call function = %#v", function)
	}
	toolResult, _ := messages[2].(map[string]any)
	if toolResult["role"] != "tool" || toolResult["tool_call_id"] != "call-1" || toolResult["content"] != `{"name":"Ada"}` {
		t.Fatalf("tool result = %#v", toolResult)
	}
}

func TestGenerateMapsToolsAndResponseFormatToWire(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		request lebro.ModelRequest
		assert  func(t *testing.T, body map[string]any)
	}{
		{
			name: "tools",
			request: lebro.ModelRequest{
				Model:    "gpt-4o",
				Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
				Tools: []lebro.ToolDefinition{
					{ID: "lookup", Description: "Finds records", InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}}}`)},
					{ID: "ping"},
				},
			},
			assert: func(t *testing.T, body map[string]any) {
				tools, _ := body["tools"].([]any)
				if len(tools) != 2 {
					t.Fatalf("tools = %#v", body["tools"])
				}
				first, _ := tools[0].(map[string]any)
				if first["type"] != "function" {
					t.Fatalf("tool type = %#v", first)
				}
				function, _ := first["function"].(map[string]any)
				if function["name"] != "lookup" || function["description"] != "Finds records" {
					t.Fatalf("function = %#v", function)
				}
				parameters, _ := function["parameters"].(map[string]any)
				if parameters == nil || parameters["properties"] == nil {
					t.Fatalf("parameters = %#v", function["parameters"])
				}
				second, _ := tools[1].(map[string]any)
				secondFunction, _ := second["function"].(map[string]any)
				schema, _ := secondFunction["parameters"].(map[string]any)
				if schema["type"] != "object" {
					t.Fatalf("default parameters = %#v", secondFunction["parameters"])
				}
			},
		},
		{
			name: "response format",
			request: lebro.ModelRequest{
				Model:    "gpt-4o",
				Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
				OutputSchema: &lebro.ModelOutputSchema{
					Name:        "invoice",
					Description: "Extracted invoice fields",
					Schema:      json.RawMessage(`{"type":"object","required":["invoice_id"]}`),
					Strict:      true,
				},
			},
			assert: func(t *testing.T, body map[string]any) {
				format, _ := body["response_format"].(map[string]any)
				if format["type"] != "json_schema" {
					t.Fatalf("response_format type = %#v", format)
				}
				jsonSchema, _ := format["json_schema"].(map[string]any)
				if jsonSchema["name"] != "invoice" || jsonSchema["strict"] != true || jsonSchema["description"] != "Extracted invoice fields" {
					t.Fatalf("json_schema = %#v", jsonSchema)
				}
				schema, _ := jsonSchema["schema"].(map[string]any)
				if schema == nil || schema["required"] == nil {
					t.Fatalf("schema = %#v", jsonSchema["schema"])
				}
			},
		},
		{
			name: "default schema name",
			request: lebro.ModelRequest{
				Model:        "gpt-4o",
				Messages:     []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
				OutputSchema: &lebro.ModelOutputSchema{Schema: json.RawMessage(`{"type":"object"}`)},
			},
			assert: func(t *testing.T, body map[string]any) {
				format, _ := body["response_format"].(map[string]any)
				jsonSchema, _ := format["json_schema"].(map[string]any)
				if jsonSchema["name"] != "response" {
					t.Fatalf("default name = %#v", jsonSchema["name"])
				}
			},
		},
		{
			name: "extension cannot clobber owned keys",
			request: lebro.ModelRequest{
				Model:        "gpt-4o",
				Messages:     []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
				Tools:        []lebro.ToolDefinition{{ID: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}},
				OutputSchema: &lebro.ModelOutputSchema{Name: "invoice", Schema: json.RawMessage(`{"type":"object"}`), Strict: true},
				Extension:    json.RawMessage(`{"tools":[{"hijacked":true}],"response_format":{"hijacked":true},"temperature":0.7,"tool_choice":"auto"}`),
			},
			assert: func(t *testing.T, body map[string]any) {
				format, _ := body["response_format"].(map[string]any)
				if format["type"] != "json_schema" {
					t.Fatalf("response_format clobbered = %#v", body["response_format"])
				}
				tools, _ := body["tools"].([]any)
				first, _ := tools[0].(map[string]any)
				if first["type"] != "function" {
					t.Fatalf("tools clobbered = %#v", body["tools"])
				}
				if body["temperature"] != 0.7 || body["tool_choice"] != "auto" {
					t.Fatalf("extension passthrough lost: %#v", body)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var observed observeRequest
			server := newRecordedServer(t, &observed, func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, http.StatusOK, chatResponse{
					Choices: []chatChoice{{Message: chatChoiceMessage{Role: "assistant", Content: json.RawMessage(`"{\"ok\":true}"`)}, FinishReason: "stop"}},
				})
			})
			model := newAdapter(t, server, Config{APIKey: "k", Model: "gpt-4o"})
			_, err := model.Generate(context.Background(), test.request)
			if err != nil {
				t.Fatal(err)
			}
			test.assert(t, observed.body(t))
		})
	}
}

func TestGenerateMapsToolCallResponse(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"id": "chatcmpl-3", "model": "gpt-4o",
			"choices": []any{map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []any{
						map[string]any{
							"id": "call-9", "type": "function",
							"function": map[string]any{"name": "lookup", "arguments": `{"id":"42"}`},
						},
						map[string]any{
							"id": "call-10", "type": "function",
							"function": map[string]any{"name": "ping", "arguments": ""},
						},
					},
				},
				"finish_reason": "tool_calls",
			}},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 6, "total_tokens": 11},
		})
	}))
	t.Cleanup(server.Close)
	model := newAdapter(t, server, Config{APIKey: "k", Model: "gpt-4o"})

	response, err := model.Generate(context.Background(), lebro.ModelRequest{
		Model:    "gpt-4o",
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
		Tools:    []lebro.ToolDefinition{{ID: "lookup"}, {ID: "ping"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.FinishReason != lebro.FinishReasonToolCalls {
		t.Fatalf("finish reason = %q, want tool_calls", response.FinishReason)
	}
	calls := response.Message.ToolCalls.Values()
	if len(calls) != 2 {
		t.Fatalf("tool calls = %#v", response.Message.ToolCalls)
	}
	if calls[0].ID != "call-9" || calls[0].ToolID != "lookup" || string(calls[0].Arguments) != `{"id":"42"}` {
		t.Fatalf("call[0] = %#v", calls[0])
	}
	if calls[1].ID != "call-10" || calls[1].ToolID != "ping" || string(calls[1].Arguments) != `{}` {
		t.Fatalf("call[1] = %#v, want empty arguments defaulted to {}", calls[1])
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("validate = %v", err)
	}
}

func TestGenerateRejectsToolCallsFinishWithoutToolCalls(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, chatResponse{
			Choices: []chatChoice{{Message: chatChoiceMessage{Role: "assistant", Content: json.RawMessage(`"text"`)}, FinishReason: "tool_calls"}},
		})
	}))
	t.Cleanup(server.Close)
	model := newAdapter(t, server, Config{APIKey: "k", Model: "gpt-4o"})

	_, err := model.Generate(context.Background(), lebro.ModelRequest{Model: "gpt-4o", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	var modelErr *lebro.ModelError
	if !errors.As(err, &modelErr) || modelErr.Kind != lebro.ModelErrorMalformedResponse {
		t.Fatalf("error = %v, want malformed_response", err)
	}
}

func TestGenerateAttachesStructuredOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		content   json.RawMessage
		wantValid bool
	}{
		{name: "json inside string content", content: json.RawMessage(`"{\"ok\":true}"`), wantValid: true},
		{name: "bare json object content", content: json.RawMessage(`{"ok":true}`), wantValid: true},
		{name: "plain text is not valid output", content: json.RawMessage(`"nope"`), wantValid: false},
		{name: "null content is not valid output", content: json.RawMessage(`null`), wantValid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusOK, chatResponse{
					Choices: []chatChoice{{Message: chatChoiceMessage{Role: "assistant", Content: test.content}, FinishReason: "stop"}},
				})
			}))
			t.Cleanup(server.Close)
			model := newAdapter(t, server, Config{APIKey: "k", Model: "gpt-4o"})

			response, err := model.Generate(context.Background(), lebro.ModelRequest{
				Model:        "gpt-4o",
				Messages:     []lebro.Message{{Role: lebro.RoleUser, Content: "return JSON"}},
				OutputSchema: &lebro.ModelOutputSchema{Name: "result", Schema: json.RawMessage(`{"type":"object"}`), Strict: true},
			})
			if !test.wantValid {
				var modelErr *lebro.ModelError
				if !errors.As(err, &modelErr) || modelErr.Kind != lebro.ModelErrorMalformedResponse {
					t.Fatalf("error = %v, want malformed_response", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if response.Message.StructuredOutput == "" {
				t.Fatal("structured output missing")
			}
			if got, want := string(response.Message.StructuredOutput.Raw()), `{"ok":true}`; got != want {
				t.Fatalf("structured output = %s, want %s", got, want)
			}
			if err := response.Validate(); err != nil {
				t.Fatalf("validate = %v", err)
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
		{name: "malformed text part", body: `{"choices":[{"message":{"role":"assistant","content":[{"type":"text","text":42}]},"finish_reason":"stop"}]}`},
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
	model := newStubModel(t, stubRoundTripper{err: errors.New("connection reset by peer")}, Config{APIKey: "k", Model: "gpt-4o"})

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
	model := newStubModel(t, stubRoundTripper{err: &timeoutNetErr{}}, Config{APIKey: "k", Model: "gpt-4o"})

	_, err := model.Generate(context.Background(), lebro.ModelRequest{Model: "gpt-4o", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	var modelErr *lebro.ModelError
	if !errors.As(err, &modelErr) || modelErr.Kind != lebro.ModelErrorTimeout {
		t.Fatalf("error = %v, want timeout", err)
	}
	if !errors.Is(err, lebro.ErrModelTimeout) {
		t.Fatalf("errors.Is(ErrModelTimeout) = false")
	}
}

func TestGenerateTranslatesMidStreamCancellation(t *testing.T) {
	t.Parallel()
	model := newStubModel(t, stubRoundTripper{
		body: io.NopCloser(&cancelReader{}),
	}, Config{APIKey: "k", Model: "gpt-4o"})

	_, err := model.Generate(context.Background(), lebro.ModelRequest{Model: "gpt-4o", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if !errors.Is(err, context.Canceled) {
		var modelErr *lebro.ModelError
		if errors.As(err, &modelErr) && modelErr.Kind == lebro.ModelErrorTimeout {
			t.Fatalf("error = %v, want context.Canceled not timeout", err)
		}
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestGenerateTranslatesMidStreamTimeout(t *testing.T) {
	t.Parallel()
	model := newStubModel(t, stubRoundTripper{
		body: io.NopCloser(&timeoutReader{}),
	}, Config{APIKey: "k", Model: "gpt-4o"})

	_, err := model.Generate(context.Background(), lebro.ModelRequest{Model: "gpt-4o", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	var modelErr *lebro.ModelError
	if !errors.As(err, &modelErr) || modelErr.Kind != lebro.ModelErrorTimeout {
		t.Fatalf("error = %v, want timeout", err)
	}
}

func TestStreamUsesIdleTimeoutInsteadOfTotalDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		flusher.Flush()
		for _, text := range []string{"a", "b", "c", "d"} {
			time.Sleep(15 * time.Millisecond)
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"`+text+`"}}]}`+"\n\n")
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer server.Close()
	model := newAdapter(t, server, Config{APIKey: "k", Model: "gpt-4o", Timeout: 30 * time.Millisecond})

	reader, err := model.Stream(context.Background(), lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	var text strings.Builder
	for {
		delta, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		text.WriteString(delta.Text)
	}
	if got := text.String(); got != "abcd" {
		t.Fatalf("stream text = %q, want abcd", got)
	}
}

func TestStreamTimesOutWhenIdle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()
	model := newAdapter(t, server, Config{APIKey: "k", Model: "gpt-4o", Timeout: 20 * time.Millisecond})

	reader, err := model.Stream(context.Background(), lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	_, err = reader.Next()
	var modelErr *lebro.ModelError
	if !errors.As(err, &modelErr) || modelErr.Kind != lebro.ModelErrorTimeout {
		t.Fatalf("error = %v, want timeout model error", err)
	}
}

func TestStreamTimesOutWaitingForResponseHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	model := newAdapter(t, server, Config{APIKey: "k", Model: "gpt-4o", Timeout: 20 * time.Millisecond})

	_, err := model.Stream(context.Background(), lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	var modelErr *lebro.ModelError
	if !errors.As(err, &modelErr) || modelErr.Kind != lebro.ModelErrorTimeout {
		t.Fatalf("error = %v, want timeout model error", err)
	}
}

func TestStreamPausesIdleTimeoutBetweenNextCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"first"}}]}`+"\n\n")
		flusher.Flush()
		time.Sleep(45 * time.Millisecond)
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"second"}}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer server.Close()
	model := newAdapter(t, server, Config{APIKey: "k", Model: "gpt-4o", Timeout: 20 * time.Millisecond})

	reader, err := model.Stream(context.Background(), lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	first, err := reader.Next()
	if err != nil || first.Text != "first" {
		t.Fatalf("first delta = %#v, %v", first, err)
	}
	// Simulate a slow stream processor. The stream must only measure time while
	// Next is blocked reading the provider, not consumer processing time.
	time.Sleep(35 * time.Millisecond)
	second, err := reader.Next()
	if err != nil || second.Text != "second" {
		t.Fatalf("second delta = %#v, %v", second, err)
	}
}

func TestStreamHonorsCallerDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	model := newAdapter(t, server, Config{APIKey: "k", Model: "gpt-4o", Timeout: time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	reader, err := model.Stream(ctx, lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	_, err = reader.Next()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
}

func TestGenerateStillHonorsTotalTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	model := newAdapter(t, server, Config{APIKey: "k", Model: "gpt-4o", Timeout: 20 * time.Millisecond})

	_, err := model.Generate(context.Background(), lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	var modelErr *lebro.ModelError
	if !errors.As(err, &modelErr) || modelErr.Kind != lebro.ModelErrorTimeout {
		t.Fatalf("error = %v, want timeout model error", err)
	}
}

func TestGenerateRespectsContextCancellation(t *testing.T) {
	t.Parallel()
	model := newStubModel(t, stubRoundTripper{err: errors.New("should not reach transport")}, Config{APIKey: "k", Model: "gpt-4o"})

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

// newStubModel builds an adapter backed by a stub RoundTripper instead of a
// real HTTP server, making transport/timeout/cancellation tests deterministic
// and independent of OS socket behavior.
func newStubModel(t *testing.T, rt http.RoundTripper, config Config) *Model {
	t.Helper()
	config.HTTPClient = &http.Client{Transport: rt}
	config.BaseURL = "https://stub.local"
	model, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return model
}

// stubRoundTripper returns a fixed response or error without touching the
// network.
type stubRoundTripper struct {
	resp *http.Response
	body io.ReadCloser
	err  error
}

func (s stubRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.resp != nil {
		return s.resp, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       s.body,
	}, nil
}

// timeoutNetErr is a net.Error that always reports a timeout.
type timeoutNetErr struct{}

func (*timeoutNetErr) Error() string   { return "i/o timeout" }
func (*timeoutNetErr) Timeout() bool   { return true }
func (*timeoutNetErr) Temporary() bool { return true }

// cancelReader returns context.Canceled on the first Read.
type cancelReader struct{}

func (*cancelReader) Read([]byte) (int, error) { return 0, context.Canceled }

// timeoutReader returns a net timeout on the first Read.
type timeoutReader struct{}

func (*timeoutReader) Read([]byte) (int, error) { return 0, &timeoutNetErr{} }
