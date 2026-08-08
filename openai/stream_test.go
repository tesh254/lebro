package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tesh254/lebro"
)

func TestModelStreamImplementsStreamingModel(t *testing.T) {
	t.Parallel()
	var _ lebro.StreamingModel = (*Model)(nil)
}

func TestModelStreamDeliversTextDeltas(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":2,\"total_tokens\":4}}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(server.Close)

	model := newAdapter(t, server, Config{APIKey: "test-key", Model: "gpt-4o"})
	reader, err := model.Stream(context.Background(), lebro.ModelRequest{
		Model:    "gpt-4o",
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer func() { _ = reader.Close() }()

	var texts []string
	var sawFinish bool
	var usage lebro.ModelUsage
	for {
		delta, derr := reader.Next()
		if errors.Is(derr, io.EOF) {
			break
		}
		if derr != nil {
			t.Fatalf("Next() error = %v", derr)
		}
		if delta.Text != "" {
			texts = append(texts, delta.Text)
		}
		if delta.FinishReason != "" {
			sawFinish = true
		}
		if delta.Usage != (lebro.ModelUsage{}) {
			usage = delta.Usage
		}
	}
	if got, want := strings.Join(texts, ""), "Hello world"; got != want {
		t.Fatalf("streamed text = %q, want %q", got, want)
	}
	if !sawFinish {
		t.Fatal("stream did not deliver a terminal finish-reason delta")
	}
	if usage.TotalTokens != 4 {
		t.Fatalf("usage total tokens = %d, want 4", usage.TotalTokens)
	}
}

func TestModelStreamRejectsToolsAndStructuredOutput(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(server.Close)

	model := newAdapter(t, server, Config{APIKey: "test-key", Model: "gpt-4o"})

	if _, err := model.Stream(context.Background(), lebro.ModelRequest{
		Model:    "gpt-4o",
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
		Tools:    []lebro.ToolDefinition{{ID: "lookup"}},
	}); err == nil {
		t.Fatal("Stream() with tools should fail")
	}

	if _, err := model.Stream(context.Background(), lebro.ModelRequest{
		Model:        "gpt-4o",
		Messages:     []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
		OutputSchema: &lebro.ModelOutputSchema{Name: "result", Schema: []byte(`{"type":"object"}`)},
	}); err == nil {
		t.Fatal("Stream() with output schema should fail")
	}
}

func TestModelStreamHonoursCancelledContext(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	model := newAdapter(t, server, Config{APIKey: "test-key", Model: "gpt-4o", Timeout: 30 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	reader, err := model.Stream(ctx, lebro.ModelRequest{
		Model:    "gpt-4o",
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer func() { _ = reader.Close() }()

	delta, err := reader.Next()
	if err != nil || delta.Text != "Hello" {
		t.Fatalf("first Next() = %v, %v; want delta Hello", delta, err)
	}
	cancel()
	for {
		_, derr := reader.Next()
		if derr != nil {
			break
		}
	}
}

func TestModelStreamClassifiesErrorEvents(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"error\":{\"message\":\"rate limited\",\"type\":\"rate_limit_exceeded\"}}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(server.Close)

	model := newAdapter(t, server, Config{APIKey: "test-key", Model: "gpt-4o"})
	reader, err := model.Stream(context.Background(), lebro.ModelRequest{
		Model:    "gpt-4o",
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer func() { _ = reader.Close() }()

	_, derr := reader.Next()
	if derr == nil {
		t.Fatal("Next() error = nil, want stream error")
	}
	var modelErr *lebro.ModelError
	if !errors.As(derr, &modelErr) {
		t.Fatalf("error = %v, want *lebro.ModelError", derr)
	}
	if modelErr.Message != "rate limited" {
		t.Fatalf("message = %q, want %q", modelErr.Message, "rate limited")
	}
}

func TestModelStreamClassifiesHTTPFailure(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"unavailable","type":"server_error"}}`)
	}))
	t.Cleanup(server.Close)

	model := newAdapter(t, server, Config{APIKey: "test-key", Model: "gpt-4o"})
	_, err := model.Stream(context.Background(), lebro.ModelRequest{
		Model:    "gpt-4o",
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Stream() error = nil, want HTTP failure")
	}
	var modelErr *lebro.ModelError
	if !errors.As(err, &modelErr) || modelErr.Kind != lebro.ModelErrorUnavailable {
		t.Fatalf("error = %v, want ModelErrorUnavailable", err)
	}
}
