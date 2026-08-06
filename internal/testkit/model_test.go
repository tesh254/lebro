package testkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/tesh254/lebro"
)

func TestModelConsumesSynchronousFixturesInDeterministicOrder(t *testing.T) {
	t.Parallel()
	providerErr := errors.New("provider unavailable")
	model := NewModel(
		Text("hello"),
		ToolCallResponse(ToolCall{ToolID: "lookup", Arguments: json.RawMessage(`{"id":"42"}`)}),
		StructuredOutput(json.RawMessage(`{"ok":true}`)),
		Failure(providerErr),
	)
	request := lebro.ModelRequest{
		Model:    "fake-model",
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "start"}},
		Tools: []lebro.ToolDefinition{{
			ID: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"string"}`),
		}},
	}

	text, err := model.Generate(context.Background(), request)
	if err != nil || text.Message.Content != "hello" {
		t.Fatalf("text response = %#v, %v", text, err)
	}
	tool, err := model.Generate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	AssertToolCalls(t, toolCallsFromResponse(tool), []ToolCall{{ID: "tool-call-0001", ToolID: "lookup", Arguments: json.RawMessage(`{"id":"42"}`)}})
	structured, err := model.Generate(context.Background(), request)
	if err != nil || string(structured.Message.StructuredOutput.Raw()) != `{"ok":true}` {
		t.Fatalf("structured response = %#v, %v", structured, err)
	}
	if _, err := model.Generate(context.Background(), request); !errors.Is(err, providerErr) {
		t.Fatalf("failure error = %v, want provider error", err)
	}
	if got := model.Remaining(); got != 0 {
		t.Fatalf("Remaining() = %d, want 0", got)
	}

	request.Messages[0].Content = "mutated"
	request.Tools[0].InputSchema[0] = '['
	calls := model.Calls()
	if got, want := calls[0].ID, "model-call-0001"; got != want {
		t.Fatalf("call ID = %q, want %q", got, want)
	}
	if got, want := calls[3].Sequence, 4; got != want {
		t.Fatalf("last sequence = %d, want %d", got, want)
	}
	if got := calls[0].At; !got.Equal(defaultStartTime) {
		t.Fatalf("first call time = %s, want %s", got, defaultStartTime)
	}
	if got := calls[0].Request.Messages[0].Content; got != "start" {
		t.Fatalf("recorded message = %q, want start", got)
	}
	if got := string(calls[0].Request.Tools[0].InputSchema); got != `{"type":"object"}` {
		t.Fatalf("recorded schema = %s", got)
	}
	calls[0].Request.Messages[0].Content = "changed again"
	if got := model.Calls()[0].Request.Messages[0].Content; got != "start" {
		t.Fatalf("Calls() snapshot mutated model: %q", got)
	}

	events := model.Events()
	if got, want := len(events), 8; got != want {
		t.Fatalf("event count = %d, want %d", got, want)
	}
	for i, event := range events {
		if event.Sequence != i+1 || event.ID != formatID("run-event", i+1) {
			t.Fatalf("event %d = %#v", i, event)
		}
	}
	AssertMessages(t, []lebro.Message{events[1].Message, events[3].Message}, []lebro.Message{text.Message, tool.Message})
	AssertToolCalls(t, events[3].ToolCalls, toolCallsFromResponse(tool))
	events[3].ToolCalls[0].Arguments[0] = '['
	if got := string(model.Events()[3].ToolCalls[0].Arguments); got != `{"id":"42"}` {
		t.Fatalf("Events() snapshot mutated model: %s", got)
	}
}

func TestModelGeneratedRecordsRepeatAcrossInstances(t *testing.T) {
	t.Parallel()
	request := lebro.ModelRequest{Model: "fake-model", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hello"}}}
	first := NewModel(ToolCallResponse(ToolCall{ToolID: "echo", Arguments: json.RawMessage(`{"text":"hello"}`)}))
	second := NewModel(ToolCallResponse(ToolCall{ToolID: "echo", Arguments: json.RawMessage(`{"text":"hello"}`)}))
	for _, model := range []*Model{first, second} {
		if _, err := model.Generate(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(first.Calls(), second.Calls()) {
		t.Fatalf("deterministic calls differ: %#v != %#v", first.Calls(), second.Calls())
	}
	AssertRunEvents(t, first.Events(), second.Events())
}

func TestModelGenerateErrorAndCancellationPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		model   *Model
		ctx     context.Context
		wantErr error
	}{
		{name: "exhausted", model: NewModel(), ctx: context.Background(), wantErr: ErrScriptExhausted},
		{name: "nil failure", model: NewModel(Failure(nil)), ctx: context.Background(), wantErr: ErrInvalidFixture},
		{name: "invalid structured output", model: NewModel(StructuredOutput(json.RawMessage(`{`))), ctx: context.Background(), wantErr: ErrInvalidFixture},
		{name: "invalid tool encoding", model: NewModel(ToolCallResponse(ToolCall{ToolID: "lookup", Arguments: json.RawMessage(`{`)})), ctx: context.Background(), wantErr: ErrInvalidFixture},
		{name: "uncancellable context", model: NewModel(WaitForCancellation()), ctx: context.Background(), wantErr: ErrInvalidFixture},
		{name: "stream fixture", model: NewModel(Stream(TextChunk("no"))), ctx: context.Background(), wantErr: ErrUnexpectedFixture},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.model.Generate(test.ctx, lebro.ModelRequest{})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Generate() error = %v, want %v", err, test.wantErr)
			}
		})
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	model := NewModel(Text("unused"))
	if _, err := model.Generate(cancelled, lebro.ModelRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled Generate() error = %v", err)
	}
	if got := model.Remaining(); got != 1 {
		t.Fatalf("pre-cancelled call consumed fixture; remaining = %d", got)
	}

	invalidModel := NewModel(Text("unused"))
	_, err := invalidModel.Generate(context.Background(), lebro.ModelRequest{Tools: []lebro.ToolDefinition{{}}})
	var modelErr *lebro.ModelError
	if !errors.As(err, &modelErr) || modelErr.Kind != lebro.ModelErrorInvalidRequest {
		t.Fatalf("invalid request error = %v", err)
	}
	if invalidModel.Remaining() != 1 || invalidModel.Events()[1].Type != RunEventModelFailed {
		t.Fatalf("invalid request changed script state: remaining=%d events=%#v", invalidModel.Remaining(), invalidModel.Events())
	}

	waiting, cancelWaiting := context.WithCancel(context.Background())
	waitingModel := NewModel(WaitForCancellation())
	done := make(chan error, 1)
	go func() {
		_, err := waitingModel.Generate(waiting, lebro.ModelRequest{})
		done <- err
	}()
	waitFor(t, func() bool { return len(waitingModel.Calls()) == 1 })
	cancelWaiting()
	AssertCancellation(t, <-done)
	if got := waitingModel.Events()[1].Type; got != RunEventModelCancelled {
		t.Fatalf("terminal event = %q, want cancellation", got)
	}
}

func TestModelStreamsTextToolsStructuredOutputFailuresAndCompletion(t *testing.T) {
	t.Parallel()
	streamErr := errors.New("stream failed")
	originalArgs := json.RawMessage(`{"city":"Nairobi"}`)
	originalJSON := json.RawMessage(`{"temperature":24.5}`)
	model := NewModel(
		Stream(
			TextChunk("weather: "),
			ToolCallChunk(ToolCall{ToolID: "weather", Arguments: originalArgs}),
			StructuredOutputChunk(originalJSON),
			FailureChunk(streamErr),
		),
		Stream(),
	)
	originalArgs[0] = '['
	originalJSON[0] = '['

	stream, err := model.Stream(context.Background(), lebro.ModelRequest{Model: "fake-model"})
	if err != nil {
		t.Fatal(err)
	}
	var events []StreamEvent
	for event := range stream {
		events = append(events, event)
	}
	if got, want := len(events), 4; got != want {
		t.Fatalf("stream event count = %d, want %d", got, want)
	}
	if events[0].Text != "weather: " || events[0].ID != "stream-event-0001" {
		t.Fatalf("text stream event = %#v", events[0])
	}
	AssertToolCalls(t, []ToolCall{*events[1].ToolCall}, []ToolCall{{ID: "tool-call-0001", ToolID: "weather", Arguments: json.RawMessage(`{"city":"Nairobi"}`)}})
	if got := string(events[2].StructuredOutput); got != `{"temperature":24.5}` {
		t.Fatalf("structured stream output = %s", got)
	}
	if got := model.Events()[3].Message.Content; got != `{"temperature":24.5}` {
		t.Fatalf("structured stream chunk message = %q", got)
	}
	if !errors.Is(events[3].Err, streamErr) {
		t.Fatalf("stream error = %v", events[3].Err)
	}
	if got := model.Events()[len(model.Events())-1].Type; got != RunEventModelFailed {
		t.Fatalf("terminal stream event = %q, want failed", got)
	}

	empty, err := model.Stream(context.Background(), lebro.ModelRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(collectStream(empty)); got != 0 {
		t.Fatalf("empty stream returned %d chunks", got)
	}
	if got := model.Events()[len(model.Events())-1].Type; got != RunEventModelCompleted {
		t.Fatalf("empty stream terminal event = %q, want completed", got)
	}
}

func TestModelStreamFailureChunkNilTerminatesStream(t *testing.T) {
	t.Parallel()
	model := NewModel(Stream(FailureChunk(nil)))
	stream, err := model.Stream(context.Background(), lebro.ModelRequest{})
	if err != nil {
		t.Fatal(err)
	}
	events := collectStream(stream)
	if got, want := len(events), 1; got != want {
		t.Fatalf("stream event count = %d, want %d", got, want)
	}
	if !errors.Is(events[0].Err, ErrInvalidFixture) {
		t.Fatalf("stream error = %v, want %v", events[0].Err, ErrInvalidFixture)
	}
	if got := model.Events()[len(model.Events())-1].Type; got != RunEventModelFailed {
		t.Fatalf("terminal stream event = %q, want failed", got)
	}
}

func TestModelStreamErrorAndCancellationPaths(t *testing.T) {
	t.Parallel()
	providerErr := errors.New("provider unavailable")
	tests := []struct {
		name    string
		model   *Model
		wantErr error
	}{
		{name: "failure", model: NewModel(Failure(providerErr)), wantErr: providerErr},
		{name: "nil failure", model: NewModel(Failure(nil)), wantErr: ErrInvalidFixture},
		{name: "uncancellable context", model: NewModel(WaitForCancellation()), wantErr: ErrInvalidFixture},
		{name: "response fixture", model: NewModel(Text("no")), wantErr: ErrUnexpectedFixture},
		{name: "exhausted", model: NewModel(), wantErr: ErrScriptExhausted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stream, err := test.model.Stream(context.Background(), lebro.ModelRequest{})
			if stream != nil || !errors.Is(err, test.wantErr) {
				t.Fatalf("Stream() = %v, %v; want nil, %v", stream, err, test.wantErr)
			}
		})
	}

	preCancelled, cancelPre := context.WithCancel(context.Background())
	cancelPre()
	if stream, err := NewModel(Stream()).Stream(preCancelled, lebro.ModelRequest{}); stream != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled Stream() = %v, %v", stream, err)
	}

	waiting, cancelWaiting := context.WithCancel(context.Background())
	waitingModel := NewModel(WaitForCancellation())
	stream, err := waitingModel.Stream(waiting, lebro.ModelRequest{})
	if err != nil {
		t.Fatal(err)
	}
	cancelWaiting()
	if got := len(collectStream(stream)); got != 0 {
		t.Fatalf("cancellation stream returned %d chunks", got)
	}
	if got := waitingModel.Events()[1].Type; got != RunEventModelCancelled {
		t.Fatalf("terminal event = %q, want cancellation", got)
	}

	blocked, cancelBlocked := context.WithCancel(context.Background())
	blockedModel := NewModel(Stream(TextChunk("unread")))
	blockedStream, err := blockedModel.Stream(blocked, lebro.ModelRequest{})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		blockedModel.mu.Lock()
		defer blockedModel.mu.Unlock()
		return blockedModel.streamSeq == 1
	})
	cancelBlocked()
	waitFor(t, func() bool {
		events := blockedModel.Events()
		return events[len(events)-1].Type == RunEventModelCancelled
	})
	if got := len(collectStream(blockedStream)); got != 0 {
		t.Fatalf("cancelled blocked stream returned %d chunks", got)
	}
}

func TestRunEventAssertionNormalizesEquivalentErrors(t *testing.T) {
	t.Parallel()
	want := []RunEvent{{
		ID: "event-1", Sequence: 1, At: defaultStartTime, Type: RunEventModelFailed,
		CallID: "call-1", Err: errors.New("same failure"),
	}}
	got := []RunEvent{{
		ID: "event-1", Sequence: 1, At: defaultStartTime, Type: RunEventModelFailed,
		CallID: "call-1", Err: errors.New("same failure"),
	}}
	AssertRunEvents(t, got, want)
	if got := errorString(nil); got != "" {
		t.Fatalf("nil error string = %q", got)
	}
}

func collectStream(stream <-chan StreamEvent) []StreamEvent {
	var events []StreamEvent
	for event := range stream {
		events = append(events, event)
	}
	return events
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition was not met before timeout")
		}
		runtime.Gosched()
	}
}

func formatID(prefix string, sequence int) string {
	return fmt.Sprintf("%s-%04d", prefix, sequence)
}

func TestFixtureCloneHelpers(t *testing.T) {
	t.Parallel()
	message := lebro.Message{Role: lebro.RoleAssistant, Content: "ok"}
	if got := cloneMessage(message); !reflect.DeepEqual(got, message) {
		t.Fatalf("cloneMessage() = %#v", got)
	}
	call := ModelCall{Request: lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hello"}}}}
	if got := cloneCall(call); !reflect.DeepEqual(got, call) {
		t.Fatalf("cloneCall() = %#v", got)
	}
}
