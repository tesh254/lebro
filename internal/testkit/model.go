// Package testkit contains deterministic fixtures for lebro's own tests and
// examples. It is internal so it cannot become part of the public runtime API.
package testkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tesh254/lebro"
)

var (
	// ErrScriptExhausted means a model call had no remaining fixture.
	ErrScriptExhausted = errors.New("testkit: model script exhausted")
	// ErrInvalidFixture means a fixture cannot produce a valid response.
	ErrInvalidFixture = errors.New("testkit: invalid model fixture")
	// ErrUnexpectedFixture means a streaming fixture was used for Generate, or
	// a synchronous fixture was used for Stream.
	ErrUnexpectedFixture = errors.New("testkit: unexpected model fixture")
)

var defaultStartTime = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)

// FixtureKind identifies the behavior scripted for one model call.
type FixtureKind string

const (
	FixtureResponse     FixtureKind = "response"
	FixtureStream       FixtureKind = "stream"
	FixtureFailure      FixtureKind = "failure"
	FixtureCancellation FixtureKind = "cancellation"
)

// ToolCall aliases the public provider-neutral tool-call contract.
type ToolCall = lebro.ModelToolCall

// StreamChunk describes one item in a scripted stream.
type StreamChunk struct {
	Text             string
	ToolCall         *ToolCall
	StructuredOutput json.RawMessage
	Err              error
}

// Fixture describes the result of one model invocation.
type Fixture struct {
	kind      FixtureKind
	response  lebro.ModelResponse
	toolCalls []ToolCall
	stream    []StreamChunk
	err       error
}

// Response returns an exact synchronous response fixture.
func Response(response lebro.ModelResponse) Fixture {
	return Fixture{kind: FixtureResponse, response: cloneResponse(response)}
}

// Text returns a successful assistant text fixture.
func Text(content string) Fixture {
	return Response(lebro.ModelResponse{
		Message:      lebro.Message{Role: lebro.RoleAssistant, Content: content},
		FinishReason: lebro.FinishReasonStop,
	})
}

// ToolCallResponse returns a tool-call fixture. Blank IDs are filled with
// deterministic tool-call IDs when the fixture is consumed.
func ToolCallResponse(calls ...ToolCall) Fixture {
	return Fixture{
		kind: FixtureResponse,
		response: lebro.ModelResponse{
			Message:      lebro.Message{Role: lebro.RoleAssistant},
			FinishReason: lebro.FinishReasonToolCalls,
		},
		toolCalls: cloneToolCalls(calls),
	}
}

// StructuredOutput returns a successful JSON response fixture.
func StructuredOutput(value json.RawMessage) Fixture {
	fixture := Response(lebro.ModelResponse{
		Message:      lebro.Message{Role: lebro.RoleAssistant, StructuredOutput: lebro.NewModelStructuredOutput(value)},
		FinishReason: lebro.FinishReasonStop,
	})
	return fixture
}

// Stream returns a fixture delivered by Model.Stream.
func Stream(chunks ...StreamChunk) Fixture {
	return Fixture{kind: FixtureStream, stream: cloneChunks(chunks)}
}

// Failure returns the supplied error from a model call.
func Failure(err error) Fixture { return Fixture{kind: FixtureFailure, err: err} }

// WaitForCancellation blocks a model call until its context is cancelled.
func WaitForCancellation() Fixture { return Fixture{kind: FixtureCancellation} }

// TextChunk creates a text stream chunk.
func TextChunk(text string) StreamChunk { return StreamChunk{Text: text} }

// ToolCallChunk creates a tool-call stream chunk.
func ToolCallChunk(call ToolCall) StreamChunk { return StreamChunk{ToolCall: &call} }

// StructuredOutputChunk creates a JSON stream chunk.
func StructuredOutputChunk(value json.RawMessage) StreamChunk {
	return StreamChunk{StructuredOutput: append(json.RawMessage(nil), value...)}
}

// FailureChunk terminates a stream with err. A nil error is normalized to
// ErrInvalidFixture so the chunk always terminates the stream.
func FailureChunk(err error) StreamChunk {
	if err == nil {
		err = ErrInvalidFixture
	}
	return StreamChunk{Err: err}
}

// ModelCall records one invocation of the scripted model.
type ModelCall struct {
	ID       string
	Sequence int
	At       time.Time
	Request  lebro.ModelRequest
}

// RunEventType identifies deterministic events emitted by the harness.
type RunEventType string

const (
	RunEventModelStarted   RunEventType = "model.started"
	RunEventModelCompleted RunEventType = "model.completed"
	RunEventModelFailed    RunEventType = "model.failed"
	RunEventModelCancelled RunEventType = "model.cancelled"
	RunEventStreamChunk    RunEventType = "model.stream_chunk"
)

// RunEvent records the observable lifecycle of a scripted call.
type RunEvent struct {
	ID        string
	Sequence  int
	At        time.Time
	Type      RunEventType
	CallID    string
	Message   lebro.Message
	ToolCalls []ToolCall
	Err       error
}

// StreamEvent is one deterministic item returned by Model.Stream.
type StreamEvent struct {
	ID               string
	Sequence         int
	At               time.Time
	CallID           string
	Text             string
	ToolCall         *ToolCall
	StructuredOutput json.RawMessage
	Err              error
}

// Model is a FIFO scripted model with deterministic IDs and timestamps.
type Model struct {
	mu        sync.Mutex
	fixtures  []Fixture
	next      int
	calls     []ModelCall
	events    []RunEvent
	callSeq   int
	eventSeq  int
	toolSeq   int
	timeSeq   int
	streamSeq int
}

var _ lebro.Model = (*Model)(nil)

// NewModel creates a model whose fixtures are consumed in the supplied order.
func NewModel(fixtures ...Fixture) *Model {
	cloned := make([]Fixture, len(fixtures))
	for i, fixture := range fixtures {
		cloned[i] = cloneFixture(fixture)
	}
	return &Model{fixtures: cloned}
}

// Generate consumes one synchronous fixture.
func (m *Model) Generate(ctx context.Context, request lebro.ModelRequest) (lebro.ModelResponse, error) {
	fixture, call, err := m.begin(ctx, request)
	if err != nil {
		return lebro.ModelResponse{}, err
	}

	switch fixture.kind {
	case FixtureResponse:
		response := cloneResponse(fixture.response)
		toolCalls := cloneToolCalls(fixture.toolCalls)
		if fixture.toolCalls == nil {
			toolCalls = response.Message.ToolCalls.Values()
		}
		for i := range toolCalls {
			if toolCalls[i].ID == "" {
				toolCalls[i].ID = m.nextToolCallID()
			}
		}
		if fixture.toolCalls != nil || !response.Message.ToolCalls.IsZero() {
			encoded, encodingErr := lebro.NewModelToolCalls(toolCalls...)
			if encodingErr != nil {
				err := fmt.Errorf("%w: %v", ErrInvalidFixture, encodingErr)
				m.finish(call.ID, RunEventModelFailed, lebro.Message{}, nil, err)
				return lebro.ModelResponse{}, err
			}
			response.Message.ToolCalls = encoded
		}
		if err := response.Validate(); err != nil {
			err := fmt.Errorf("%w: %v", ErrInvalidFixture, err)
			m.finish(call.ID, RunEventModelFailed, lebro.Message{}, nil, err)
			return lebro.ModelResponse{}, err
		}
		m.finish(call.ID, RunEventModelCompleted, response.Message, toolCalls, nil)
		return response, nil
	case FixtureFailure:
		if fixture.err == nil {
			err := fmt.Errorf("%w: failure error is nil", ErrInvalidFixture)
			m.finish(call.ID, RunEventModelFailed, lebro.Message{}, nil, err)
			return lebro.ModelResponse{}, err
		}
		m.finish(call.ID, RunEventModelFailed, lebro.Message{}, nil, fixture.err)
		return lebro.ModelResponse{}, fixture.err
	case FixtureCancellation:
		if ctx.Done() == nil {
			err := fmt.Errorf("%w: cancellation fixture requires a cancellable context", ErrInvalidFixture)
			m.finish(call.ID, RunEventModelFailed, lebro.Message{}, nil, err)
			return lebro.ModelResponse{}, err
		}
		<-ctx.Done()
		err := ctx.Err()
		m.finish(call.ID, RunEventModelCancelled, lebro.Message{}, nil, err)
		return lebro.ModelResponse{}, err
	default:
		err := fmt.Errorf("%w: %s cannot be used with Generate", ErrUnexpectedFixture, fixture.kind)
		m.finish(call.ID, RunEventModelFailed, lebro.Message{}, nil, err)
		return lebro.ModelResponse{}, err
	}
}

// Stream consumes one stream fixture and emits its chunks in FIFO order. This
// test-only method lets streaming behavior be scripted before a public stream
// protocol is introduced.
func (m *Model) Stream(ctx context.Context, request lebro.ModelRequest) (<-chan StreamEvent, error) {
	fixture, call, err := m.begin(ctx, request)
	if err != nil {
		return nil, err
	}

	switch fixture.kind {
	case FixtureFailure:
		if fixture.err == nil {
			err := fmt.Errorf("%w: failure error is nil", ErrInvalidFixture)
			m.finish(call.ID, RunEventModelFailed, lebro.Message{}, nil, err)
			return nil, err
		}
		m.finish(call.ID, RunEventModelFailed, lebro.Message{}, nil, fixture.err)
		return nil, fixture.err
	case FixtureCancellation:
		if ctx.Done() == nil {
			err := fmt.Errorf("%w: cancellation fixture requires a cancellable context", ErrInvalidFixture)
			m.finish(call.ID, RunEventModelFailed, lebro.Message{}, nil, err)
			return nil, err
		}
		out := make(chan StreamEvent)
		go func() {
			defer close(out)
			<-ctx.Done()
			m.finish(call.ID, RunEventModelCancelled, lebro.Message{}, nil, ctx.Err())
		}()
		return out, nil
	case FixtureStream:
		out := make(chan StreamEvent)
		go m.emitStream(ctx, call, fixture.stream, out)
		return out, nil
	default:
		err := fmt.Errorf("%w: %s cannot be used with Stream", ErrUnexpectedFixture, fixture.kind)
		m.finish(call.ID, RunEventModelFailed, lebro.Message{}, nil, err)
		return nil, err
	}
}

// Calls returns a defensive snapshot in invocation order.
func (m *Model) Calls() []ModelCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]ModelCall, len(m.calls))
	for i, call := range m.calls {
		result[i] = cloneCall(call)
	}
	return result
}

// Events returns a defensive snapshot in event order.
func (m *Model) Events() []RunEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]RunEvent, len(m.events))
	for i, event := range m.events {
		result[i] = cloneRunEvent(event)
	}
	return result
}

// Remaining reports how many fixtures have not been consumed.
func (m *Model) Remaining() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.fixtures) - m.next
}

func (m *Model) begin(ctx context.Context, request lebro.ModelRequest) (Fixture, ModelCall, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.callSeq++
	call := ModelCall{
		ID:       fmt.Sprintf("model-call-%04d", m.callSeq),
		Sequence: m.callSeq,
		At:       m.nextTimeLocked(),
		Request:  cloneRequest(request),
	}
	m.calls = append(m.calls, call)
	m.appendEventLocked(RunEvent{Type: RunEventModelStarted, CallID: call.ID})

	if err := ctx.Err(); err != nil {
		m.appendEventLocked(RunEvent{Type: RunEventModelCancelled, CallID: call.ID, Err: err})
		return Fixture{}, call, err
	}
	if validationErr := request.Validate(); validationErr != nil {
		err := &lebro.ModelError{Kind: lebro.ModelErrorInvalidRequest, Message: validationErr.Error(), Err: validationErr}
		m.appendEventLocked(RunEvent{Type: RunEventModelFailed, CallID: call.ID, Err: err})
		return Fixture{}, call, err
	}
	if m.next == len(m.fixtures) {
		m.appendEventLocked(RunEvent{Type: RunEventModelFailed, CallID: call.ID, Err: ErrScriptExhausted})
		return Fixture{}, call, ErrScriptExhausted
	}
	fixture := cloneFixture(m.fixtures[m.next])
	m.next++
	return fixture, call, nil
}

func (m *Model) emitStream(ctx context.Context, call ModelCall, chunks []StreamChunk, out chan<- StreamEvent) {
	defer close(out)
	for _, chunk := range chunks {
		event := m.streamEvent(call.ID, chunk)
		select {
		case out <- event:
		case <-ctx.Done():
			m.finish(call.ID, RunEventModelCancelled, lebro.Message{}, nil, ctx.Err())
			return
		}
		if event.Err != nil {
			m.finish(call.ID, RunEventModelFailed, lebro.Message{}, nil, event.Err)
			return
		}
		toolCalls := []ToolCall(nil)
		if event.ToolCall != nil {
			toolCalls = []ToolCall{cloneToolCall(*event.ToolCall)}
		}
		message := lebro.Message{Role: lebro.RoleAssistant, Content: event.Text}
		if event.StructuredOutput != nil {
			message.Content = string(event.StructuredOutput)
		}
		m.finish(call.ID, RunEventStreamChunk, message, toolCalls, nil)
	}
	m.finish(call.ID, RunEventModelCompleted, lebro.Message{}, nil, nil)
}

func (m *Model) streamEvent(callID string, chunk StreamChunk) StreamEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.streamSeq++
	event := StreamEvent{
		ID:               fmt.Sprintf("stream-event-%04d", m.streamSeq),
		Sequence:         m.streamSeq,
		At:               m.nextTimeLocked(),
		CallID:           callID,
		Text:             chunk.Text,
		StructuredOutput: append(json.RawMessage(nil), chunk.StructuredOutput...),
		Err:              chunk.Err,
	}
	if chunk.ToolCall != nil {
		call := cloneToolCall(*chunk.ToolCall)
		if call.ID == "" {
			m.toolSeq++
			call.ID = fmt.Sprintf("tool-call-%04d", m.toolSeq)
		}
		event.ToolCall = &call
	}
	return event
}

func (m *Model) nextToolCallID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.toolSeq++
	return fmt.Sprintf("tool-call-%04d", m.toolSeq)
}

func (m *Model) finish(callID string, eventType RunEventType, message lebro.Message, toolCalls []ToolCall, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appendEventLocked(RunEvent{Type: eventType, CallID: callID, Message: message, ToolCalls: toolCalls, Err: err})
}

func (m *Model) appendEventLocked(event RunEvent) {
	m.eventSeq++
	event.ID = fmt.Sprintf("run-event-%04d", m.eventSeq)
	event.Sequence = m.eventSeq
	event.At = m.nextTimeLocked()
	event.Message = cloneMessage(event.Message)
	event.ToolCalls = cloneToolCalls(event.ToolCalls)
	m.events = append(m.events, event)
}

func (m *Model) nextTimeLocked() time.Time {
	timestamp := defaultStartTime.Add(time.Duration(m.timeSeq) * time.Millisecond)
	m.timeSeq++
	return timestamp
}

func toolCallsFromResponse(response lebro.ModelResponse) []ToolCall {
	return response.Message.ToolCalls.Values()
}

func cloneFixture(fixture Fixture) Fixture {
	return Fixture{
		kind:      fixture.kind,
		response:  cloneResponse(fixture.response),
		toolCalls: cloneToolCalls(fixture.toolCalls),
		stream:    cloneChunks(fixture.stream),
		err:       fixture.err,
	}
}

func cloneResponse(response lebro.ModelResponse) lebro.ModelResponse {
	response.Message = cloneMessage(response.Message)
	response.Extension = append(json.RawMessage(nil), response.Extension...)
	return response
}

func cloneMessage(message lebro.Message) lebro.Message { return message }

func cloneRequest(request lebro.ModelRequest) lebro.ModelRequest {
	request.Messages = append([]lebro.Message(nil), request.Messages...)
	for i := range request.Messages {
		request.Messages[i] = cloneMessage(request.Messages[i])
	}
	request.Tools = append([]lebro.ToolDefinition(nil), request.Tools...)
	for i := range request.Tools {
		request.Tools[i].InputSchema = append(json.RawMessage(nil), request.Tools[i].InputSchema...)
		request.Tools[i].OutputSchema = append(json.RawMessage(nil), request.Tools[i].OutputSchema...)
	}
	if request.OutputSchema != nil {
		outputSchema := *request.OutputSchema
		outputSchema.Schema = append(json.RawMessage(nil), request.OutputSchema.Schema...)
		request.OutputSchema = &outputSchema
	}
	request.Extension = append(json.RawMessage(nil), request.Extension...)
	return request
}

func cloneCall(call ModelCall) ModelCall { call.Request = cloneRequest(call.Request); return call }

func cloneToolCall(call ToolCall) ToolCall {
	call.Arguments = append(json.RawMessage(nil), call.Arguments...)
	return call
}

func cloneToolCalls(calls []ToolCall) []ToolCall {
	if calls == nil {
		return nil
	}
	result := make([]ToolCall, len(calls))
	for i, call := range calls {
		result[i] = cloneToolCall(call)
	}
	return result
}

func cloneChunks(chunks []StreamChunk) []StreamChunk {
	result := make([]StreamChunk, len(chunks))
	for i, chunk := range chunks {
		result[i] = chunk
		result[i].StructuredOutput = append(json.RawMessage(nil), chunk.StructuredOutput...)
		if chunk.ToolCall != nil {
			call := cloneToolCall(*chunk.ToolCall)
			result[i].ToolCall = &call
		}
	}
	return result
}

func cloneRunEvent(event RunEvent) RunEvent {
	event.Message = cloneMessage(event.Message)
	event.ToolCalls = cloneToolCalls(event.ToolCalls)
	return event
}
