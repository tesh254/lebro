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

func TestAgentRunStreamResolvesUnavailableCostOnce(t *testing.T) {
	t.Parallel()

	model := newStreamScriptedModel([]StreamDelta{
		{Text: "hi"},
		{FinishReason: FinishReasonStop, Usage: ModelUsage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3}},
	})
	calls := 0
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "cost-stream", Model: "fixture-model", Instructions: "be brief"},
		Model:      model,
		CostResolver: CostResolverFunc(func(context.Context, ModelAttemptRecord) (ModelCost, error) {
			calls++
			return ModelCost{}, errors.New("pricing service offline")
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := agent.RunStream(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("RunStream() setup error = %v", err)
	}
	defer run.Cancel()

	var last StreamDelta
	for delta := range run.Deltas {
		last = delta
	}
	result, runErr := run.Wait()
	if runErr != nil || result.Status != RunStatusSucceeded {
		t.Fatalf("stream result %+v %v", result, runErr)
	}
	if calls != 1 {
		t.Fatalf("cost resolver invoked %d times, want 1", calls)
	}
	if len(last.Accounting.Costs) != 1 || last.Accounting.Costs[0].Source != CostUnavailable {
		t.Fatalf("terminal delta accounting = %#v", last.Accounting)
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

func TestAgentRunStreamPreservesReasoningDeltasAndTranscript(t *testing.T) {
	t.Parallel()

	details := NewModelReasoningDetails(json.RawMessage(`[{"type":"thinking","signature":"opaque"}]`))
	model := newStreamScriptedModel([]StreamDelta{
		{Reasoning: ModelReasoning{Text: "first "}},
		{Text: "answer"},
		{Reasoning: ModelReasoning{Text: "second", Details: details}, Usage: ModelUsage{ReasoningTokens: 7, TotalTokens: 9}},
		{FinishReason: FinishReasonStop},
	})
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "reasoning", Model: "fixture-model"},
		Model:      model,
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := agent.RunStream(context.Background(), RunInput{
		Messages:  []Message{{Role: RoleUser, Content: "solve"}},
		Reasoning: ReasoningConfig{Effort: ReasoningMedium},
	})
	if err != nil {
		t.Fatal(err)
	}
	var reasoning strings.Builder
	for delta := range run.Deltas {
		reasoning.WriteString(delta.Reasoning.Text)
	}
	result, err := run.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := reasoning.String(), "first second"; got != want {
		t.Fatalf("reasoning deltas = %q, want %q", got, want)
	}
	message := result.Messages[len(result.Messages)-1]
	if message.Content != "answer" || message.Reasoning.Text != "first second" {
		t.Fatalf("assistant message = %#v", message)
	}
	if got := string(message.Reasoning.Details.Raw()); got != string(details.Raw()) {
		t.Fatalf("reasoning details = %s, want %s", got, details)
	}
	calls := model.Calls()
	if len(calls) != 1 || calls[0].Reasoning != (ReasoningConfig{Effort: ReasoningMedium}) {
		t.Fatalf("model calls = %#v", calls)
	}
}

func TestAgentRunStreamRejectsInvalidReasoningConfigBeforeModelCall(t *testing.T) {
	t.Parallel()

	model := newStreamScriptedModel(textDeltas("unused"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "reasoning", Model: "fixture-model"},
		Model:      model,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = agent.RunStream(context.Background(), RunInput{Reasoning: ReasoningConfig{Effort: "unsupported"}})
	if err == nil || !strings.Contains(err.Error(), "unsupported reasoning effort") {
		t.Fatalf("RunStream() error = %v", err)
	}
	if calls := model.Calls(); len(calls) != 0 {
		t.Fatalf("model calls = %#v, want none", calls)
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

func TestAgentRunStreamNormalizesResolverFailure(t *testing.T) {
	t.Parallel()

	recorder := NewRunRecorder()
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent"},
		Model:      newStreamScriptedModel(textDeltas("unused")),
		Listener:   recorder,
		ModelResolver: func(context.Context, RunInput) (ModelSelection, error) {
			return ModelSelection{}, errors.New("tenant unavailable")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := agent.RunStream(context.Background(), RunInput{})
	if run != nil || !errors.Is(err, ErrAgentResolver) {
		t.Fatalf("run = %#v, error = %v", run, err)
	}
	terminal, ok := recorder.TerminalEvent()
	if !ok || terminal.Type != RunEventFailed || !errors.Is(terminal.Error, ErrAgentResolver) {
		t.Fatalf("terminal event = %#v", terminal)
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

func TestAgentRunStreamTimeoutPreservesPartialText(t *testing.T) {
	t.Parallel()

	model := newStreamScriptedModel([]StreamDelta{
		{Text: "almost complete"},
		{Err: &ModelError{Kind: ModelErrorTimeout, Provider: "fixture", Message: "stream idle timeout exceeded"}},
	})
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model"},
		Model:      model,
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := agent.RunStream(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "hello"}}})
	if err != nil {
		t.Fatalf("RunStream() setup error = %v", err)
	}
	defer run.Cancel()

	result, runErr := run.Drain()
	if !errors.Is(runErr, ErrAgentTimeout) {
		t.Fatalf("error = %v, want ErrAgentTimeout", runErr)
	}
	var agentErr *AgentError
	if !errors.As(runErr, &agentErr) || agentErr.Kind != AgentErrorTimeout {
		t.Fatalf("error = %v, want AgentErrorTimeout", runErr)
	}
	if result.Status != RunStatusFailed || len(result.Messages) != 2 || result.Messages[1].Content != "almost complete" {
		t.Fatalf("result = %#v, want failed result with partial assistant text", result)
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

// TestAgentRunStreamTypesStreamAggregationFailureAsMalformedResponse feeds a
// deliberately broken stream — duplicate tool-call IDs at the aggregation
// boundary, independent of any adapter — and pins the typed failure contract:
// the nested *ModelError is discoverable through errors.As, the broad
// AgentError wrapper is preserved, the cause is retained, and the
// deterministic failure is not retryable.
func TestAgentRunStreamTypesStreamAggregationFailureAsMalformedResponse(t *testing.T) {
	t.Parallel()

	call := ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{}`)}
	model := newStreamScriptedModel([]StreamDelta{
		{ToolCall: &call},
		{ToolCall: &call},
		{FinishReason: FinishReasonToolCalls},
	})
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "dup-stream", Model: "fixture-model"},
		Model:      model,
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := agent.RunStream(context.Background(), RunInput{RunID: "dup-run", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("RunStream() setup error = %v", err)
	}
	defer run.Cancel()
	for range run.Deltas {
	}
	result, runErr := run.Wait()
	if runErr == nil {
		t.Fatal("Wait() error = nil, want aggregation failure")
	}
	if result.Status != RunStatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	var agentErr *AgentError
	if !errors.As(runErr, &agentErr) || agentErr.Kind != AgentErrorProviderFailure {
		t.Fatalf("error = %v, want AgentErrorProviderFailure wrapper", runErr)
	}
	var modelErr *ModelError
	if !errors.As(runErr, &modelErr) || modelErr.Kind != ModelErrorMalformedResponse {
		t.Fatalf("error = %v, want nested ModelErrorMalformedResponse", runErr)
	}
	if modelErr.Err == nil || !strings.Contains(modelErr.Err.Error(), "duplicate model tool call ID") {
		t.Fatalf("cause = %v, want duplicate tool call ID", modelErr.Err)
	}
	if modelErr.Retryable() {
		t.Fatal("malformed response must not be retryable")
	}
	if len(result.Messages) != 1 {
		t.Fatalf("transcript = %#v, want user message only (no partial assistant content)", result.Messages)
	}
}

// TestAgentRunStreamTypesProcessorProducedDuplicateAsMalformedResponse covers
// the synthesized-delta path: a non-streaming model returns valid tool calls
// and a stream-delta processor rewrites one call's ID into a duplicate. The
// aggregation failure after processors must expose the same typed
// malformed-response cause instead of an untyped encoding error, and the
// pre-processor partial response stays available.
func TestAgentRunStreamTypesProcessorProducedDuplicateAsMalformedResponse(t *testing.T) {
	t.Parallel()

	calls, err := NewModelToolCalls(
		ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{}`)},
		ModelToolCall{ID: "call-2", ToolID: "ping", Arguments: json.RawMessage(`{}`)},
	)
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := NewProcessorPipeline(runtimeProcessor{name: "rewriter", delta: func(request ProcessorStreamDeltaRequest) ProcessorStreamDeltaResult {
		if request.Delta.ToolCall != nil && request.Delta.ToolCall.ID == "call-2" {
			rewritten := *request.Delta.ToolCall
			rewritten.ID = "call-1"
			request.Delta.ToolCall = &rewritten
		}
		return ProcessorStreamDeltaResult{Decision: ProcessorDecision{Kind: ProcessorTransform}, Delta: request.Delta}
	}})
	if err != nil {
		t.Fatal(err)
	}
	model := newScriptedModel(scriptedResponse{response: ModelResponse{
		Message:      Message{Role: RoleAssistant, ToolCalls: calls},
		FinishReason: FinishReasonToolCalls,
	}})
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "dup-processor", Model: "fixture-model"},
		Model:      model,
		Processors: pipeline,
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := agent.RunStream(context.Background(), RunInput{RunID: "dup-processor-run", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("RunStream() setup error = %v", err)
	}
	defer run.Cancel()
	for range run.Deltas {
	}
	result, runErr := run.Wait()
	if runErr == nil {
		t.Fatal("Wait() error = nil, want processor-produced duplicate failure")
	}
	var agentErr *AgentError
	if !errors.As(runErr, &agentErr) || agentErr.Kind != AgentErrorProviderFailure {
		t.Fatalf("error = %v, want AgentErrorProviderFailure wrapper", runErr)
	}
	var modelErr *ModelError
	if !errors.As(runErr, &modelErr) || modelErr.Kind != ModelErrorMalformedResponse {
		t.Fatalf("error = %v, want nested ModelErrorMalformedResponse", runErr)
	}
	if modelErr.Err == nil || !strings.Contains(modelErr.Err.Error(), "duplicate model tool call ID") {
		t.Fatalf("cause = %v, want duplicate tool call ID", modelErr.Err)
	}
	if result.Status != RunStatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}
}

// deltaFailureProcessor fails the stream-delta phase so the run fails after
// deltas were received, exercising processor classification at the stream
// boundary.
type deltaFailureProcessor struct{}

func (deltaFailureProcessor) Name() string { return "delta-fail" }
func (deltaFailureProcessor) ProcessStreamDelta(context.Context, ProcessorStreamDeltaRequest) (ProcessorStreamDeltaResult, error) {
	return ProcessorStreamDeltaResult{}, errors.New("redaction backend offline")
}

// TestAgentRunStreamKeepsProcessorFailuresUntypedAsModelErrors guards the
// classification boundary: a stream-delta processor that fails mid-stream is
// a processor failure, not a malformed provider response.
func TestAgentRunStreamKeepsProcessorFailuresUntypedAsModelErrors(t *testing.T) {
	t.Parallel()

	pipeline, err := NewProcessorPipeline(deltaFailureProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	model := newStreamScriptedModel(textDeltas("hello"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "processor-fail", Model: "fixture-model"},
		Model:      model,
		Processors: pipeline,
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := agent.RunStream(context.Background(), RunInput{RunID: "processor-fail-run", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("RunStream() setup error = %v", err)
	}
	defer run.Cancel()
	for range run.Deltas {
	}
	_, runErr := run.Wait()
	var modelErr *ModelError
	if errors.As(runErr, &modelErr) {
		t.Fatalf("error = %v, want processor classification not model error", runErr)
	}
	var agentErr *AgentError
	if !errors.As(runErr, &agentErr) || agentErr.Kind != AgentErrorProcessor {
		t.Fatalf("error = %v, want AgentErrorProcessor", runErr)
	}
}

// TestAgentRunStreamRetainsObservedUsageOnFailedCall replays the session
// failure end to end: the provider reports usage and a request identity, the
// runtime rejects the duplicated tool calls, and the durable diagnostics must
// keep the observed metadata on the failed attempt and the model-finished
// failure event while the run still fails and persists no transcript.
func TestAgentRunStreamRetainsObservedUsageOnFailedCall(t *testing.T) {
	t.Parallel()

	call := ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{}`)}
	model := newStreamScriptedModel([]StreamDelta{
		{ToolCall: &call},
		{ToolCall: &call},
		{
			FinishReason: FinishReasonToolCalls,
			Usage:        ModelUsage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12},
			Accounting:   ModelAccounting{ProviderRequestID: "req-1"},
		},
	})
	store := NewMemoryStore()
	resolverCalls := 0
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "fail-usage", Model: "fixture-model"},
		Model:      model,
		Store:      store,
		CostResolver: CostResolverFunc(func(context.Context, ModelAttemptRecord) (ModelCost, error) {
			resolverCalls++
			return ModelCost{}, errors.New("pricing service offline")
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := agent.RunStream(context.Background(), RunInput{RunID: "fail-usage-run", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("RunStream() setup error = %v", err)
	}
	defer run.Cancel()
	for range run.Deltas {
	}
	result, runErr := run.Wait()
	if runErr == nil || result.Status != RunStatusFailed {
		t.Fatalf("result = %+v %v, want failed run", result, runErr)
	}
	var modelErr *ModelError
	if !errors.As(runErr, &modelErr) || modelErr.Kind != ModelErrorMalformedResponse {
		t.Fatalf("error = %v, want typed malformed response", runErr)
	}

	ctx := context.Background()
	attempts, err := store.ModelAttempts().ListModelAttempts(ctx, ModelAttemptFilter{RunID: "fail-usage-run"}, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts.Records) != 1 {
		t.Fatalf("persisted attempts = %d, want 1", len(attempts.Records))
	}
	attempt := attempts.Records[0]
	if attempt.Status != ModelAttemptFailed || attempt.Usage != (ModelUsage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}) {
		t.Fatalf("failed attempt = %#v, want failed status with observed usage", attempt)
	}
	if attempt.Accounting.ProviderRequestID != "req-1" || attempt.FinishReason != FinishReasonToolCalls {
		t.Fatalf("failed attempt identity = %#v finish %q", attempt.Accounting, attempt.FinishReason)
	}

	events, err := store.RunEvents().ListRunEvents(ctx, RunEventFilter{RunID: "fail-usage-run", Type: RunEventModelFinished}, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Records) != 1 {
		t.Fatalf("model_finished events = %d, want 1", len(events.Records))
	}
	event := events.Records[0]
	if event.ErrorKind != string(AgentErrorProviderFailure) || event.Usage != (ModelUsage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}) || event.Accounting.ProviderRequestID != "req-1" || event.FinishReason != FinishReasonToolCalls {
		t.Fatalf("failure event = %#v finish %q", event, event.FinishReason)
	}
	if resolverCalls != 1 {
		t.Fatalf("cost resolver invoked %d times, want 1 (at the terminal delta, never re-resolved on the failure path)", resolverCalls)
	}

	messages, err := store.Messages().ListMessages(ctx, ThreadID(""), PageRequest{})
	if err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if len(messages.Records) != 0 {
		t.Fatalf("failed run must not commit a canonical transcript, got %#v", messages.Records)
	}
}

// TestAgentRunStreamExecutionIdentitySeparatesRetries proves the retry-safe
// diagnostics contract end to end: two executions of one logical run with
// distinct ExecutionIDs persist distinct, queryable attempts, events, and
// tool records, while an execution without an identity keeps the legacy
// single-execution ID format.
func TestAgentRunStreamExecutionIdentitySeparatesRetries(t *testing.T) {
	t.Parallel()

	answer := textDeltas("done")
	model := newStreamScriptedModel(toolCallDeltaStream(ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{}`)}), answer, answer, answer, answer, answer)
	registry, _ := newAgentTestRegistry(t)
	store := NewMemoryStore()
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "exec-agent", Model: "fixture-model", Tools: []ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
		Store:      store,
	})
	if err != nil {
		t.Fatal(err)
	}

	runStream := func(executionID string) error {
		run, err := agent.RunStream(context.Background(), RunInput{
			RunID:       "exec-run",
			ExecutionID: executionID,
			Messages:    []Message{{Role: RoleUser, Content: "hi"}},
		})
		if err != nil {
			return err
		}
		defer run.Cancel()
		for range run.Deltas {
		}
		_, waitErr := run.Wait()
		return waitErr
	}
	if err := runStream("exec-a"); err != nil {
		t.Fatalf("execution A error = %v", err)
	}
	if err := runStream("exec-b"); err != nil {
		t.Fatalf("execution B error = %v", err)
	}
	// A third execution without an identity keeps the legacy format.
	if err := runStream(""); err != nil {
		t.Fatalf("legacy execution error = %v", err)
	}

	ctx := context.Background()
	attempts, err := store.ModelAttempts().ListModelAttempts(ctx, ModelAttemptFilter{RunID: "exec-run"}, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts.Records) != 4 {
		t.Fatalf("persisted attempts = %d, want 4", len(attempts.Records))
	}
	wantIDs := []string{"exec-run-exec-a-attempt-1", "exec-run-exec-a-attempt-2", "exec-run-exec-b-attempt-1", "exec-run-attempt-1"}
	for i, attempt := range attempts.Records {
		if attempt.ID != wantIDs[i] {
			t.Fatalf("attempt[%d].ID = %q, want %q", i, attempt.ID, wantIDs[i])
		}
	}
	scoped, err := store.ModelAttempts().ListModelAttempts(ctx, ModelAttemptFilter{RunID: "exec-run", ExecutionID: "exec-b"}, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped.Records) != 1 || scoped.Records[0].ID != "exec-run-exec-b-attempt-1" {
		t.Fatalf("execution-scoped attempts = %#v", scoped.Records)
	}

	tools, err := store.ToolExecutions().ListToolExecutions(ctx, ToolExecutionFilter{RunID: "exec-run"}, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Records) != 1 {
		t.Fatalf("persisted tool executions = %d, want 1 (only execution A called the tool)", len(tools.Records))
	}
	if tools.Records[0].ExecutionID != "exec-a" || tools.Records[0].ID != "exec-run-exec-a-tool-1" {
		t.Fatalf("tool execution = %#v, want execution A's identity-scoped record", tools.Records[0])
	}
	toolScoped, err := store.ToolExecutions().ListToolExecutions(ctx, ToolExecutionFilter{RunID: "exec-run", ExecutionID: "exec-b"}, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(toolScoped.Records) != 0 {
		t.Fatalf("execution-scoped tools = %#v, want none (execution B never called a tool)", toolScoped.Records)
	}

	events, err := store.RunEvents().ListRunEvents(ctx, RunEventFilter{RunID: "exec-run"}, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// The legacy execution's event identity stays sequence-scoped; the two
	// identity-bearing executions never collide with it or each other.
	seen := map[string]int{}
	for _, event := range events.Records {
		seen[event.ID]++
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("event %q persisted %d times, want 1", id, count)
		}
	}
	if len(events.Records) != len(seen) {
		t.Fatalf("event IDs = %d records but %d unique IDs", len(events.Records), len(seen))
	}
}

// observingTool records every delivered observation for a run.
type observingTool struct {
	mu           sync.Mutex
	observations []ToolExecutionObservation
	panicOnFirst bool
	panicked     bool
}

func (t *observingTool) observe(o ToolExecutionObservation) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.panicOnFirst && !t.panicked {
		t.panicked = true
		panic("observer bug")
	}
	t.observations = append(t.observations, o)
}

func (t *observingTool) snapshot() []ToolExecutionObservation {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]ToolExecutionObservation(nil), t.observations...)
}

// TestAgentToolObserverReceivesRealPayloads pins the opted-in capture
// contract: the observer sees the actual arguments and result at the
// execution boundary — including when a later model step fails — with
// run/execution/step/call correlation, while the durable diagnostics stay
// content-free.
func TestAgentToolObserverReceivesRealPayloads(t *testing.T) {
	t.Parallel()

	model := newStreamScriptedModel(toolCallDeltaStream(ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{"q":"weather"}`)}), []StreamDelta{{Err: &ModelError{Kind: ModelErrorTransport, Provider: "fixture", Message: "provider dropped the connection"}}}, textDeltas("unused"))
	registry, _ := newAgentTestRegistry(t)
	watcher := &observingTool{}
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "watch-agent", Model: "fixture-model", Tools: []ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
		ToolObserver: ToolResultObserverFunc(func(o ToolExecutionObservation) {
			watcher.observe(o)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	stream, streamErr := agent.RunStream(context.Background(), RunInput{
		RunID:       "watch-run",
		ExecutionID: "watch-exec",
		Messages:    []Message{{Role: RoleUser, Content: "hi"}},
	})
	if streamErr != nil {
		t.Fatal(streamErr)
	}
	result, runErr := stream.Drain()
	if result.Status != RunStatusFailed {
		t.Fatalf("status = %q, want failed (the later model step fails)", result.Status)
	}
	if runErr == nil {
		t.Fatal("error = nil, want later model failure")
	}

	observations := watcher.snapshot()
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want 1", len(observations))
	}
	observed := observations[0]
	if observed.RunID != "watch-run" || observed.ExecutionID != "watch-exec" || observed.Step != 1 || observed.ToolCallID != "call-1" || observed.ToolID != "lookup" {
		t.Fatalf("correlation = %#v", observed)
	}
	if string(observed.Arguments) != `{"q":"weather"}` {
		t.Fatalf("arguments = %s", observed.Arguments)
	}
	if string(observed.Result) != `{}` {
		t.Fatalf("result = %s, want the real tool output", observed.Result)
	}
	if observed.State != ToolExecutionSucceeded || observed.Err != nil {
		t.Fatalf("state = %q err = %v, want success", observed.State, observed.Err)
	}
	if observed.StartedAt.IsZero() || observed.FinishedAt.IsZero() || observed.FinishedAt.Before(observed.StartedAt) {
		t.Fatalf("timestamps = %v -> %v", observed.StartedAt, observed.FinishedAt)
	}
}

// TestAgentToolObserverReceivesFailureOutcome covers error outcomes: a tool
// whose handler fails delivers its state and error (never the arguments in
// the place of a result), and a cancelled tool delivers its cancelled state.
func TestAgentToolObserverReceivesFailureOutcome(t *testing.T) {
	t.Parallel()

	model := newStreamScriptedModel(toolCallDeltaStream(ModelToolCall{ID: "call-err", ToolID: "boom", Arguments: json.RawMessage(`{"boom":true}`)}))
	failingTool := &agentTestTool{
		definition: ToolDefinition{ID: "boom", InputSchema: json.RawMessage(`{"type":"object"}`)},
		execute: func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return nil, errors.New("upstream unreachable")
		},
	}
	registry, err := NewToolRegistry(stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) { return stubCompiledSchema{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(failingTool); err != nil {
		t.Fatal(err)
	}
	watcher := &observingTool{}
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "fail-tool-agent", Model: "fixture-model", Tools: []ToolID{"boom"}},
		Model:      model,
		Tools:      registry,
		ToolObserver: ToolResultObserverFunc(func(o ToolExecutionObservation) {
			watcher.observe(o)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	stream, streamErr := agent.RunStream(context.Background(), RunInput{RunID: "fail-tool-run", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if streamErr != nil {
		t.Fatal(streamErr)
	}
	result, runErr := stream.Drain()
	if runErr == nil || result.Status != RunStatusFailed {
		t.Fatalf("result = %+v %v, want failed run", result, runErr)
	}
	observations := watcher.snapshot()
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want 1", len(observations))
	}
	observed := observations[0]
	if observed.State != ToolExecutionHandlerError || observed.Err == nil {
		t.Fatalf("state = %q err = %v, want handler error", observed.State, observed.Err)
	}
	if len(observed.Result) != 0 {
		t.Fatalf("result = %s, want empty for a failed execution", observed.Result)
	}
	if string(observed.Arguments) != `{"boom":true}` {
		t.Fatalf("arguments = %s", observed.Arguments)
	}
}

// TestAgentToolObserverPanicIsContainedAndOrdered proves an observer bug
// cannot affect the run: the panicking delivery is contained, the poisoned
// tool still runs, and the next observation still arrives, in execution
// order.
func TestAgentToolObserverPanicIsContainedAndOrdered(t *testing.T) {
	t.Parallel()

	model := newStreamScriptedModel(
		toolCallDeltaStream(ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{"one":1}`)}),
		toolCallDeltaStream(ModelToolCall{ID: "call-2", ToolID: "lookup", Arguments: json.RawMessage(`{"two":2}`)}),
		textDeltas("done"),
	)
	registry, _ := newAgentTestRegistry(t)
	watcher := &observingTool{panicOnFirst: true}
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "panic-agent", Model: "fixture-model", Tools: []ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
		ToolObserver: ToolResultObserverFunc(func(o ToolExecutionObservation) {
			watcher.observe(o)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	stream, streamErr := agent.RunStream(context.Background(), RunInput{RunID: "panic-run", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if streamErr != nil {
		t.Fatal(streamErr)
	}
	result, runErr := stream.Drain()
	if runErr != nil || result.Status != RunStatusSucceeded {
		t.Fatalf("result = %+v %v, want succeeded run despite the observer panic", result, runErr)
	}
	observations := watcher.snapshot()
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want only the second delivery (the panicking one was contained)", len(observations))
	}
	if string(observations[0].Arguments) != `{"two":2}` || observations[0].ToolCallID != "call-2" {
		t.Fatalf("delivered observation = %+#v, want the second tool call", observations[0])
	}
}

// blockingTool waits on its context so the test can cancel the run while the
// tool is executing, exercising the observer's cancelled-state delivery.
type blockingTool struct {
	definition ToolDefinition
	started    chan struct{}
}

func (t *blockingTool) Definition() ToolDefinition { return t.definition }
func (t *blockingTool) Execute(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	close(t.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestAgentToolObserverReceivesCancelledOutcome covers the cancellation
// branch of the capture contract: a tool cancelled through its context
// delivers ToolExecutionCancelled (never a fabricated success), and the run
// itself reports cancellation.
func TestAgentToolObserverReceivesCancelledOutcome(t *testing.T) {
	t.Parallel()

	model := newStreamScriptedModel(toolCallDeltaStream(ModelToolCall{ID: "call-1", ToolID: "waiter", Arguments: json.RawMessage(`{}`)}))
	tool := &blockingTool{definition: ToolDefinition{ID: "waiter", InputSchema: json.RawMessage(`{"type":"object"}`)}, started: make(chan struct{})}
	registry, err := NewToolRegistry(stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) { return stubCompiledSchema{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}
	watcher := &observingTool{}
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "cancel-agent", Model: "fixture-model", Tools: []ToolID{"waiter"}},
		Model:      model,
		Tools:      registry,
		ToolObserver: ToolResultObserverFunc(func(o ToolExecutionObservation) {
			watcher.observe(o)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	run, err := agent.RunStream(runCtx, RunInput{RunID: "cancel-obs-run", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("RunStream() setup error = %v", err)
	}
	defer run.Cancel()

	// Drain in the background: the run goroutine must be able to emit
	// deltas while the tool executes, or it would never reach the tool.
	drained := make(chan struct{})
	go func() {
		for range run.Deltas {
		}
		close(drained)
	}()

	<-tool.started
	cancelRun()
	<-drained
	result, runErr := run.Wait()
	if !errors.Is(runErr, ErrAgentCancelled) {
		t.Fatalf("error = %v, want cancellation", runErr)
	}
	if result.Status != RunStatusCancelled {
		t.Fatalf("status = %q, want cancelled", result.Status)
	}
	observations := watcher.snapshot()
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want 1", len(observations))
	}
	observed := observations[0]
	if observed.State != ToolExecutionCancelled || observed.Err == nil {
		t.Fatalf("state = %q err = %v, want cancelled", observed.State, observed.Err)
	}
	if len(observed.Result) != 0 {
		t.Fatalf("result = %s, want empty for a cancelled execution", observed.Result)
	}
}

// TestAgentRunObservesToolRuntime pins the observer's timestamps on the
// non-streaming Run path: StartedAt is sampled before the tool executes and
// FinishedAt when it returns, so the observation brackets the tool's real
// runtime instead of reporting an empty window.
func TestAgentRunObservesToolRuntime(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(toolCallResponse(ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{}`)}), textResponse("done"))
	registry, _ := newAgentTestRegistry(t)
	watcher := &observingTool{}
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "runtime-agent", Model: "fixture-model", Tools: []ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
		ToolObserver: ToolResultObserverFunc(func(o ToolExecutionObservation) {
			watcher.observe(o)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := agent.Run(context.Background(), RunInput{RunID: "runtime-run", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil || result.Status != RunStatusSucceeded {
		t.Fatalf("Run() = %+v %v, want success", result, err)
	}
	observations := watcher.snapshot()
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want 1", len(observations))
	}
	observed := observations[0]
	if observed.ExecutionID != "" || observed.RunID != "runtime-run" {
		t.Fatalf("correlation = %#v", observed)
	}
	if observed.State != ToolExecutionSucceeded || string(observed.Result) != `{}` {
		t.Fatalf("observation = %#v, want the real success payload", observed)
	}
	if !observed.FinishedAt.After(observed.StartedAt) {
		t.Fatalf("timestamps %v -> %v, want FinishedAt after StartedAt", observed.StartedAt, observed.FinishedAt)
	}
}

// TestAgentExecutionIDStableAcrossInputProcessorReplacement pins the
// single-execution-identity rule: an input processor that replaces the run
// input must not split the observer's correlation from the persisted
// diagnostics — both describe the pre-processor execution identity.
func TestAgentExecutionIDStableAcrossInputProcessorReplacement(t *testing.T) {
	t.Parallel()

	pipeline, err := NewProcessorPipeline(runtimeProcessor{name: "replacer", input: func(request ProcessorInputRequest) ProcessorInputResult {
		replaced := request.Input
		replaced.ExecutionID = "processor-invented-id"
		replaced.Metadata = map[string]string{"rewritten": "yes"}
		return ProcessorInputResult{Decision: ProcessorDecision{Kind: ProcessorTransform}, Input: replaced}
	}})
	if err != nil {
		t.Fatal(err)
	}
	model := newStreamScriptedModel(textDeltas("done"))
	store := NewMemoryStore()
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "identity-agent", Model: "fixture-model"},
		Model:      model,
		Processors: pipeline,
		Store:      store,
	})
	if err != nil {
		t.Fatal(err)
	}

	stream, streamErr := agent.RunStream(context.Background(), RunInput{
		RunID:       "identity-run",
		ExecutionID: "queue-exec-1",
		Messages:    []Message{{Role: RoleUser, Content: "hi"}},
	})
	if streamErr != nil {
		t.Fatal(streamErr)
	}
	result, runErr := stream.Drain()
	if runErr != nil || result.Status != RunStatusSucceeded {
		t.Fatalf("result = %+v %v, want success", result, runErr)
	}

	ctx := context.Background()
	events, err := store.RunEvents().ListRunEvents(ctx, RunEventFilter{RunID: "identity-run"}, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Records) == 0 {
		t.Fatal("no events persisted")
	}
	for _, event := range events.Records {
		if event.ExecutionID != "queue-exec-1" {
			t.Fatalf("event %q execution ID = %q, want the pre-processor identity", event.ID, event.ExecutionID)
		}
	}
	if _, err := store.RunEvents().ListRunEvents(ctx, RunEventFilter{RunID: "identity-run", ExecutionID: "processor-invented-id"}, PageRequest{}); err != nil {
		t.Fatal(err)
	}
	scoped, err := store.RunEvents().ListRunEvents(ctx, RunEventFilter{RunID: "identity-run", ExecutionID: "processor-invented-id"}, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped.Records) != 0 {
		t.Fatalf("processor-invented execution ID leaked into diagnostics: %+v", scoped.Records)
	}
}

// TestAgentRunRejectsNULInExecutionID pins the validation: PostgreSQL text
// columns cannot store NUL, so an execution identity containing one must fail
// the run before any diagnostics are produced rather than silently losing
// them at persist time.
func TestAgentRunRejectsNULInExecutionID(t *testing.T) {
	t.Parallel()

	model := newStreamScriptedModel(textDeltas("must not run"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "nul-agent", Model: "fixture-model"},
		Model:      model,
	})
	if err != nil {
		t.Fatal(err)
	}

	stream, err := agent.RunStream(context.Background(), RunInput{
		RunID:       "nul-run",
		ExecutionID: "exec\x00bad",
		Messages:    []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		if !strings.Contains(err.Error(), "execution ID must not contain NUL") {
			t.Fatalf("setup error = %v, want NUL rejection", err)
		}
		return
	}
	defer stream.Cancel()
	result, runErr := stream.Drain()
	if runErr == nil || result.Status != RunStatusFailed {
		t.Fatalf("result = %+v %v, want failed run", result, runErr)
	}
	if !strings.Contains(runErr.Error(), "execution ID must not contain NUL") {
		t.Fatalf("error = %v, want NUL rejection", runErr)
	}
	if calls := model.Calls(); len(calls) != 0 {
		t.Fatalf("model calls = %#v, want none", calls)
	}
}

// TestAgentRunStreamRoutedFailureKeepsObservedMetadataAndOutcome covers the
// routed path of the failed-call accounting contract: the provider call
// succeeds at the provider level (the router completes the attempt when the
// response arrives), a stream-delta processor then breaks the tool calls, and
// the durable attempt record must both keep the observed usage and identity
// and report the call's actual failed outcome instead of the premature
// provider-level success.
func TestAgentRunStreamRoutedFailureKeepsObservedMetadataAndOutcome(t *testing.T) {
	t.Parallel()

	calls, err := NewModelToolCalls(
		ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{}`)},
		ModelToolCall{ID: "call-2", ToolID: "ping", Arguments: json.RawMessage(`{}`)},
	)
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := NewProcessorPipeline(runtimeProcessor{name: "rewriter", delta: func(request ProcessorStreamDeltaRequest) ProcessorStreamDeltaResult {
		if request.Delta.ToolCall != nil && request.Delta.ToolCall.ID == "call-2" {
			rewritten := *request.Delta.ToolCall
			rewritten.ID = "call-1"
			request.Delta.ToolCall = &rewritten
		}
		return ProcessorStreamDeltaResult{Decision: ProcessorDecision{Kind: ProcessorTransform}, Delta: request.Delta}
	}})
	if err != nil {
		t.Fatal(err)
	}
	model := newScriptedModel(scriptedResponse{response: ModelResponse{
		Message:      Message{Role: RoleAssistant, ToolCalls: calls},
		FinishReason: FinishReasonToolCalls,
		Usage:        ModelUsage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12},
		Accounting:   ModelAccounting{ProviderRequestID: "req-routed"},
	}})
	registry := NewProviderRegistry()
	if err := registry.Register(ProviderEntry{ID: "fixture", Model: model, Capabilities: ProviderCapabilities{SupportsTools: true}}); err != nil {
		t.Fatal(err)
	}
	router, err := NewModelRouter(ModelRouterConfig{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "routed-fail", Model: "fixture-model"},
		Router:     router,
		Processors: pipeline,
		Store:      store,
	})
	if err != nil {
		t.Fatal(err)
	}

	stream, streamErr := agent.RunStream(context.Background(), RunInput{RunID: "routed-fail-run", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if streamErr != nil {
		t.Fatal(streamErr)
	}
	result, runErr := stream.Drain()
	if runErr == nil || result.Status != RunStatusFailed {
		t.Fatalf("result = %+v %v, want failed run", result, runErr)
	}
	var modelErr *ModelError
	if !errors.As(runErr, &modelErr) || modelErr.Kind != ModelErrorMalformedResponse {
		t.Fatalf("error = %v, want typed malformed response", runErr)
	}

	attempts, err := store.ModelAttempts().ListModelAttempts(context.Background(), ModelAttemptFilter{RunID: "routed-fail-run"}, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts.Records) != 1 {
		t.Fatalf("persisted attempts = %d, want 1", len(attempts.Records))
	}
	attempt := attempts.Records[0]
	if attempt.Status != ModelAttemptFailed || attempt.ErrorKind != string(ModelErrorMalformedResponse) {
		t.Fatalf("attempt = %s/%s, want failed outcome relabeled from provider success", attempt.Status, attempt.ErrorKind)
	}
	if attempt.Usage != (ModelUsage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}) || attempt.Accounting.ProviderRequestID != "req-routed" || attempt.FinishReason != FinishReasonToolCalls {
		t.Fatalf("attempt metadata = %#v finish %q", attempt, attempt.FinishReason)
	}

	// The durable attempt-finished event must agree with the relabeled
	// record, or operators joining events with attempts see a success event
	// next to a failed attempt for the same call.
	attemptEvents, err := store.RunEvents().ListRunEvents(context.Background(), RunEventFilter{RunID: "routed-fail-run", Type: RunEventModelAttemptFinished}, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(attemptEvents.Records) != 1 {
		t.Fatalf("attempt-finished events = %d, want 1", len(attemptEvents.Records))
	}
	event := attemptEvents.Records[0]
	if event.AttemptStatus != ModelAttemptFailed || event.ErrorKind != string(ModelErrorMalformedResponse) {
		t.Fatalf("attempt-finished event = %s/%s, want the relabeled outcome", event.AttemptStatus, event.ErrorKind)
	}
}

// TestAgentRoutingFailureAfterSuccessNeverRelabelsEarlierAttempts pins the
// relabel boundary: a model call that fails BEFORE any attempt begins
// (routing rejects the request) must leave an earlier call's successful
// attempt record and event untouched.
func TestAgentRoutingFailureAfterSuccessNeverRelabelsEarlierAttempts(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(toolCallResponse(ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{}`)}), textResponse("unused"))
	calls := 0
	registry := NewProviderRegistry()
	if err := registry.Register(ProviderEntry{ID: "fixture", Model: model, Capabilities: ProviderCapabilities{SupportsTools: true}}); err != nil {
		t.Fatal(err)
	}
	router, err := NewModelRouter(ModelRouterConfig{Registry: registry, Policy: RoutingPolicy{
		Predicate: func(ModelRequest) ProviderID {
			calls++
			if calls >= 2 {
				return "missing"
			}
			return "fixture"
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	tools, _ := newAgentTestRegistry(t)
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "boundary-agent", Model: "fixture-model", Tools: []ToolID{"lookup"}},
		Router:     router,
		Store:      store,
		Tools:      tools,
		MaxSteps:   3,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, runErr := agent.Run(context.Background(), RunInput{RunID: "boundary-run", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if runErr == nil || result.Status != RunStatusFailed {
		t.Fatalf("result = %+v %v, want failed run", result, runErr)
	}

	attempts, err := store.ModelAttempts().ListModelAttempts(context.Background(), ModelAttemptFilter{RunID: "boundary-run"}, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts.Records) != 1 {
		t.Fatalf("persisted attempts = %d, want 1 (the routing failure started no attempt)", len(attempts.Records))
	}
	attempt := attempts.Records[0]
	if attempt.Status != ModelAttemptSuccess || attempt.ErrorKind != "" {
		t.Fatalf("earlier attempt relabeled by a later routing failure: %#v", attempt)
	}
	attemptEvents, err := store.RunEvents().ListRunEvents(context.Background(), RunEventFilter{RunID: "boundary-run", Type: RunEventModelAttemptFinished}, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range attemptEvents.Records {
		if event.AttemptStatus != ModelAttemptSuccess {
			t.Fatalf("earlier attempt event relabeled: %#v", event)
		}
	}
}
