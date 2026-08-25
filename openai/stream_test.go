package openai

import (
	"context"
	"encoding/json"
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

func TestModelStreamDeliversOrderedReasoningAndUsage(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		write := func(event string) {
			_, _ = io.WriteString(w, "data: "+event+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		write(`{"id":"chatcmpl-reasoning","choices":[{"index":0,"delta":{"reasoning":"check constraints","reasoning_details":[{"type":"reasoning.encrypted","data":"opaque"}]}}]}`)
		write(`{"id":"chatcmpl-reasoning","choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":8,"completion_tokens_details":{"reasoning_tokens":5},"total_tokens":11}}`)
		write(`[DONE]`)
	}))
	t.Cleanup(server.Close)

	model := newAdapter(t, server, Config{APIKey: "test-key", Model: "gpt-4o"})
	reader, err := model.Stream(context.Background(), lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "solve"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	var gotReasoning, gotText string
	var details lebro.ModelReasoningDetails
	var usage lebro.ModelUsage
	for {
		delta, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		gotReasoning += delta.Reasoning.Text
		gotText += delta.Text
		if delta.Reasoning.Details != "" {
			details = delta.Reasoning.Details
		}
		if delta.Usage != (lebro.ModelUsage{}) {
			usage = delta.Usage
		}
	}
	if gotReasoning != "check constraints" || gotText != "answer" {
		t.Fatalf("stream = reasoning %q, text %q", gotReasoning, gotText)
	}
	if got := string(details.Raw()); got != `[{"type":"reasoning.encrypted","data":"opaque"}]` {
		t.Fatalf("reasoning details = %s", got)
	}
	if usage != (lebro.ModelUsage{InputTokens: 3, OutputTokens: 8, ReasoningTokens: 5, TotalTokens: 11}) {
		t.Fatalf("usage = %#v", usage)
	}
}

// streamToolCallFixture events model the canonical fragmented streamed tool
// call: fragment one carries id and name; fragments two and three append
// argument JSON across separate SSE events, as OpenAI-compatible providers do.
var streamToolCallEvents = []string{
	`{"index":0,"id":"call-1","type":"function","function":{"name":"lookup","arguments":""}}`,
	`{"index":0,"function":{"arguments":"{\"id\":"}}`,
	`{"index":0,"function":{"arguments":"\"42\"}"}}`,
}

func TestModelStreamDeliversCompleteToolCalls(t *testing.T) {
	t.Parallel()
	var observed observeRequest
	server := newRecordedServer(t, &observed, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = io.WriteString(w, "data: "+s+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		for _, fragment := range streamToolCallEvents {
			write(`{"id":"chatcmpl-4","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[` + fragment + `]}}]}`)
		}
		write(`{"id":"chatcmpl-4","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call-2","type":"function","function":{"name":"ping"}}]}}]}`)
		write(`{"id":"chatcmpl-4","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`)
		write(`[DONE]`)
	})
	t.Cleanup(server.Close)

	model := newAdapter(t, server, Config{APIKey: "test-key", Model: "gpt-4o"})
	reader, err := model.Stream(context.Background(), lebro.ModelRequest{
		Model:    "gpt-4o",
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "look it up"}},
		Tools:    []lebro.ToolDefinition{{ID: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}, {ID: "ping"}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer func() { _ = reader.Close() }()

	var calls []lebro.ModelToolCall
	var finish lebro.FinishReason
	var usage lebro.ModelUsage
	for {
		delta, derr := reader.Next()
		if errors.Is(derr, io.EOF) {
			break
		}
		if derr != nil {
			t.Fatalf("Next() error = %v", derr)
		}
		if delta.ToolCall != nil {
			calls = append(calls, *delta.ToolCall)
		}
		if delta.FinishReason != "" {
			finish = delta.FinishReason
		}
		if delta.Usage != (lebro.ModelUsage{}) {
			usage = delta.Usage
		}
	}
	if len(calls) != 2 {
		t.Fatalf("complete tool calls = %#v, want two", calls)
	}
	if calls[0].ID != "call-1" || calls[0].ToolID != "lookup" || string(calls[0].Arguments) != `{"id":"42"}` {
		t.Fatalf("call[0] = %#v", calls[0])
	}
	if calls[1].ID != "call-2" || calls[1].ToolID != "ping" || string(calls[1].Arguments) != `{}` {
		t.Fatalf("call[1] = %#v", calls[1])
	}
	if finish != lebro.FinishReasonToolCalls {
		t.Fatalf("finish reason = %q, want tool_calls", finish)
	}
	if usage.TotalTokens != 7 {
		t.Fatalf("usage total tokens = %d, want 7", usage.TotalTokens)
	}
	for _, delta := range []struct{ call lebro.ModelToolCall }{{calls[0]}, {calls[1]}} {
		if err := (lebro.StreamDelta{ToolCall: &delta.call}).Validate(); err != nil {
			t.Fatalf("delta validate = %v", err)
		}
	}

	body := observed.body(t)
	tools, _ := body["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("wire tools = %#v", body["tools"])
	}
}

func TestModelStreamAttachesStructuredOutput(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = io.WriteString(w, "data: "+s+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		write(`{"id":"chatcmpl-5","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"{\"ok\":"}}]}`)
		write(`{"id":"chatcmpl-5","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"true}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
		write(`[DONE]`)
	}))
	t.Cleanup(server.Close)

	model := newAdapter(t, server, Config{APIKey: "test-key", Model: "gpt-4o"})
	reader, err := model.Stream(context.Background(), lebro.ModelRequest{
		Model:        "gpt-4o",
		Messages:     []lebro.Message{{Role: lebro.RoleUser, Content: "return JSON"}},
		OutputSchema: &lebro.ModelOutputSchema{Name: "result", Schema: json.RawMessage(`{"type":"object"}`), Strict: true},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer func() { _ = reader.Close() }()

	var text strings.Builder
	var structured lebro.ModelStructuredOutput
	sawTerminal := false
	for {
		delta, derr := reader.Next()
		if errors.Is(derr, io.EOF) {
			break
		}
		if derr != nil {
			t.Fatalf("Next() error = %v", derr)
		}
		text.WriteString(delta.Text)
		if delta.StructuredOutput != "" {
			structured = delta.StructuredOutput
		}
		if delta.FinishReason == lebro.FinishReasonStop {
			sawTerminal = true
		}
	}
	if got, want := text.String(), `{"ok":true}`; got != want {
		t.Fatalf("streamed text = %q, want %q", got, want)
	}
	if !sawTerminal {
		t.Fatal("stream did not deliver a terminal stop delta")
	}
	if structured == "" || string(structured.Raw()) != `{"ok":true}` {
		t.Fatalf("structured output = %s", structured.Raw())
	}
}

// TestModelStreamKeepsOrderWhenContentAndFinishShareOneEvent guards against
// providers that send the last content chunk and the finish reason in a single
// event: the text must still be delivered before the terminal delta.
func TestModelStreamKeepsOrderWhenContentAndFinishShareOneEvent(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"id":"c","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"bye"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\n")
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

	type outcome struct {
		text     string
		finish   lebro.FinishReason
		terminal bool
	}
	var got outcome
	for {
		delta, derr := reader.Next()
		if errors.Is(derr, io.EOF) {
			break
		}
		if derr != nil {
			t.Fatalf("Next() error = %v", derr)
		}
		got.text += delta.Text
		if delta.FinishReason != "" {
			got.finish = delta.FinishReason
			got.terminal = true
		}
	}
	if got.text != "bye" || got.finish != lebro.FinishReasonStop || !got.terminal {
		t.Fatalf("stream outcome = %#v, want text before terminal stop", got)
	}
}

func TestModelStreamRejectsMalformedStreamedToolArguments(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = io.WriteString(w, "data: "+s+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		write(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"lookup","arguments":"{broken"}}]}}]}`)
		write(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
	}))
	t.Cleanup(server.Close)

	model := newAdapter(t, server, Config{APIKey: "test-key", Model: "gpt-4o"})
	reader, err := model.Stream(context.Background(), lebro.ModelRequest{
		Model:    "gpt-4o",
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
		Tools:    []lebro.ToolDefinition{{ID: "lookup"}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer func() { _ = reader.Close() }()

	var streamErr error
	for {
		_, derr := reader.Next()
		if errors.Is(derr, io.EOF) {
			break
		}
		if derr != nil {
			streamErr = derr
			break
		}
	}
	if streamErr == nil {
		t.Fatal("Next() error = nil, want malformed tool arguments failure")
	}
	var modelErr *lebro.ModelError
	if !errors.As(streamErr, &modelErr) || modelErr.Kind != lebro.ModelErrorMalformedResponse {
		t.Fatalf("error = %v, want malformed_response", streamErr)
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
