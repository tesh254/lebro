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

	lebrojsonschema "github.com/tesh254/lebro/jsonschema"

	"github.com/tesh254/lebro"
)

func TestModelStreamImplementsStreamingModel(t *testing.T) {
	t.Parallel()
	var _ lebro.StreamingModel = (*Model)(nil)
}

func TestOpenRouterStreamEmitsFinalUsageAndZeroCostExactlyOnce(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"gen-123\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"gen-123\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12,\"prompt_tokens_details\":{\"cached_tokens\":4},\"cost\":0,\"cost_details\":{\"upstream_inference_cost\":0}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	model := newAdapter(t, server, Config{APIKey: "test-key", Model: "vendor/model", PricingDomain: lebro.PricingDomainOpenRouter})
	reader, err := model.Stream(context.Background(), lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	terminalCount := 0
	for {
		delta, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		if !delta.IsTerminal() {
			continue
		}
		terminalCount++
		if delta.Usage.CacheReadTokens != 4 || delta.Accounting.ProviderRequestID != "gen-123" {
			t.Fatalf("terminal metadata = %#v", delta)
		}
		if len(delta.Accounting.Costs) != 1 || delta.Accounting.Costs[0].Source != lebro.CostProviderReported || delta.Accounting.Costs[0].Amount != "0" {
			t.Fatalf("terminal cost = %#v, want known provider-reported zero", delta.Accounting.Costs)
		}
	}
	if terminalCount != 1 {
		t.Fatalf("terminal delta count = %d, want 1", terminalCount)
	}
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

// streamedToolCallFixture events model the fragmented streamed tool call the
// session-failure investigation replayed: fragments carry id, name, and
// argument JSON across separate SSE events.
var streamedToolCallFixture = []string{
	`{"id":"fixture","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_fixture","type":"function","function":{"name":"lookup","arguments":""}}]},"finish_reason":null}]}`,
	`{"id":"fixture","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]},"finish_reason":null}]}`,
}

// finishWithoutUsage repeats the terminal finish reason with an empty delta
// and no usage; finishWithUsage is the trailing usage chunk some providers
// emit after the first finish.
const (
	finishWithoutUsage = `{"id":"fixture","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`
	finishWithUsage    = `{"id":"fixture","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"cost":0}}`
	usageOnlyChunk     = `{"id":"fixture","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"cost":0}}`
)

func streamSSE(t *testing.T, events ...string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, event := range events {
			_, _ = io.WriteString(w, "data: "+event+"\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	return server
}

// collectStream drains reader and returns emitted tool calls, the terminal
// deltas, and the usage observed on the last terminal.
func collectStream(t *testing.T, reader lebro.StreamReader) (calls []lebro.ModelToolCall, terminals int, usage lebro.ModelUsage) {
	t.Helper()
	for {
		delta, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return calls, terminals, usage
		}
		if err != nil {
			t.Fatalf("Next() error = %v", err)
		}
		if delta.ToolCall != nil {
			calls = append(calls, *delta.ToolCall)
		}
		if delta.IsTerminal() {
			terminals++
			usage = delta.Usage
		}
	}
}

// TestModelStreamRepeatedToolCallsFinishEmitsCallsOnce pins the idempotent
// tool-call completion lifecycle: a provider that repeats the tool_calls
// finish reason — most commonly a trailing usage chunk — must emit each
// intended call once, one terminal, and the received usage exactly as the
// conventional single-finish shapes do.
func TestModelStreamRepeatedToolCallsFinishEmitsCallsOnce(t *testing.T) {
	t.Parallel()
	for _, fixture := range []struct {
		name    string
		events  []string
		calls   int
		usage   int64
		callIDs []string
	}{
		{
			name:    "repeated finish then trailing usage",
			events:  append(append([]string{}, streamedToolCallFixture...), finishWithoutUsage, finishWithUsage),
			calls:   1,
			usage:   12,
			callIDs: []string{"call_fixture"},
		},
		{
			name:    "finish then usage-only chunk",
			events:  append(append([]string{}, streamedToolCallFixture...), finishWithoutUsage, usageOnlyChunk),
			calls:   1,
			usage:   12,
			callIDs: []string{"call_fixture"},
		},
		{
			name:    "single finish with usage",
			events:  append(append([]string{}, streamedToolCallFixture...), finishWithUsage),
			calls:   1,
			usage:   12,
			callIDs: []string{"call_fixture"},
		},
		{
			name:    "repeated finishes without usage",
			events:  append(append([]string{}, streamedToolCallFixture...), finishWithoutUsage, finishWithoutUsage),
			calls:   1,
			usage:   0,
			callIDs: []string{"call_fixture"},
		},
		{
			name:    "first finish already has usage then repeated",
			events:  append(append([]string{}, streamedToolCallFixture...), finishWithUsage, finishWithUsage),
			calls:   1,
			usage:   12,
			callIDs: []string{"call_fixture"},
		},
		{
			name: "two interleaved tools with repeated finish and usage",
			events: append([]string{},
				`{"id":"fixture","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-a","type":"function","function":{"name":"lookup"}}]}}]}`,
				`{"id":"fixture","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call-b","type":"function","function":{"name":"ping","arguments":"{\"v\":1}"}}]}}]}`,
				`{"id":"fixture","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`,
				finishWithoutUsage,
				finishWithUsage),
			calls:   2,
			usage:   12,
			callIDs: []string{"call-a", "call-b"},
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()
			model := newAdapter(t, streamSSE(t, fixture.events...), Config{APIKey: "test-key", Model: "gpt-4o"})
			reader, err := model.Stream(context.Background(), lebro.ModelRequest{
				Model:    "gpt-4o",
				Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
				Tools:    []lebro.ToolDefinition{{ID: "lookup"}, {ID: "ping"}},
			})
			if err != nil {
				t.Fatalf("Stream() error = %v", err)
			}
			defer func() { _ = reader.Close() }()

			calls, terminals, usage := collectStream(t, reader)
			if len(calls) != fixture.calls {
				t.Fatalf("emitted tool calls = %#v, want %d", calls, fixture.calls)
			}
			for i, call := range calls {
				if call.ID != fixture.callIDs[i] {
					t.Fatalf("call[%d].ID = %q, want %q", i, call.ID, fixture.callIDs[i])
				}
			}
			if _, err := lebro.NewModelToolCalls(calls...); err != nil {
				t.Fatalf("aggregate emitted tool calls: %v", err)
			}
			if terminals != 1 {
				t.Fatalf("terminal delta count = %d, want 1", terminals)
			}
			if usage.TotalTokens != fixture.usage {
				t.Fatalf("terminal usage tokens = %d, want %d", usage.TotalTokens, fixture.usage)
			}
			if fixture.usage > 0 {
				// Trailing usage must arrive with the full received
				// identity: the request ID and the provider-reported zero
				// cost, exactly as single-finish fixtures preserve them.
				if usage != (lebro.ModelUsage{InputTokens: 10, OutputTokens: 2, TotalTokens: fixture.usage}) {
					t.Fatalf("terminal usage = %#v, want full received usage", usage)
				}
			}
		})
	}
}

// TestModelStreamRepeatedFinishKeepsAccountingIdentity asserts the full
// received accounting on the trailing terminal: the provider request ID and
// the known provider-reported zero cost must survive the repeated finish,
// not just the token counts.
func TestModelStreamRepeatedFinishKeepsAccountingIdentity(t *testing.T) {
	t.Parallel()
	model := newAdapter(t, streamSSE(t, append(append([]string{}, streamedToolCallFixture...), finishWithoutUsage, finishWithUsage)...), Config{APIKey: "test-key", Model: "gpt-4o", PricingDomain: lebro.PricingDomainOpenRouter})
	reader, err := model.Stream(context.Background(), lebro.ModelRequest{
		Model:    "gpt-4o",
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
		Tools:    []lebro.ToolDefinition{{ID: "lookup"}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer func() { _ = reader.Close() }()

	var accounting lebro.ModelAccounting
	terminals := 0
	for {
		delta, derr := reader.Next()
		if errors.Is(derr, io.EOF) {
			break
		}
		if derr != nil {
			t.Fatalf("Next() error = %v", derr)
		}
		if delta.IsTerminal() {
			terminals++
			accounting = delta.Accounting
		}
	}
	if terminals != 1 {
		t.Fatalf("terminal delta count = %d, want 1", terminals)
	}
	if accounting.ProviderRequestID != "fixture" {
		t.Fatalf("terminal request ID = %q, want %q", accounting.ProviderRequestID, "fixture")
	}
	if len(accounting.Costs) != 1 || accounting.Costs[0].Source != lebro.CostProviderReported || accounting.Costs[0].Amount != "0" {
		t.Fatalf("terminal cost = %#v, want known provider-reported zero", accounting.Costs)
	}
}

// TestModelStreamRejectsToolFragmentsAfterCompletion proves the lifecycle
// guard surfaces protocol violations instead of silently dropping calls: a
// fragment-bearing event after the tool calls completed fails the stream as a
// malformed response rather than losing the call.
func TestModelStreamRejectsToolFragmentsAfterCompletion(t *testing.T) {
	t.Parallel()
	model := newAdapter(t, streamSSE(t, append(append([]string{}, streamedToolCallFixture...), finishWithoutUsage, `{"id":"fixture","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call-late","type":"function","function":{"name":"ping"}}]},"finish_reason":null}]}`)...), Config{APIKey: "test-key", Model: "gpt-4o"})
	reader, err := model.Stream(context.Background(), lebro.ModelRequest{
		Model:    "gpt-4o",
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
		Tools:    []lebro.ToolDefinition{{ID: "lookup"}, {ID: "ping"}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer func() { _ = reader.Close() }()

	var streamErr error
	for {
		delta, derr := reader.Next()
		if errors.Is(derr, io.EOF) {
			break
		}
		if delta.ToolCall != nil {
			continue
		}
		if delta.IsTerminal() {
			continue
		}
		if derr != nil {
			streamErr = derr
			break
		}
	}
	if streamErr == nil {
		t.Fatal("Next() error = nil, want malformed response for late tool fragments")
	}
	var modelErr *lebro.ModelError
	if !errors.As(streamErr, &modelErr) || modelErr.Kind != lebro.ModelErrorMalformedResponse {
		t.Fatalf("error = %v, want malformed_response", streamErr)
	}
}

// countingTool is a real registered tool the agent fixture executes; it
// records every execution so the test can prove the repaired stream executes
// each intended tool exactly once.
type countingTool struct {
	mu      sync.Mutex
	calls   int
	lastArg string
}

func (t *countingTool) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{ID: "lookup", Description: "look up a value", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (t *countingTool) Execute(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	t.lastArg = string(args)
	return json.RawMessage(`{"result":"ok"}`), nil
}

// TestAgentStreamExecutesStreamedToolCallsOnce drives a real tool-enabled
// agent through the repeated-finish fixture: the first model step streams
// tool calls under the duplicate-finish shape, the tool executes once, and
// the second model step answers so the run completes successfully.
func TestAgentStreamExecutesStreamedToolCallsOnce(t *testing.T) {
	t.Parallel()
	requests := 0
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		mu.Lock()
		requests++
		call := requests
		mu.Unlock()
		switch call {
		case 1:
			for _, event := range append(append([]string{}, streamedToolCallFixture...), finishWithoutUsage, finishWithUsage) {
				_, _ = io.WriteString(w, "data: "+event+"\n\n")
			}
		default:
			_, _ = io.WriteString(w, "data: "+`{"id":"fixture","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	model := newAdapter(t, server, Config{APIKey: "test-key", Model: "gpt-4o"})
	tool := &countingTool{}
	registry, err := lebro.NewToolRegistry(lebrojsonschema.NewCompiler())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "fixture-agent", Tools: []lebro.ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
		MaxSteps:   3,
	})
	if err != nil {
		t.Fatal(err)
	}

	stream, err := agent.RunStream(context.Background(), lebro.RunInput{RunID: "fixture-run", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "list files"}}})
	if err != nil {
		t.Fatal(err)
	}
	for range stream.Deltas {
	}
	result, err := stream.Wait()
	if err != nil {
		t.Fatalf("RunStream() error = %v", err)
	}
	if result.Status != lebro.RunStatusSucceeded {
		t.Fatalf("run status = %s, want succeeded", result.Status)
	}
	tool.mu.Lock()
	calls := tool.calls
	args := tool.lastArg
	tool.mu.Unlock()
	if calls != 1 {
		t.Fatalf("tool executions = %d, want 1", calls)
	}
	if args != "{}" {
		t.Fatalf("tool arguments = %q, want %q", args, "{}")
	}
	if len(result.Messages) < 3 {
		t.Fatalf("transcript messages = %d, want assistant, tool, and final assistant", len(result.Messages))
	}
	if result.Messages[len(result.Messages)-1].Content != "done" {
		t.Fatalf("final message = %q, want %q", result.Messages[len(result.Messages)-1].Content, "done")
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
