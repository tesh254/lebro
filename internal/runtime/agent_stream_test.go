package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestMain registers goleak verification for the entire runtime test suite.
// Per-test defer-based verification conflicts with t.Parallel because sibling
// tests run on their own goroutines; VerifyTestMain runs once after all tests
// complete so only true leaks are reported.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// streamScriptedModel is a minimal FIFO streaming model used by agent
// streaming tests. It cannot use the testkit harness because importing it
// would create an import cycle with the root package.
type streamScriptedModel struct {
	mu      sync.Mutex
	calls   []ModelRequest
	streams [][]StreamDelta
	next    int
}

type streamFailModel struct {
	err         error
	streamCalls int
}

var _ Model = (*streamFailModel)(nil)
var _ StreamingModel = (*streamFailModel)(nil)

func (m *streamFailModel) Generate(context.Context, ModelRequest) (ModelResponse, error) {
	return ModelResponse{}, m.err
}

func (m *streamFailModel) Stream(context.Context, ModelRequest) (StreamReader, error) {
	m.streamCalls++
	return nil, m.err
}

var _ Model = (*streamScriptedModel)(nil)
var _ StreamingModel = (*streamScriptedModel)(nil)

func newStreamScriptedModel(streams ...[]StreamDelta) *streamScriptedModel {
	return &streamScriptedModel{streams: streams}
}

func (m *streamScriptedModel) Generate(ctx context.Context, request ModelRequest) (ModelResponse, error) {
	return ModelResponse{}, errors.New("lebro: stream-scripted model does not support Generate")
}

func (m *streamScriptedModel) Stream(ctx context.Context, request ModelRequest) (StreamReader, error) {
	m.mu.Lock()
	m.calls = append(m.calls, request)
	if m.next >= len(m.streams) {
		m.mu.Unlock()
		return nil, errors.New("lebro: scripted stream exhausted")
	}
	chunks := m.streams[m.next]
	m.next++
	m.mu.Unlock()

	out := make(chan StreamDelta, len(chunks))
	closed := make(chan struct{})
	go func() {
		defer close(out)
		for _, delta := range chunks {
			select {
			case out <- delta:
			case <-ctx.Done():
				return
			case <-closed:
				return
			}
		}
	}()

	return &StreamReaderFunc{
		NextFn: func() (StreamDelta, error) {
			delta, ok := <-out
			if !ok {
				return StreamDelta{}, io.EOF
			}
			return delta, nil
		},
		CloseFn: func() error {
			select {
			case <-closed:
			default:
				close(closed)
			}
			return nil
		},
	}, nil
}

func (m *streamScriptedModel) Calls() []ModelRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ModelRequest(nil), m.calls...)
}

func textDeltas(parts ...string) []StreamDelta {
	deltas := make([]StreamDelta, 0, len(parts)+1)
	for _, part := range parts {
		deltas = append(deltas, StreamDelta{Text: part})
	}
	deltas = append(deltas, StreamDelta{FinishReason: FinishReasonStop})
	return deltas
}

func toolCallDeltaStream(calls ...ModelToolCall) []StreamDelta {
	deltas := make([]StreamDelta, 0, len(calls)+1)
	for _, call := range calls {
		clone := call
		deltas = append(deltas, StreamDelta{ToolCall: &clone})
	}
	deltas = append(deltas, StreamDelta{FinishReason: FinishReasonToolCalls})
	return deltas
}

func structuredDeltaStream(value json.RawMessage) []StreamDelta {
	return []StreamDelta{
		{StructuredOutput: NewModelStructuredOutput(value)},
		{FinishReason: FinishReasonStop},
	}
}

func TestAgentRunStreamTextOnlyEmitsOrderedDeltas(t *testing.T) {
	t.Parallel()

	model := newStreamScriptedModel(textDeltas("hello ", "world"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "echo", Model: "fixture-model", Instructions: "be brief"},
		Model:      model,
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := agent.RunStream(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Metadata: map[string]string{"request_id": "req-1"},
	})
	if err != nil {
		t.Fatalf("RunStream() setup error = %v", err)
	}
	defer run.Cancel()

	var texts []string
	for delta := range run.Deltas {
		if delta.Text != "" {
			texts = append(texts, delta.Text)
		}
	}
	if got, want := strings.Join(texts, ""), "hello world"; got != want {
		t.Fatalf("streamed text = %q, want %q", got, want)
	}

	result, runErr := run.Wait()
	if runErr != nil {
		t.Fatalf("Wait() error = %v", runErr)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
	if len(result.Messages) != 3 {
		t.Fatalf("transcript length = %d, want 3", len(result.Messages))
	}
	if result.Messages[2].Role != RoleAssistant || result.Messages[2].Content != "hello world" {
		t.Fatalf("final assistant message = %#v", result.Messages[2])
	}
	if result.Metadata["request_id"] != "req-1" {
		t.Fatalf("metadata = %#v", result.Metadata)
	}
}

func TestAgentRunStreamUsesResolvedModelAndInstructions(t *testing.T) {
	t.Parallel()

	configured := newStreamScriptedModel(textDeltas("configured"))
	selected := newStreamScriptedModel(textDeltas("selected"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Instructions: "configured", Model: "configured-model"},
		Model:      configured,
		InstructionsResolver: func(context.Context, RunInput) (string, error) {
			return "resolved", nil
		},
		ModelResolver: func(context.Context, RunInput) (ModelSelection, error) {
			return ModelSelection{Model: selected, ModelName: "resolved-model"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := agent.RunStream(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	for range run.Deltas {
	}
	result, err := run.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[0].Content != "resolved" || result.Messages[len(result.Messages)-1].Content != "selected" {
		t.Fatalf("result messages = %#v", result.Messages)
	}
	if len(configured.Calls()) != 0 || len(selected.Calls()) != 1 || selected.Calls()[0].Model != "resolved-model" {
		t.Fatalf("configured calls = %#v, selected calls = %#v", configured.Calls(), selected.Calls())
	}
}

func TestAgentRunStreamEquivalenceWithRunForTextOnly(t *testing.T) {
	t.Parallel()

	streamModel := newStreamScriptedModel(textDeltas("hello back"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "echo", Model: "fixture-model"},
		Model:      streamModel,
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := agent.RunStream(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("RunStream() setup error = %v", err)
	}
	defer run.Cancel()

	streamResult, streamErr := run.Drain()
	if streamErr != nil {
		t.Fatalf("Drain() error = %v", streamErr)
	}

	genModel := newScriptedModel(textResponse("hello back"))
	genAgent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "echo", Model: "fixture-model"},
		Model:      genModel,
	})
	if err != nil {
		t.Fatal(err)
	}
	genResult, genErr := genAgent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if genErr != nil {
		t.Fatalf("Run() error = %v", genErr)
	}

	if streamResult.Status != genResult.Status {
		t.Fatalf("status = %q, want %q", streamResult.Status, genResult.Status)
	}
	if len(streamResult.Messages) != len(genResult.Messages) {
		t.Fatalf("transcript length = %d, want %d", len(streamResult.Messages), len(genResult.Messages))
	}
	if streamResult.Messages[len(streamResult.Messages)-1].Content != genResult.Messages[len(genResult.Messages)-1].Content {
		t.Fatalf("final content mismatch: stream=%q gen=%q",
			streamResult.Messages[len(streamResult.Messages)-1].Content,
			genResult.Messages[len(genResult.Messages)-1].Content)
	}
}

func TestAgentRunStreamRouterTracksAttemptsAndUsesProviderStreaming(t *testing.T) {
	t.Parallel()

	primary := &streamFailModel{err: &ModelError{Kind: ModelErrorUnavailable, Message: "primary unavailable"}}
	fallback := newStreamScriptedModel(textDeltas("from fallback"))
	registry := NewProviderRegistry()
	if err := registry.Register(ProviderEntry{ID: "primary", Model: primary}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(ProviderEntry{ID: "fallback", Model: fallback}); err != nil {
		t.Fatal(err)
	}
	router, err := NewModelRouter(ModelRouterConfig{
		Registry: registry,
		Policy:   RoutingPolicy{Primary: "primary"},
		Fallback: &FallbackPolicy{Chain: []ProviderID{"fallback"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := NewRunRecorder()
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "router", Model: "fixture-model"},
		Router:     router,
		Listener:   recorder,
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := agent.RunStream(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "hello"}}})
	if err != nil {
		t.Fatalf("RunStream() error = %v", err)
	}
	result, err := run.Drain()
	if err != nil {
		t.Fatalf("Drain() error = %v", err)
	}
	if primary.streamCalls != 1 {
		t.Fatalf("primary streaming calls = %d, want 1", primary.streamCalls)
	}
	if got := len(fallback.Calls()); got != 1 {
		t.Fatalf("fallback streaming calls = %d, want 1", got)
	}
	if got := result.ModelAttempts; len(got) != 2 ||
		got[0].Provider != "primary" || got[0].Status != ModelAttemptFallback || got[0].Error != primary.err ||
		got[1].Provider != "fallback" || got[1].Status != ModelAttemptSuccess {
		t.Fatalf("ModelAttempts = %#v", got)
	}

	var events []RunEvent
	for _, event := range recorder.Events() {
		if event.Type == RunEventModelAttemptStarted || event.Type == RunEventModelAttemptFinished {
			events = append(events, event)
		}
	}
	if got, want := len(events), 4; got != want {
		t.Fatalf("attempt events = %d, want %d: %#v", got, want, events)
	}
	if events[0].Type != RunEventModelAttemptStarted || events[0].Provider != "primary" ||
		events[1].Type != RunEventModelAttemptFinished || events[1].Provider != "primary" || events[1].AttemptStatus != ModelAttemptFallback ||
		events[2].Type != RunEventModelAttemptStarted || events[2].Provider != "fallback" ||
		events[3].Type != RunEventModelAttemptFinished || events[3].Provider != "fallback" || events[3].AttemptStatus != ModelAttemptSuccess {
		t.Fatalf("attempt events = %#v", events)
	}
}

func TestAgentRunStreamToolCallThenFinalText(t *testing.T) {
	t.Parallel()

	registry, handler := newAgentTestRegistry(t)
	handler.execute = func(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
		return append(json.RawMessage(nil), input...), nil
	}

	encoded, err := NewModelToolCalls(ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{"city":"Nairobi"}`)})
	if err != nil {
		t.Fatal(err)
	}

	model := newStreamScriptedModel(
		toolCallDeltaStream(ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{"city":"Nairobi"}`)}),
		textDeltas("Nairobi 24.5"),
	)
	_ = encoded
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "weather", Model: "fixture-model", Tools: []ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := agent.RunStream(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "weather in Nairobi?"}},
	})
	if err != nil {
		t.Fatalf("RunStream() setup error = %v", err)
	}
	defer run.Cancel()

	var deltaCount int
	for range run.Deltas {
		deltaCount++
	}
	result, runErr := run.Wait()
	if runErr != nil {
		t.Fatalf("Wait() error = %v", runErr)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
	if deltaCount < 2 {
		t.Fatalf("delta count = %d, want at least 2", deltaCount)
	}
	if len(result.Messages) != 4 {
		t.Fatalf("transcript length = %d, want 4", len(result.Messages))
	}
	if result.Messages[3].Content != "Nairobi 24.5" {
		t.Fatalf("final content = %q", result.Messages[3].Content)
	}
}

func TestAgentRunStreamStructuredOutputValidation(t *testing.T) {
	t.Parallel()

	compiled := stubCompiledSchema{}
	model := newStreamScriptedModel(structuredDeltaStream(json.RawMessage(`{"ok":true}`)))
	agent, err := NewAgent(AgentConfig{
		Definition:   AgentDefinition{ID: "agent", Model: "fixture-model"},
		Model:        model,
		OutputSchema: &ModelOutputSchema{Name: "result", Schema: json.RawMessage(`{"type":"object"}`)},
		SchemaCompiler: stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
			return compiled, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := agent.RunStream(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "return JSON"}},
	})
	if err != nil {
		t.Fatalf("RunStream() setup error = %v", err)
	}
	defer run.Cancel()

	result, runErr := run.Drain()
	if runErr != nil {
		t.Fatalf("Drain() error = %v", runErr)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
	if output := result.StructuredOutput(); output == "" {
		t.Fatal("structured output is empty")
	}
}

func TestAgentRunStreamCancellationStopsActiveWork(t *testing.T) {
	t.Parallel()

	model := newStreamScriptedModel(textDeltas("never", "delivered"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model"},
		Model:      model,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	run, err := agent.RunStream(ctx, RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("RunStream() setup error = %v", err)
	}
	defer run.Cancel()

	cancel()

	result, runErr := run.Wait()
	if runErr == nil {
		t.Fatal("Wait() error = nil, want cancellation")
	}
	var agentErr *AgentError
	if !errors.As(runErr, &agentErr) || agentErr.Kind != AgentErrorCancelled {
		t.Fatalf("error = %v, want AgentErrorCancelled", runErr)
	}
	if !errors.Is(runErr, ErrAgentCancelled) {
		t.Fatalf("errors.Is(ErrAgentCancelled) = false")
	}
	if result.Status != RunStatusCancelled {
		t.Fatalf("status = %q, want cancelled", result.Status)
	}
}

func TestAgentRunStreamCallerAbandonNoLeak(t *testing.T) {
	t.Parallel()

	model := newStreamScriptedModel(textDeltas("hello", "world"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model"},
		Model:      model,
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := agent.RunStream(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("RunStream() setup error = %v", err)
	}

	run.Cancel()
	_, _ = run.Wait()
}

func TestAgentRunStreamFallsBackToGenerateWhenNotStreaming(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(textResponse("hello back"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "echo", Model: "fixture-model"},
		Model:      model,
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := agent.RunStream(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("RunStream() setup error = %v", err)
	}
	defer run.Cancel()

	var deltaCount int
	for range run.Deltas {
		deltaCount++
	}
	if deltaCount != 1 {
		t.Fatalf("delta count = %d, want 1 (Generate fallback emits single delta)", deltaCount)
	}
	result, runErr := run.Wait()
	if runErr != nil {
		t.Fatalf("Wait() error = %v", runErr)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
	if result.Messages[len(result.Messages)-1].Content != "hello back" {
		t.Fatalf("final content = %q", result.Messages[len(result.Messages)-1].Content)
	}
}

func TestAgentRunStreamEmitsDeltaRunEvents(t *testing.T) {
	t.Parallel()

	recorder := NewRunRecorder()
	model := newStreamScriptedModel(textDeltas("hello", "world"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "echo", Model: "fixture-model"},
		Model:      model,
		Listener:   recorder,
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := agent.RunStream(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("RunStream() setup error = %v", err)
	}
	defer run.Cancel()

	if _, err := run.Drain(); err != nil {
		t.Fatalf("Drain() error = %v", err)
	}

	events := recorder.Events()
	var deltaEvents int
	for _, event := range events {
		if event.Type == RunEventDelta {
			deltaEvents++
		}
	}
	if deltaEvents < 2 {
		t.Fatalf("delta events = %d, want at least 2", deltaEvents)
	}
	terminal, ok := recorder.TerminalEvent()
	if !ok || terminal.Type != RunEventSucceeded {
		t.Fatalf("terminal event = %#v, want run_succeeded", terminal)
	}
}

func TestStreamDeltaValidateRejectsEmptyDelta(t *testing.T) {
	t.Parallel()
	if err := (StreamDelta{}).Validate(); err == nil {
		t.Fatal("empty delta should fail validation")
	}
}

func TestStreamDeltaValidateRejectsInvalidStructuredOutput(t *testing.T) {
	t.Parallel()
	delta := StreamDelta{StructuredOutput: NewModelStructuredOutput(json.RawMessage(`{invalid`))}
	if err := delta.Validate(); err == nil {
		t.Fatal("invalid structured output should fail validation")
	}
}

func TestStreamDeltaIsTerminal(t *testing.T) {
	t.Parallel()
	if !(StreamDelta{FinishReason: FinishReasonStop}).IsTerminal() {
		t.Fatal("finish reason stop should be terminal")
	}
	if !(StreamDelta{Err: errors.New("boom")}).IsTerminal() {
		t.Fatal("error delta should be terminal")
	}
	if (StreamDelta{Text: "hi"}).IsTerminal() {
		t.Fatal("text delta should not be terminal")
	}
}

func TestAsStreamingModelReturnsNilForNonStreaming(t *testing.T) {
	t.Parallel()
	if got := AsStreamingModel(echoModel{}); got != nil {
		t.Fatalf("AsStreamingModel(echoModel) = %v, want nil", got)
	}
}

func TestAsStreamingModelReturnsStreamingAdapter(t *testing.T) {
	t.Parallel()
	model := newStreamScriptedModel(textDeltas("hi"))
	if got := AsStreamingModel(model); got == nil {
		t.Fatal("AsStreamingModel(streamScriptedModel) = nil, want StreamingModel")
	}
}

func TestStreamReaderFuncDefaultsToEOF(t *testing.T) {
	t.Parallel()
	reader := &StreamReaderFunc{}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next() error = %v, want io.EOF", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestAgentRunStreamHonoursPreCancelledContext(t *testing.T) {
	t.Parallel()

	model := newStreamScriptedModel(textDeltas("never"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model"},
		Model:      model,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = agent.RunStream(ctx, RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err == nil {
		t.Fatal("RunStream() error = nil, want cancellation")
	}
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != AgentErrorCancelled {
		t.Fatalf("error = %v, want AgentErrorCancelled", err)
	}
}

func TestAgentRunStreamStepLimitExhausted(t *testing.T) {
	t.Parallel()

	var streams [][]StreamDelta
	for i := 0; i < 3; i++ {
		streams = append(streams, toolCallDeltaStream(ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{}`)}))
	}
	registry, _ := newAgentTestRegistry(t)
	model := newStreamScriptedModel(streams...)
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model", Tools: []ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
		MaxSteps:   3,
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := agent.RunStream(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "loop"}},
	})
	if err != nil {
		t.Fatalf("RunStream() setup error = %v", err)
	}
	defer run.Cancel()

	_, runErr := run.Drain()
	if runErr == nil {
		t.Fatal("Drain() error = nil, want step limit exhausted")
	}
	if !errors.Is(runErr, ErrAgentStepLimitExhausted) {
		t.Fatalf("error = %v, want ErrAgentStepLimitExhausted", runErr)
	}
}

func TestAgentRunStreamProviderFailure(t *testing.T) {
	t.Parallel()

	model := newStreamScriptedModel([]StreamDelta{{Err: errors.New("provider down")}})
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model"},
		Model:      model,
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := agent.RunStream(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("RunStream() setup error = %v", err)
	}
	defer run.Cancel()

	_, runErr := run.Drain()
	if runErr == nil {
		t.Fatal("Drain() error = nil, want provider failure")
	}
	var agentErr *AgentError
	if !errors.As(runErr, &agentErr) || agentErr.Kind != AgentErrorProviderFailure {
		t.Fatalf("error = %v, want AgentErrorProviderFailure", runErr)
	}
}

func TestAgentRunStreamTimeoutCancellation(t *testing.T) {
	t.Parallel()

	slowModel := newStreamScriptedModel(textDeltas("hello"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model"},
		Model:      slowModel,
		Deadline:   1 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := agent.RunStream(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("RunStream() setup error = %v", err)
	}
	defer run.Cancel()

	time.Sleep(20 * time.Millisecond)
	_, runErr := run.Drain()
	if runErr == nil {
		t.Fatal("Drain() error = nil, want deadline cancellation")
	}
	var agentErr *AgentError
	if !errors.As(runErr, &agentErr) || agentErr.Kind != AgentErrorCancelled {
		t.Fatalf("error = %v, want AgentErrorCancelled", runErr)
	}
}
