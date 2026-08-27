package geminiapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/tesh254/lebro"
	genai "google.golang.org/genai"
)

// newClientModel builds an adapter backed by a real genai client pointed at a
// fake HTTP server, so Generate and Stream can be exercised end-to-end.
func newClientModel(t *testing.T, provider, model string, handler http.HandlerFunc) (*Model, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
		APIKey:      "key",
		Backend:     genai.BackendGeminiAPI,
		HTTPClient:  server.Client(),
		HTTPOptions: genai.HTTPOptions{BaseURL: server.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	shared, err := New(Config{Provider: provider, Client: client, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	return shared, server
}

func TestNewValidatesClient(t *testing.T) {
	if _, err := New(Config{Provider: "gemini", Client: nil, Model: "m"}); err == nil {
		t.Fatal("New() error = nil, want nil-client error")
	}
	shared, err := New(Config{Provider: "gemini", Client: &genai.Client{}, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if shared.model != "m" || shared.provider != "gemini" {
		t.Fatalf("New = %#v", shared)
	}
}

func TestGenerateRoundTrip(t *testing.T) {
	m, server := newClientModel(t, "gemini", "gemini-2.5-flash", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":"answer"}]},"contentRating":{}}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`))
	})
	defer server.Close()
	resp, err := m.Generate(t.Context(), lebro.ModelRequest{
		Messages: []lebro.Message{
			{Role: lebro.RoleSystem, Content: "be brief"},
			{Role: lebro.RoleUser, Content: "hi"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "answer" || resp.FinishReason != lebro.FinishReasonStop {
		t.Fatalf("response = %#v", resp)
	}
	if resp.Usage.TotalTokens != 3 {
		t.Fatalf("usage = %#v", resp.Usage)
	}
}

func TestGenerateMapsMaxTokensFinishReason(t *testing.T) {
	m, server := newClientModel(t, "gemini", "m", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"finishReason":"MAX_TOKENS","content":{"role":"model","parts":[{"text":"cut"}]},"contentRating":{}}]}`))
	})
	defer server.Close()
	resp, err := m.Generate(t.Context(), lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != lebro.FinishReasonLength {
		t.Fatalf("finish = %v, want length", resp.FinishReason)
	}
}

func TestGenerateMapsSafetyFinishReason(t *testing.T) {
	m, server := newClientModel(t, "gemini", "m", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"finishReason":"SAFETY","content":{"role":"model","parts":[{"text":""}]},"contentRating":{}}]}`))
	})
	defer server.Close()
	resp, err := m.Generate(t.Context(), lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != lebro.FinishReasonContent {
		t.Fatalf("finish = %v, want content", resp.FinishReason)
	}
}

func TestGenerateMapsNotFoundAndServerErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		code     int
		wantKind lebro.ModelErrorKind
	}{
		{"not_found", http.StatusNotFound, lebro.ModelErrorNotFound},
		{"server_error", http.StatusInternalServerError, lebro.ModelErrorUnavailable},
		{"forbidden", http.StatusForbidden, lebro.ModelErrorPermissionDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, server := newClientModel(t, "gemini", "m", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(`{"error":{"code":` + strconv.Itoa(tc.code) + `,"status":"ERR","message":"x"}}`))
			})
			defer server.Close()
			_, err := m.Generate(t.Context(), lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
			var modelErr *lebro.ModelError
			if !errors.As(err, &modelErr) || modelErr.Kind != tc.wantKind {
				t.Fatalf("error = %#v, want kind %v", err, tc.wantKind)
			}
		})
	}
}

func TestGenerateMapsNetworkTimeout(t *testing.T) {
	m, server := newClientModel(t, "gemini", "m", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	})
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	_, err := m.Generate(ctx, lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %#v, want context.DeadlineExceeded", err)
	}
}

func TestGenerateRejectsEmptyModel(t *testing.T) {
	m := &Model{provider: "gemini"}
	_, err := m.Generate(t.Context(), lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	var modelErr *lebro.ModelError
	if !errors.As(err, &modelErr) || modelErr.Kind != lebro.ModelErrorInvalidRequest {
		t.Fatalf("error = %#v, want invalid", err)
	}
}

func TestGenerateMapsMalformedStructuredOutput(t *testing.T) {
	m, server := newClientModel(t, "gemini", "m", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":"not json"}]},"contentRating":{}}]}`))
	})
	defer server.Close()
	_, err := m.Generate(t.Context(), lebro.ModelRequest{
		OutputSchema: &lebro.ModelOutputSchema{Schema: json.RawMessage(`{"type":"object"}`)},
		Messages:     []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
	})
	var modelErr *lebro.ModelError
	if !errors.As(err, &modelErr) || modelErr.Kind != lebro.ModelErrorMalformedResponse {
		t.Fatalf("error = %#v, want malformed", err)
	}
}

func TestStreamRoundTrip(t *testing.T) {
	m, server := newClientModel(t, "gemini", "m", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"a\"}]},\"contentRating\":{}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"finishReason\":\"STOP\",\"content\":{\"role\":\"model\",\"parts\":[{}]},\"contentRating\":{}}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}\n\n"))
		w.(http.Flusher).Flush()
	})
	defer server.Close()
	reader, err := m.Stream(t.Context(), lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	var terminal lebro.StreamDelta
	for {
		delta, err := reader.Next()
		if err != nil {
			break
		}
		text += delta.Text
		if delta.FinishReason != "" {
			terminal = delta
		}
	}
	_ = reader.Close()
	if text != "a" {
		t.Fatalf("text = %q", text)
	}
	if terminal.FinishReason != lebro.FinishReasonStop || terminal.Usage.TotalTokens != 2 {
		t.Fatalf("terminal = %#v", terminal)
	}
}

func TestStreamMapsErrors(t *testing.T) {
	m, server := newClientModel(t, "gemini", "m", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"slow"}}`))
	})
	defer server.Close()
	reader, err := m.Stream(t.Context(), lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	var streamErr error
	for {
		delta, err := reader.Next()
		if err != nil {
			break
		}
		if delta.Err != nil {
			streamErr = delta.Err
			break
		}
	}
	var modelErr *lebro.ModelError
	if !errors.As(streamErr, &modelErr) || modelErr.Kind != lebro.ModelErrorRateLimited {
		t.Fatalf("stream error = %#v, want rate-limited", streamErr)
	}
}

func TestStreamCloseUnblocksPendingSend(t *testing.T) {
	m, server := newClientModel(t, "gemini", "m", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	defer server.Close()
	reader, err := m.Stream(t.Context(), lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	for {
		_, err := reader.Next()
		if err != nil {
			break
		}
	}
}

func TestStreamStructuredOutputTerminal(t *testing.T) {
	m, server := newClientModel(t, "gemini", "m", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"{\\\"ok\\\":true}\"}]},\"contentRating\":{}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"finishReason\":\"STOP\",\"content\":{\"role\":\"model\",\"parts\":[{}]},\"contentRating\":{}}]}\n\n"))
		w.(http.Flusher).Flush()
	})
	defer server.Close()
	reader, err := m.Stream(t.Context(), lebro.ModelRequest{
		OutputSchema: &lebro.ModelOutputSchema{Schema: json.RawMessage(`{"type":"object"}`)},
		Messages:     []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	var terminal lebro.StreamDelta
	for {
		delta, err := reader.Next()
		if err != nil {
			break
		}
		if delta.FinishReason != "" {
			terminal = delta
		}
	}
	if string(terminal.StructuredOutput) != `{"ok":true}` {
		t.Fatalf("structured = %s", terminal.StructuredOutput)
	}
}

func TestStreamStructuredOutputInvalidJSONFailsLoudly(t *testing.T) {
	m, server := newClientModel(t, "vertexai", "m", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"not json\"}]},\"contentRating\":{}}]}\n\n"))
		w.(http.Flusher).Flush()
	})
	defer server.Close()
	reader, err := m.Stream(t.Context(), lebro.ModelRequest{
		OutputSchema: &lebro.ModelOutputSchema{Schema: json.RawMessage(`{"type":"object"}`)},
		Messages:     []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	var streamErr error
	for {
		delta, err := reader.Next()
		if err != nil {
			break
		}
		if delta.Err != nil {
			streamErr = delta.Err
			break
		}
	}
	var modelErr *lebro.ModelError
	if !errors.As(streamErr, &modelErr) || modelErr.Kind != lebro.ModelErrorMalformedResponse {
		t.Fatalf("stream error = %#v, want malformed", streamErr)
	}
	if modelErr.Provider != "vertexai" || modelErr.Message != "lebro: vertexai structured output is not valid JSON" {
		t.Fatalf("error = %#v", modelErr)
	}
}

func TestStreamToolCallTerminal(t *testing.T) {
	m, server := newClientModel(t, "gemini", "m", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"functionCall\":{\"id\":\"c1\",\"name\":\"lookup\",\"args\":{\"q\":\"x\"}}}]},\"contentRating\":{}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"finishReason\":\"STOP\",\"content\":{\"role\":\"model\",\"parts\":[{}]},\"contentRating\":{}}]}\n\n"))
		w.(http.Flusher).Flush()
	})
	defer server.Close()
	reader, err := m.Stream(t.Context(), lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	var terminal lebro.StreamDelta
	for {
		delta, err := reader.Next()
		if err != nil {
			break
		}
		if delta.FinishReason != "" {
			terminal = delta
		}
		if delta.ToolCall != nil {
			if delta.ToolCall.ToolID != "lookup" {
				t.Fatalf("tool call = %#v", delta.ToolCall)
			}
		}
	}
	if terminal.FinishReason != lebro.FinishReasonToolCalls {
		t.Fatalf("terminal finish = %v, want tool-calls", terminal.FinishReason)
	}
}

func TestGenerateWithToolsAndToolResults(t *testing.T) {
	m, server := newClientModel(t, "gemini", "m", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":"done"}]},"contentRating":{}}]}`))
	})
	defer server.Close()
	calls, err := lebro.NewModelToolCalls(lebro.ModelToolCall{ID: "call-1", ToolID: "weather", Arguments: json.RawMessage(`{"city":"Nairobi"}`)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Generate(t.Context(), lebro.ModelRequest{
		Messages: []lebro.Message{
			{Role: lebro.RoleUser, Content: "weather?"},
			{Role: lebro.RoleAssistant, ToolCalls: calls},
			{Role: lebro.RoleTool, ToolCallID: "call-1", Content: "24C"},
		},
		Tools: []lebro.ToolDefinition{{ID: "weather", Description: "lookup weather", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGenerateWithReasoning(t *testing.T) {
	m, server := newClientModel(t, "gemini", "gemini-2.5-flash", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":"thinking","thought":true},{"text":"answer"}]},"contentRating":{}}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"thoughtsTokenCount":5,"totalTokenCount":8}}`))
	})
	defer server.Close()
	resp, err := m.Generate(t.Context(), lebro.ModelRequest{
		Reasoning: lebro.ReasoningConfig{Effort: lebro.ReasoningHigh},
		Messages:  []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Reasoning.Text != "thinking" || resp.Usage.ReasoningTokens != 5 {
		t.Fatalf("response = %#v", resp)
	}
}

func TestGenerateMapsDeadlineExceeded(t *testing.T) {
	m := &Model{provider: "gemini"}
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	if !errors.Is(m.error(ctx, context.DeadlineExceeded), context.DeadlineExceeded) {
		t.Fatal("deadline was not preserved")
	}
}

func TestGenerateMapsUnknownError(t *testing.T) {
	m := &Model{provider: "gemini"}
	err := m.error(context.Background(), errors.New("something broke"))
	var modelErr *lebro.ModelError
	if !errors.As(err, &modelErr) || modelErr.Kind != lebro.ModelErrorUnavailable {
		t.Fatalf("error = %#v, want unavailable", err)
	}
}
