package vertexai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tesh254/lebro"
)

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{Model: "gemini-2.5-flash"}); err == nil {
		t.Fatal("New() error = nil, want project error")
	}
	if _, err := New(Config{Project: "proj"}); err == nil {
		t.Fatal("New() error = nil, want model error")
	}
}

func TestGenerateCallsVertexV1Endpoint(t *testing.T) {
	var gotPath, gotAPIKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-goog-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":"hello"},{"functionCall":{"id":"call-1","name":"lookup","args":{"city":"Nairobi"}}}]},"contentRating":{}}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"thoughtsTokenCount":1,"totalTokenCount":6}}`))
	}))
	defer server.Close()
	model, err := New(Config{Project: "proj", Model: "gemini-2.5-flash", BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	response, err := model.Generate(t.Context(), lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/projects/proj/locations/global/publishers/google/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAPIKey != "" {
		t.Fatalf("api key header = %q, want none for ADC", gotAPIKey)
	}
	calls := response.Message.ToolCalls.Values()
	if response.Message.Content != "hello" || len(calls) != 1 || calls[0].ToolID != "lookup" {
		t.Fatalf("response = %#v", response.Message)
	}
	if response.FinishReason != lebro.FinishReasonToolCalls || response.Usage.ReasoningTokens != 1 {
		t.Fatalf("response = %#v", response)
	}
}

func TestGenerateUsesConfiguredLocation(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":"ok"}]},"contentRating":{}}]}`))
	}))
	defer server.Close()
	model, err := New(Config{Project: "proj", Location: "us-central1", Model: "gemini-2.5-flash", BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.Generate(t.Context(), lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/projects/proj/locations/us-central1/publishers/google/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("path = %q", gotPath)
	}
}

func TestGenerateMapsAPIErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"slow down"}}`))
	}))
	defer server.Close()
	model, err := New(Config{Project: "proj", Model: "gemini-2.5-flash", BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = model.Generate(t.Context(), lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	var modelErr *lebro.ModelError
	if !errors.As(err, &modelErr) || modelErr.Kind != lebro.ModelErrorRateLimited || modelErr.Provider != "vertexai" || modelErr.StatusCode != 429 {
		t.Fatalf("error = %#v", err)
	}
}

func TestGenerateMapsAuthenticationErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":401,"status":"UNAUTHENTICATED","message":"caller identity denied"}}`))
	}))
	defer server.Close()
	model, err := New(Config{Project: "proj", Model: "gemini-2.5-flash", BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = model.Generate(t.Context(), lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	var modelErr *lebro.ModelError
	if !errors.As(err, &modelErr) || modelErr.Kind != lebro.ModelErrorAuthentication {
		t.Fatalf("error = %#v", err)
	}
}

func TestStreamDeltasAndTerminalUsage(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"he\"}]},\"contentRating\":{}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"llo\"}]},\"contentRating\":{}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"finishReason\":\"STOP\",\"content\":{\"role\":\"model\",\"parts\":[{}]},\"contentRating\":{}}],\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":2,\"totalTokenCount\":4}}\n\n"))
	}))
	defer server.Close()
	model, err := New(Config{Project: "proj", Model: "gemini-2.5-flash", BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := model.Stream(t.Context(), lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	var text string
	var terminal lebro.StreamDelta
	for {
		delta, err := reader.Next()
		if err != nil {
			break
		}
		if delta.FinishReason != "" || delta.Usage != (lebro.ModelUsage{}) {
			terminal = delta
		}
		text += delta.Text
	}
	if gotPath != "/v1/projects/proj/locations/global/publishers/google/models/gemini-2.5-flash:streamGenerateContent" {
		t.Fatalf("path = %q", gotPath)
	}
	if text != "hello" {
		t.Fatalf("text = %q", text)
	}
	if terminal.FinishReason != lebro.FinishReasonStop || terminal.Usage.TotalTokens != 4 {
		t.Fatalf("terminal = %#v", terminal)
	}
}

func TestStreamPropagatesCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"he\"}]},\"contentRating\":{}}]}\n\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	model, err := New(Config{Project: "proj", Model: "gemini-2.5-flash", BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	reader, err := model.Stream(ctx, lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	if _, err := reader.Next(); err != nil {
		t.Fatal(err)
	}
	cancel()
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
	if !errors.Is(streamErr, context.Canceled) {
		t.Fatalf("stream error = %v, want context.Canceled", streamErr)
	}
}
