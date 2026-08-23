package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/httpapi"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

func TestAISDKV4StreamFixture(t *testing.T) {
	model := streamingModel{deltas: []lebro.StreamDelta{
		{Text: "Hel"},
		{Text: "lo", Usage: lebro.ModelUsage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}, FinishReason: lebro.FinishReasonStop},
	}}
	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeAgent(newAgent(t, "assistant", model)))

	recorder := doJSON(t, server.Handler(), http.MethodPost, "/agents/assistant/runs/ai-sdk/stream?version=v4", httpapi.RunRequest{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("content type = %q", got)
	}
	want := "0:\"Hel\"\n0:\"lo\"\nd:{\"finishReason\":\"stop\",\"usage\":{\"completionTokens\":3,\"promptTokens\":2}}\n"
	if got := recorder.Body.String(); got != want {
		t.Fatalf("v4 fixture mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestAISDKV5StreamFixture(t *testing.T) {
	model := streamingModel{deltas: []lebro.StreamDelta{
		{Text: "Hello", StructuredOutput: lebro.NewModelStructuredOutput(json.RawMessage(`{"answer":"world"}`))},
		{FinishReason: lebro.FinishReasonStop, Usage: lebro.ModelUsage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}},
	}}
	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeAgent(newAgent(t, "assistant", model)))

	recorder := doJSON(t, server.Handler(), http.MethodPost, "/agents/assistant/runs/ai-sdk/stream?version=v5", httpapi.RunRequest{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type = %q", got)
	}
	want := strings.Join([]string{
		"data: {\"type\":\"start\"}",
		"data: {\"type\":\"start-step\"}",
		"data: {\"id\":\"text-0\",\"type\":\"text-start\"}",
		"data: {\"delta\":\"Hello\",\"id\":\"text-0\",\"type\":\"text-delta\"}",
		"data: {\"data\":{\"answer\":\"world\"},\"type\":\"data-lebro-structured-output\"}",
		"data: {\"data\":{\"inputTokens\":2,\"outputTokens\":3,\"totalTokens\":5},\"type\":\"data-lebro-usage\"}",
		"data: {\"data\":\"stop\",\"type\":\"data-lebro-finish-reason\"}",
		"data: {\"id\":\"text-0\",\"type\":\"text-end\"}",
		"data: {\"type\":\"finish-step\"}",
		"data: {\"type\":\"finish\"}",
	}, "\n\n") + "\n\n"
	if got := recorder.Body.String(); got != want {
		t.Fatalf("v5 fixture mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestAISDKV4StructuredOutputFixture(t *testing.T) {
	model := streamingModel{deltas: []lebro.StreamDelta{
		{StructuredOutput: lebro.NewModelStructuredOutput(json.RawMessage(`{"answer":"world"}`))},
		{FinishReason: lebro.FinishReasonStop},
	}}
	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeAgent(newAgent(t, "assistant", model)))

	recorder := doJSON(t, server.Handler(), http.MethodPost, "/agents/assistant/runs/ai-sdk/stream?version=v4", httpapi.RunRequest{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body)
	}
	want := "2:[{\"answer\":\"world\"}]\nd:{\"finishReason\":\"stop\",\"usage\":{\"completionTokens\":0,\"promptTokens\":0}}\n"
	if got := recorder.Body.String(); got != want {
		t.Fatalf("v4 structured fixture mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestAISDKStreamMapsToolCallsAndTerminalErrors(t *testing.T) {
	registry := mustValue(lebro.NewToolRegistry(lebrojsonschema.NewCompiler()))
	must(t, registry.Register(echoTool{}))
	calls, err := lebro.NewModelToolCalls(lebro.ModelToolCall{ID: "call-1", ToolID: "echo", Arguments: json.RawMessage(`{"value":"x"}`)})
	must(t, err)
	step := 0
	agent := newAgentWithConfig(t, lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "assistant", Tools: []lebro.ToolID{"echo"}},
		Tools:      registry,
		Model: modelFunc(func(context.Context, lebro.ModelRequest) (lebro.ModelResponse, error) {
			step++
			if step == 1 {
				return lebro.ModelResponse{Message: lebro.Message{Role: lebro.RoleAssistant, ToolCalls: calls}, FinishReason: lebro.FinishReasonToolCalls}, nil
			}
			return textResponse("done"), nil
		}),
	})
	server := httpapi.NewServer(httpapi.ServerConfig{Redactor: httpapi.PassthroughRedactor})
	must(t, server.ExposeAgent(agent))
	recorder := doJSON(t, server.Handler(), http.MethodPost, "/agents/assistant/runs/ai-sdk/stream?version=v5", httpapi.RunRequest{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body)
	}
	body := recorder.Body.String()
	for _, want := range []string{"\"type\":\"tool-input-start\"", "\"type\":\"tool-input-available\"", "\"toolCallId\":\"call-1\"", "\"input\":{\"value\":\"x\"}"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing %s: %s", want, body)
		}
	}

	failed := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, failed.ExposeAgent(newAgent(t, "assistant", failingModel{kind: lebro.ModelErrorUnavailable})))
	recorder = doJSON(t, failed.Handler(), http.MethodPost, "/agents/assistant/runs/ai-sdk/stream?version=v5", httpapi.RunRequest{})
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"type":"error"`) || !strings.Contains(recorder.Body.String(), "model provider failed") {
		t.Fatalf("terminal error = status %d body %s", recorder.Code, recorder.Body)
	}
}

func TestAISDKStreamRequiresKnownVersion(t *testing.T) {
	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeAgent(newAgent(t, "assistant", &scriptedModel{})))
	for _, target := range []string{"/agents/assistant/runs/ai-sdk/stream", "/agents/assistant/runs/ai-sdk/stream?version=v6"} {
		recorder := doJSON(t, server.Handler(), http.MethodPost, target, httpapi.RunRequest{})
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d", target, recorder.Code)
		}
		assertErrorCode(t, recorder, httpapi.ErrorCodeInvalidRequest)
	}
}
