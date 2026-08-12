package httpapi_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/httpapi"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

// sseEvent is one parsed Server-Sent Event.
type sseEvent struct {
	name string
	data httpapi.StreamEvent
}

// readSSE parses an event stream until the reader is exhausted.
func readSSE(t *testing.T, reader io.Reader) []sseEvent {
	t.Helper()
	var events []sseEvent
	scanner := bufio.NewScanner(reader)
	var name string
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var event httpapi.StreamEvent
			payload := strings.TrimPrefix(line, "data: ")
			if err := json.Unmarshal([]byte(payload), &event); err != nil {
				t.Fatalf("decode event data %q: %v", payload, err)
			}
			events = append(events, sseEvent{name: name, data: event})
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("scan event stream: %v", err)
	}
	return events
}

// streamingModel emits a fixed sequence of deltas, then a terminal delta.
type streamingModel struct {
	deltas []lebro.StreamDelta
}

func (m streamingModel) Generate(context.Context, lebro.ModelRequest) (lebro.ModelResponse, error) {
	return textResponse("non-streaming fallback"), nil
}

func (m streamingModel) Stream(ctx context.Context, _ lebro.ModelRequest) (lebro.StreamReader, error) {
	index := 0
	return &lebro.StreamReaderFunc{
		NextFn: func() (lebro.StreamDelta, error) {
			if err := ctx.Err(); err != nil {
				return lebro.StreamDelta{}, err
			}
			if index >= len(m.deltas) {
				return lebro.StreamDelta{}, io.EOF
			}
			delta := m.deltas[index]
			index++
			return delta, nil
		},
	}, nil
}

func TestStreamEmitsOrderedDeltasThenOneTerminalEvent(t *testing.T) {
	model := streamingModel{deltas: []lebro.StreamDelta{
		{Text: "Hel"},
		{Text: "lo"},
		{Text: "!", FinishReason: lebro.FinishReasonStop, Usage: lebro.ModelUsage{TotalTokens: 12}},
	}}
	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeAgent(newAgent(t, "assistant", model)))

	recorder := doJSON(t, server.Handler(), http.MethodPost, "/agents/assistant/runs/stream", httpapi.RunRequest{
		Messages: []httpapi.MessageInput{{Content: "hi"}},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type = %q, want text/event-stream", got)
	}

	events := readSSE(t, recorder.Body)
	if len(events) < 2 {
		t.Fatalf("events = %d, want deltas plus a terminal event: %+v", len(events), events)
	}

	var text strings.Builder
	terminals := 0
	for i, event := range events {
		switch event.name {
		case "model_delta":
			if terminals > 0 {
				t.Errorf("delta at index %d arrived after a terminal event", i)
			}
			text.WriteString(event.data.Text)
		case "run_succeeded", "run_failed", "run_cancelled":
			terminals++
			if i != len(events)-1 {
				t.Errorf("terminal event %q at index %d is not last", event.name, i)
			}
		default:
			t.Errorf("unexpected event name %q", event.name)
		}
	}
	if terminals != 1 {
		t.Fatalf("terminal events = %d, want exactly 1", terminals)
	}
	if text.String() != "Hello!" {
		t.Fatalf("assembled text = %q, want %q", text.String(), "Hello!")
	}

	final := events[len(events)-1]
	if final.name != "run_succeeded" {
		t.Fatalf("terminal event = %q, want run_succeeded", final.name)
	}
	if final.data.Status != string(lebro.RunStatusSucceeded) {
		t.Fatalf("terminal status = %q", final.data.Status)
	}
	if final.data.RunID == "" {
		t.Fatal("terminal event carries no run_id")
	}
	if final.data.Text != "Hello!" {
		t.Fatalf("terminal text = %q, want the assembled message", final.data.Text)
	}
}

// A failing run must still terminate the stream with a typed error rather than
// dropping the connection, so a client can distinguish failure from a network
// fault.
func TestStreamTerminatesFailedRunWithTypedError(t *testing.T) {
	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeAgent(newAgent(t, "assistant", failingModel{kind: lebro.ModelErrorUnavailable})))

	recorder := doJSON(t, server.Handler(), http.MethodPost, "/agents/assistant/runs/stream", httpapi.RunRequest{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with an in-stream error", recorder.Code)
	}

	events := readSSE(t, recorder.Body)
	if len(events) == 0 {
		t.Fatal("no events emitted")
	}
	final := events[len(events)-1]
	if final.name != "run_failed" {
		t.Fatalf("terminal event = %q, want run_failed", final.name)
	}
	if final.data.Error == nil {
		t.Fatal("terminal event carries no error")
	}
	if final.data.Error.Code != httpapi.ErrorCodeProviderFailure {
		t.Fatalf("error code = %q, want %q", final.data.Error.Code, httpapi.ErrorCodeProviderFailure)
	}
}

// An aborting provider stream must not emit an empty model_delta before the
// terminal event. StreamEvent has no field for a delta-level error, so an
// error-only delta would serialize to an event carrying nothing but a type,
// immediately followed by the terminal event with the real classification.
func TestStreamSuppressesErrorOnlyDeltas(t *testing.T) {
	model := streamingModel{deltas: []lebro.StreamDelta{
		{Text: "partial output"},
		{Err: errors.New("provider aborted the stream")},
	}}
	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeAgent(newAgent(t, "assistant", model)))

	recorder := doJSON(t, server.Handler(), http.MethodPost, "/agents/assistant/runs/stream", httpapi.RunRequest{})
	events := readSSE(t, recorder.Body)

	for i, event := range events {
		if event.name != "model_delta" {
			continue
		}
		if event.data.Text == "" && event.data.ToolCall == nil &&
			len(event.data.StructuredOutput) == 0 && event.data.FinishReason == "" &&
			event.data.Usage == nil {
			t.Fatalf("empty model_delta at index %d carries nothing a client can use: %+v", i, event.data)
		}
	}

	if len(events) == 0 {
		t.Fatal("no events emitted")
	}
	final := events[len(events)-1]
	if final.name != "run_failed" {
		t.Fatalf("terminal event = %q, want run_failed", final.name)
	}
	if final.data.Error == nil {
		t.Fatal("terminal event carries no error, so the abort is unreported")
	}
}

// Usage is documented on the terminal event, so it must actually be populated
// there. It is the run total, not one call's figures: a provider reports usage
// per call, and a client treating the terminal event as the end-of-run marker
// should not have to sum deltas itself.
func TestStreamTerminalEventCarriesRunTotalUsage(t *testing.T) {
	model := streamingModel{deltas: []lebro.StreamDelta{
		{Text: "one", Usage: lebro.ModelUsage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8}},
		{Text: "two", FinishReason: lebro.FinishReasonStop, Usage: lebro.ModelUsage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}},
	}}
	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeAgent(newAgent(t, "assistant", model)))

	recorder := doJSON(t, server.Handler(), http.MethodPost, "/agents/assistant/runs/stream", httpapi.RunRequest{})
	events := readSSE(t, recorder.Body)

	final := events[len(events)-1]
	if final.name != "run_succeeded" {
		t.Fatalf("terminal event = %q, want run_succeeded", final.name)
	}
	if final.data.Usage == nil {
		t.Fatal("terminal event carries no usage despite documenting the field")
	}
	// The per-call figures differ, so a total that merely echoed the last delta
	// would score differently from a genuine sum.
	want := httpapi.Usage{InputTokens: 4, OutputTokens: 7, TotalTokens: 11}
	if *final.data.Usage != want {
		t.Fatalf("usage = %+v, want the run total %+v", *final.data.Usage, want)
	}
}

// Errors discovered before the stream opens are reported with a real status
// code, because the status is not yet committed.
func TestStreamReportsPreflightErrorsWithStatus(t *testing.T) {
	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeAgent(newAgent(t, "assistant", &scriptedModel{})))
	handler := server.Handler()

	recorder := doJSON(t, handler, http.MethodPost, "/agents/absent/runs/stream", httpapi.RunRequest{})
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown agent status = %d, want %d", recorder.Code, http.StatusNotFound)
	}

	recorder = doRaw(t, handler, http.MethodPost, "/agents/assistant/runs/stream", `{"messages":`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("malformed body status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	assertErrorCode(t, recorder, httpapi.ErrorCodeInvalidRequest)
}

// A client that disconnects mid-stream must have its run cancelled rather than
// left to burn provider budget.
//
// Cancellation here arrives through the request context, which the runtime's
// delta send already selects on, so this test does not by itself prove the
// handler's own Cancel is reached — see
// TestStreamReleasesRunWhenRequestContextStaysLive for that. What it does prove
// is that the run stops when the client goes away, which is the behavior a
// caller depends on.
func TestStreamCancelsRunWhenClientDisconnects(t *testing.T) {
	observed := make(chan struct{})
	var once sync.Once

	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeAgent(newAgent(t, "assistant", cancelObservingModel{
		onCancel: func() { once.Do(func() { close(observed) }) },
	})))

	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	// Cancelling the request context is what a client going away looks like to
	// the server. Closing the response body alone is not: the connection
	// returns to the idle pool and the server-side context stays live.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		httpServer.URL+"/agents/assistant/runs/stream", bytes.NewReader([]byte(`{}`)))
	must(t, err)

	response, err := http.DefaultClient.Do(request)
	must(t, err)
	defer func() { _ = response.Body.Close() }()

	// Read one delta so the run is demonstrably streaming before hanging up.
	buffer := make([]byte, 64)
	if _, err := response.Body.Read(buffer); err != nil {
		t.Fatalf("read first delta: %v", err)
	}
	cancel()

	select {
	case <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("run was not cancelled after the client disconnected")
	}
}

// The handler must release the run goroutine even when the request context
// never cancels.
//
// This is the case that makes the handler's deferred Cancel load-bearing. The
// runtime publishes each delta with a send that selects on the run context, so
// a disconnect unblocks it for free; but when the handler returns early while
// the request is still live — a payload that fails to marshal, say — nothing
// cancels that context, and the run goroutine stays blocked mid-send forever.
// Only StreamRun.Cancel plus a full drain releases it.
//
// The stream is abandoned here through a ResponseWriter whose writes fail while
// the request context stays open, which is exactly that shape. goleak (see
// main_test.go) turns a surviving goroutine into a failure.
func TestStreamReleasesRunWhenRequestContextStaysLive(t *testing.T) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeAgent(newAgent(t, "assistant", endlessStreamModel{stop: stop})))

	// context.Background() never cancels, so the run's own send has no escape
	// hatch; the handler is the only thing that can free the goroutine.
	request := httptest.NewRequest(http.MethodPost, "/agents/assistant/runs/stream",
		bytes.NewReader([]byte(`{}`))).WithContext(context.Background())

	before := goruntime.NumGoroutine()
	server.Handler().ServeHTTP(&failingWriter{header: http.Header{}}, request)

	// The handler has returned, so its deferred Cancel and drain have already
	// run. Any goroutine still parked is leaked, not in flight.
	deadline := time.After(5 * time.Second)
	for goruntime.NumGoroutine() > before {
		select {
		case <-deadline:
			t.Fatalf("run goroutine was not released: goroutines %d, want at most %d", goruntime.NumGoroutine(), before)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// failingWriter accepts the header write and then fails every body write,
// simulating a connection that died without cancelling the request context.
type failingWriter struct {
	header http.Header
}

func (w *failingWriter) Header() http.Header { return w.header }

func (w *failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("connection is gone")
}

func (w *failingWriter) WriteHeader(int) {}

func (w *failingWriter) Flush() {}

// cancelObservingModel streams indefinitely and reports when the run stops
// consuming it, whether that arrives as a cancelled context or as the reader
// being closed.
//
// Both signals are watched because the runtime may abandon a stream either way:
// it stops calling Next and closes the reader, so a fixture that only polled
// ctx.Done() from inside Next could sit unread and never observe anything. A
// dedicated watcher goroutine sees the cancellation regardless, and Close
// covers the path where the reader is released while the context stays live.
type cancelObservingModel struct {
	onCancel func()
}

func (cancelObservingModel) Generate(context.Context, lebro.ModelRequest) (lebro.ModelResponse, error) {
	return textResponse("unused"), nil
}

func (m cancelObservingModel) Stream(ctx context.Context, _ lebro.ModelRequest) (lebro.StreamReader, error) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			m.onCancel()
		case <-done:
		}
	}()
	var closeOnce sync.Once
	return &lebro.StreamReaderFunc{
		NextFn: func() (lebro.StreamDelta, error) {
			if err := ctx.Err(); err != nil {
				return lebro.StreamDelta{}, err
			}
			return lebro.StreamDelta{Text: "delta "}, nil
		},
		CloseFn: func() error {
			// Release the watcher so it cannot outlive the stream and trip
			// goleak on a run that ended without cancellation.
			closeOnce.Do(func() { close(done) })
			return nil
		},
	}, nil
}

// endlessStreamModel produces deltas forever without consulting its context,
// modelling a provider adapter that does not honor cancellation promptly. It
// stops only when the test tears down, so a handler that abandons the stream
// without draining leaves the run goroutine blocked on a channel send.
type endlessStreamModel struct {
	stop chan struct{}
}

func (endlessStreamModel) Generate(context.Context, lebro.ModelRequest) (lebro.ModelResponse, error) {
	return textResponse("unused"), nil
}

func (m endlessStreamModel) Stream(context.Context, lebro.ModelRequest) (lebro.StreamReader, error) {
	return &lebro.StreamReaderFunc{
		NextFn: func() (lebro.StreamDelta, error) {
			select {
			case <-m.stop:
				return lebro.StreamDelta{}, io.EOF
			default:
				return lebro.StreamDelta{Text: "delta "}, nil
			}
		},
	}, nil
}

// Tool-call arguments are model-composed from the whole transcript, so the
// default policy must remove them. The fixture's arguments differ from its
// text, so a pass-through bug cannot score the same as correct redaction.
func TestDefaultRedactorRemovesToolCallArguments(t *testing.T) {
	const secret = "SSN 123-45-6789 from the retrieved document"
	call := lebro.ModelToolCall{
		ID:        "call-1",
		ToolID:    "lookup",
		Arguments: json.RawMessage(`{"query":"` + secret + `"}`),
	}
	model := streamingModel{deltas: []lebro.StreamDelta{
		{Text: "visible assistant text"},
		{ToolCall: &call},
		{Text: "", FinishReason: lebro.FinishReasonStop},
	}}

	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeAgent(newAgent(t, "assistant", model)))

	recorder := doJSON(t, server.Handler(), http.MethodPost, "/agents/assistant/runs/stream", httpapi.RunRequest{})
	body := recorder.Body.String()
	if strings.Contains(body, secret) {
		t.Fatalf("default redactor leaked tool-call arguments: %s", body)
	}

	events := readSSE(t, strings.NewReader(body))
	var sawToolCall, sawText bool
	for _, event := range events {
		if event.data.ToolCall != nil {
			sawToolCall = true
			if len(event.data.ToolCall.Arguments) != 0 {
				t.Fatalf("tool call arguments survived redaction: %s", event.data.ToolCall.Arguments)
			}
			// The call's identity must survive, or a client cannot correlate
			// the invocation with its later result.
			if event.data.ToolCall.ID != "call-1" || event.data.ToolCall.ToolID != "lookup" {
				t.Fatalf("tool call identity lost: %+v", event.data.ToolCall)
			}
		}
		if strings.Contains(event.data.Text, "visible assistant text") {
			sawText = true
		}
	}
	if !sawToolCall {
		t.Fatal("no tool call event was emitted")
	}
	if !sawText {
		t.Fatal("assistant text was redacted; only tool arguments should be")
	}
}

// Redaction must not mutate the transcript the run is assembling: the tool
// still has to receive its real arguments.
func TestRedactionDoesNotAffectToolExecution(t *testing.T) {
	arguments := make(chan string, 1)
	registry := mustValue(lebro.NewToolRegistry(lebrojsonschema.NewCompiler()))
	must(t, registry.Register(capturingTool{arguments: arguments}))

	call := lebro.ModelToolCall{
		ID:        "call-1",
		ToolID:    "capture",
		Arguments: json.RawMessage(`{"value":"real argument"}`),
	}
	calls, err := lebro.NewModelToolCalls(call)
	must(t, err)

	step := 0
	agent := newAgentWithConfig(t, lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "assistant", Tools: []lebro.ToolID{"capture"}},
		Tools:      registry,
		Model: modelFunc(func(context.Context, lebro.ModelRequest) (lebro.ModelResponse, error) {
			step++
			if step == 1 {
				return lebro.ModelResponse{
					Message:      lebro.Message{Role: lebro.RoleAssistant, ToolCalls: calls},
					FinishReason: lebro.FinishReasonToolCalls,
				}, nil
			}
			return textResponse("done"), nil
		}),
	})

	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeAgent(agent))

	recorder := doJSON(t, server.Handler(), http.MethodPost, "/agents/assistant/runs/stream", httpapi.RunRequest{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body)
	}

	select {
	case got := <-arguments:
		if !strings.Contains(got, "real argument") {
			t.Fatalf("tool received %q, want the unredacted arguments", got)
		}
	case <-time.After(time.Second):
		t.Fatal("tool was never invoked")
	}
	if strings.Contains(recorder.Body.String(), "real argument") {
		t.Fatal("arguments reached the wire despite default redaction")
	}
}

func TestPassthroughRedactorKeepsArguments(t *testing.T) {
	call := lebro.ModelToolCall{
		ID:        "call-1",
		ToolID:    "lookup",
		Arguments: json.RawMessage(`{"query":"visible on purpose"}`),
	}
	model := streamingModel{deltas: []lebro.StreamDelta{
		{ToolCall: &call},
		{Text: "done", FinishReason: lebro.FinishReasonStop},
	}}

	server := httpapi.NewServer(httpapi.ServerConfig{Redactor: httpapi.PassthroughRedactor})
	must(t, server.ExposeAgent(newAgent(t, "assistant", model)))

	recorder := doJSON(t, server.Handler(), http.MethodPost, "/agents/assistant/runs/stream", httpapi.RunRequest{})
	if !strings.Contains(recorder.Body.String(), "visible on purpose") {
		t.Fatalf("passthrough redactor dropped arguments: %s", recorder.Body)
	}
}

// A redactor returning the zero delta suppresses that delta entirely.
//
// The assertion is scoped to delta events on purpose. A Redactor governs the
// incremental stream, not the run's transcript: the terminal event reports the
// assembled assistant message, which the runtime built from the unredacted
// deltas, so suppressed text legitimately reappears there. Asserting over the
// whole body would encode the opposite, wrong expectation.
func TestRedactorCanSuppressDeltas(t *testing.T) {
	model := streamingModel{deltas: []lebro.StreamDelta{
		{Text: "suppress me"},
		{Text: "keep me", FinishReason: lebro.FinishReasonStop},
	}}
	server := httpapi.NewServer(httpapi.ServerConfig{
		Redactor: func(delta lebro.StreamDelta) lebro.StreamDelta {
			if delta.Text == "suppress me" {
				return lebro.StreamDelta{}
			}
			return delta
		},
	})
	must(t, server.ExposeAgent(newAgent(t, "assistant", model)))

	recorder := doJSON(t, server.Handler(), http.MethodPost, "/agents/assistant/runs/stream", httpapi.RunRequest{})
	events := readSSE(t, recorder.Body)

	var deltaText strings.Builder
	for _, event := range events {
		if event.name == "model_delta" {
			deltaText.WriteString(event.data.Text)
		}
	}
	if strings.Contains(deltaText.String(), "suppress me") {
		t.Fatalf("suppressed delta was emitted: %q", deltaText.String())
	}
	if !strings.Contains(deltaText.String(), "keep me") {
		t.Fatalf("kept delta was dropped: %q", deltaText.String())
	}
}

// capturingTool records the arguments it receives.
type capturingTool struct {
	arguments chan string
}

func (capturingTool) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{
		ID:          "capture",
		Description: "Capture the arguments",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`),
	}
}

func (t capturingTool) Execute(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
	select {
	case t.arguments <- string(input):
	default:
	}
	return json.RawMessage(`{"ok":true}`), nil
}
