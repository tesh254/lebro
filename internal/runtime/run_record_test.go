package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunEventTypeIsTerminal(t *testing.T) {
	t.Parallel()

	terminal := []RunEventType{RunEventSucceeded, RunEventFailed, RunEventCancelled}
	for _, eventType := range terminal {
		if !eventType.IsTerminal() {
			t.Fatalf("%q should be terminal", eventType)
		}
	}

	nonTerminal := []RunEventType{
		RunEventStarted, RunEventModelStarted, RunEventModelFinished,
		RunEventToolRequested, RunEventToolStarted, RunEventToolFinished,
		RunEventStepStarted, RunEventStepFinished,
	}
	for _, eventType := range nonTerminal {
		if eventType.IsTerminal() {
			t.Fatalf("%q should not be terminal", eventType)
		}
	}
}

func TestRunRecorderCollectsEventsInOrder(t *testing.T) {
	t.Parallel()

	recorder := NewRunRecorder()
	events := []RunEvent{
		{Sequence: 1, Type: RunEventStarted, RunID: "run-1"},
		{Sequence: 2, Type: RunEventModelStarted, RunID: "run-1", Step: 1},
		{Sequence: 3, Type: RunEventModelFinished, RunID: "run-1", Step: 1},
		{Sequence: 4, Type: RunEventSucceeded, RunID: "run-1", Status: RunStatusSucceeded},
	}
	for _, event := range events {
		recorder.OnRunEvent(event)
	}

	if recorder.EventCount() != 4 {
		t.Fatalf("count = %d, want 4", recorder.EventCount())
	}

	collected := recorder.Events()
	if len(collected) != 4 {
		t.Fatalf("events length = %d, want 4", len(collected))
	}
	for i, event := range collected {
		if event.Sequence != events[i].Sequence {
			t.Fatalf("event %d sequence = %d, want %d", i, event.Sequence, events[i].Sequence)
		}
		if event.Type != events[i].Type {
			t.Fatalf("event %d type = %q, want %q", i, event.Type, events[i].Type)
		}
	}
}

func TestRunRecorderEventsReturnsCopy(t *testing.T) {
	t.Parallel()

	recorder := NewRunRecorder()
	recorder.OnRunEvent(RunEvent{Sequence: 1, Type: RunEventStarted, RunID: "run-1"})

	events := recorder.Events()
	events[0].Type = RunEventFailed

	original := recorder.Events()
	if original[0].Type != RunEventStarted {
		t.Fatalf("Events() did not return a copy: type = %q", original[0].Type)
	}
}

func TestRunRecorderTerminalEvent(t *testing.T) {
	t.Parallel()

	recorder := NewRunRecorder()
	if _, ok := recorder.TerminalEvent(); ok {
		t.Fatal("empty recorder should not have a terminal event")
	}

	recorder.OnRunEvent(RunEvent{Sequence: 1, Type: RunEventStarted, RunID: "run-1"})
	recorder.OnRunEvent(RunEvent{Sequence: 2, Type: RunEventModelStarted, RunID: "run-1", Step: 1})
	recorder.OnRunEvent(RunEvent{Sequence: 3, Type: RunEventModelFinished, RunID: "run-1", Step: 1})
	if _, ok := recorder.TerminalEvent(); ok {
		t.Fatal("recorder without terminal event should return false")
	}

	recorder.OnRunEvent(RunEvent{Sequence: 4, Type: RunEventSucceeded, RunID: "run-1", Status: RunStatusSucceeded})
	terminal, ok := recorder.TerminalEvent()
	if !ok {
		t.Fatal("recorder with terminal event should return true")
	}
	if terminal.Type != RunEventSucceeded || terminal.Sequence != 4 {
		t.Fatalf("terminal = %#v", terminal)
	}
}

func TestRunRecorderNilSafe(t *testing.T) {
	t.Parallel()

	var recorder *RunRecorder
	recorder.OnRunEvent(RunEvent{})
	if recorder.EventCount() != 0 {
		t.Fatal("nil recorder should be safe")
	}
	if recorder.Events() != nil {
		t.Fatal("nil recorder should return nil events")
	}
	if _, ok := recorder.TerminalEvent(); ok {
		t.Fatal("nil recorder should not have a terminal event")
	}
}

func TestRunRecorderConcurrentSafe(t *testing.T) {
	t.Parallel()

	recorder := NewRunRecorder()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(seq int) {
			defer wg.Done()
			recorder.OnRunEvent(RunEvent{Sequence: seq, Type: RunEventStarted})
		}(i + 1)
	}
	wg.Wait()
	if recorder.EventCount() != 100 {
		t.Fatalf("count = %d, want 100", recorder.EventCount())
	}
}

func TestFixedClockReturnsConstantTime(t *testing.T) {
	t.Parallel()

	fixed := time.Unix(1234567890, 0)
	clock := NewFixedClock(fixed)
	for i := 0; i < 5; i++ {
		if clock.Now() != fixed {
			t.Fatalf("Now() = %v, want %v", clock.Now(), fixed)
		}
	}
}

func TestFixedIDSourceReturnsIDsInOrder(t *testing.T) {
	t.Parallel()

	runIDs := []RunID{"run-a", "run-b", "run-c"}
	stepIDs := []StepID{"step-1", "step-2", "step-3"}
	source := NewFixedIDSource(runIDs, stepIDs)

	for i, want := range runIDs {
		got := source.NewRunID()
		if got != want {
			t.Fatalf("NewRunID() call %d = %q, want %q", i, got, want)
		}
	}

	for i, want := range stepIDs {
		got := source.NewStepID()
		if got != want {
			t.Fatalf("NewStepID() call %d = %q, want %q", i, got, want)
		}
	}
}

func TestFixedIDSourceExhaustedReturnsLast(t *testing.T) {
	t.Parallel()

	source := NewFixedIDSource([]RunID{"only-run"}, []StepID{"only-step"})
	source.NewRunID()
	source.NewStepID()

	if got := source.NewRunID(); got != "only-run" {
		t.Fatalf("exhausted run ID = %q, want only-run", got)
	}
	if got := source.NewStepID(); got != "only-step" {
		t.Fatalf("exhausted step ID = %q, want only-step", got)
	}
}

func TestFixedIDSourceEmptyReturnsFallback(t *testing.T) {
	t.Parallel()

	source := NewFixedIDSource(nil, nil)
	if got := source.NewRunID(); got != RunID("fixed-run") {
		t.Fatalf("empty source run ID = %q, want fixed-run", got)
	}
	if got := source.NewStepID(); got != StepID("fixed-step") {
		t.Fatalf("empty source step ID = %q, want fixed-step", got)
	}
}

func TestSequentialIDSourceFormats(t *testing.T) {
	t.Parallel()

	source := &sequentialIDSource{}
	runID := source.NewRunID()
	if runID != "agent-run-0001" {
		t.Fatalf("first run ID = %q, want agent-run-0001", runID)
	}
	runID = source.NewRunID()
	if runID != "agent-run-0002" {
		t.Fatalf("second run ID = %q, want agent-run-0002", runID)
	}

	stepID := source.NewStepID()
	if stepID != "step-001" {
		t.Fatalf("first step ID = %q, want step-001", stepID)
	}
	stepID = source.NewStepID()
	if stepID != "step-002" {
		t.Fatalf("second step ID = %q, want step-002", stepID)
	}
}

func TestSequentialIDSourceConcurrentSafe(t *testing.T) {
	t.Parallel()

	source := &sequentialIDSource{}
	var wg sync.WaitGroup
	ids := make(chan RunID, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids <- source.NewRunID()
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[RunID]bool, 100)
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate run ID %q", id)
		}
		seen[id] = true
	}
	if len(seen) != 100 {
		t.Fatalf("unique run IDs = %d, want 100", len(seen))
	}
}

// --- Agent integration tests ---

func TestAgentEmitsEventsForSuccessfulTextRun(t *testing.T) {
	t.Parallel()

	recorder := NewRunRecorder()
	clock := NewFixedClock(time.Unix(1000, 0))
	ids := NewFixedIDSource([]RunID{"test-run"}, []StepID{"test-step"})

	model := newScriptedModel(textResponse("hello back"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "echo", Model: "fixture-model"},
		Model:      model,
		Listener:   recorder,
		Clock:      clock,
		IDSource:   ids,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
	if result.ID != "test-run" {
		t.Fatalf("run ID = %q, want test-run", result.ID)
	}

	events := recorder.Events()
	expectedTypes := []RunEventType{
		RunEventStarted,
		RunEventModelStarted,
		RunEventModelFinished,
		RunEventSucceeded,
	}
	if len(events) != len(expectedTypes) {
		t.Fatalf("event count = %d, want %d", len(events), len(expectedTypes))
	}
	for i, want := range expectedTypes {
		if events[i].Type != want {
			t.Fatalf("event %d type = %q, want %q", i, events[i].Type, want)
		}
		if events[i].Sequence != i+1 {
			t.Fatalf("event %d sequence = %d, want %d", i, events[i].Sequence, i+1)
		}
		if events[i].RunID != "test-run" {
			t.Fatalf("event %d run ID = %q, want test-run", i, events[i].RunID)
		}
		if events[i].Timestamp != time.Unix(1000, 0) {
			t.Fatalf("event %d timestamp = %v, want epoch 1000", i, events[i].Timestamp)
		}
	}

	if events[1].Step != 1 || events[1].StepID != "test-step" {
		t.Fatalf("model_started event = %#v", events[1])
	}
	if events[2].Step != 1 || events[2].StepID != "test-step" {
		t.Fatalf("model_finished event = %#v", events[2])
	}
	if events[2].FinishReason != FinishReasonStop {
		t.Fatalf("model_finished finish reason = %q, want stop", events[2].FinishReason)
	}
	if events[3].Status != RunStatusSucceeded {
		t.Fatalf("terminal status = %q, want succeeded", events[3].Status)
	}

	terminal, ok := recorder.TerminalEvent()
	if !ok || terminal.Type != RunEventSucceeded {
		t.Fatalf("terminal event = %#v, want run_succeeded", terminal)
	}
}

func TestAgentEmitsEventsForToolRun(t *testing.T) {
	t.Parallel()

	recorder := NewRunRecorder()
	clock := NewFixedClock(time.Unix(2000, 0))
	ids := NewFixedIDSource([]RunID{"tool-run"}, []StepID{"step-1", "step-2"})

	registry, handler := newAgentTestRegistry(t)
	handler.execute = func(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
		return append(json.RawMessage(nil), input...), nil
	}

	calls, err := NewModelToolCalls(
		ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{"city":"Nairobi"}`)},
	)
	if err != nil {
		t.Fatal(err)
	}
	model := newScriptedModel(
		scriptedResponse{response: ModelResponse{Message: Message{Role: RoleAssistant, ToolCalls: calls}, FinishReason: FinishReasonToolCalls}},
		textResponse("done"),
	)

	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "weather", Model: "fixture-model", Tools: []ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
		Listener:   recorder,
		Clock:      clock,
		IDSource:   ids,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "weather?"}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}

	events := recorder.Events()
	expectedTypes := []RunEventType{
		RunEventStarted,       // 1: run started
		RunEventModelStarted,  // 2: step 1 model start
		RunEventModelFinished, // 3: step 1 model finish (tool_calls)
		RunEventToolRequested, // 4: tool requested
		RunEventToolStarted,   // 5: tool started
		RunEventToolFinished,  // 6: tool finished
		RunEventModelStarted,  // 7: step 2 model start
		RunEventModelFinished, // 8: step 2 model finish (stop)
		RunEventSucceeded,     // 9: terminal
	}
	if len(events) != len(expectedTypes) {
		t.Fatalf("event count = %d, want %d", len(events), len(expectedTypes))
	}
	for i, want := range expectedTypes {
		if events[i].Type != want {
			t.Fatalf("event %d type = %q, want %q", i, events[i].Type, want)
		}
		if events[i].Sequence != i+1 {
			t.Fatalf("event %d sequence = %d, want %d", i, events[i].Sequence, i+1)
		}
	}

	toolReq := events[3]
	if toolReq.ToolCallID != "call-1" || toolReq.ToolID != "lookup" {
		t.Fatalf("tool_requested event = %#v", toolReq)
	}

	toolFin := events[5]
	if toolFin.ToolCallID != "call-1" || toolFin.ToolID != "lookup" || toolFin.ToolState != ToolExecutionSucceeded {
		t.Fatalf("tool_finished event = %#v", toolFin)
	}

	step2Start := events[6]
	if step2Start.Step != 2 || step2Start.StepID != "step-2" {
		t.Fatalf("step 2 model_started event = %#v", step2Start)
	}
}

func TestAgentEmitsFailureEventOnProviderError(t *testing.T) {
	t.Parallel()

	recorder := NewRunRecorder()
	providerErr := &ModelError{Kind: ModelErrorUnavailable, Provider: "fixture", Message: "offline"}
	model := newScriptedModel(scriptedResponse{err: providerErr})
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model"},
		Model:      model,
		Listener:   recorder,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want provider failure")
	}

	events := recorder.Events()
	expectedTypes := []RunEventType{
		RunEventStarted,
		RunEventModelStarted,
		RunEventModelFinished,
		RunEventFailed,
	}
	if len(events) != len(expectedTypes) {
		t.Fatalf("event count = %d, want %d", len(events), len(expectedTypes))
	}
	for i, want := range expectedTypes {
		if events[i].Type != want {
			t.Fatalf("event %d type = %q, want %q", i, events[i].Type, want)
		}
	}

	modelFin := events[2]
	if modelFin.Error == nil {
		t.Fatal("model_finished event should carry error")
	}

	terminal := events[3]
	if terminal.Type != RunEventFailed || terminal.Status != RunStatusFailed {
		t.Fatalf("terminal event = %#v", terminal)
	}
	if terminal.Error == nil {
		t.Fatal("terminal failure event should carry error")
	}
}

func TestAgentEmitsCancelledEventOnContextCancellation(t *testing.T) {
	t.Parallel()

	recorder := NewRunRecorder()
	model := newScriptedModel(scriptedResponse{waitForCancel: true})
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model"},
		Model:      model,
		Listener:   recorder,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = agent.Run(ctx, RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want cancellation")
	}

	events := recorder.Events()
	if len(events) == 0 {
		t.Fatal("no events emitted")
	}
	last := events[len(events)-1]
	if last.Type != RunEventCancelled {
		t.Fatalf("last event type = %q, want run_cancelled", last.Type)
	}
	if last.Status != RunStatusCancelled {
		t.Fatalf("terminal status = %q, want cancelled", last.Status)
	}

	terminal, ok := recorder.TerminalEvent()
	if !ok || terminal.Type != RunEventCancelled {
		t.Fatalf("terminal event = %#v, want run_cancelled", terminal)
	}
}

func TestAgentEmitsFailureEventOnStepLimitExhausted(t *testing.T) {
	t.Parallel()

	recorder := NewRunRecorder()
	registry, handler := newAgentTestRegistry(t)
	handler.execute = func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	}
	fixtures := make([]scriptedResponse, 0, 5)
	for i := 0; i < 5; i++ {
		fixtures = append(fixtures, toolCallResponse(ModelToolCall{ID: fmt.Sprintf("call-%d", i), ToolID: "lookup", Arguments: json.RawMessage(`{}`)}))
	}
	model := newScriptedModel(fixtures...)

	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model", Tools: []ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
		MaxSteps:   2,
		Listener:   recorder,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "loop"}},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want step limit exhausted")
	}

	terminal, ok := recorder.TerminalEvent()
	if !ok || terminal.Type != RunEventFailed {
		t.Fatalf("terminal event = %#v, want run_failed", terminal)
	}
	if terminal.Status != RunStatusFailed {
		t.Fatalf("terminal status = %q, want failed", terminal.Status)
	}
}

func TestAgentEmitsFailureEventOnToolFailure(t *testing.T) {
	t.Parallel()

	recorder := NewRunRecorder()
	registry, handler := newAgentTestRegistry(t)
	handler.execute = func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return nil, errors.New("handler offline")
	}
	model := newScriptedModel(toolCallResponse(ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{}`)}))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model", Tools: []ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
		Listener:   recorder,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "lookup"}},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want tool failure")
	}

	events := recorder.Events()
	toolFin := events[5]
	if toolFin.Type != RunEventToolFinished {
		t.Fatalf("event 5 type = %q, want tool_finished", toolFin.Type)
	}
	if toolFin.ToolState != ToolExecutionHandlerError {
		t.Fatalf("tool state = %q, want handler_error", toolFin.ToolState)
	}
	if toolFin.Error == nil {
		t.Fatal("tool_finished should carry error on failure")
	}

	terminal, ok := recorder.TerminalEvent()
	if !ok || terminal.Type != RunEventFailed {
		t.Fatalf("terminal = %#v, want run_failed", terminal)
	}
}

func TestAgentNoListenerDoesNotAlterBehavior(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(textResponse("hello back"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "echo", Model: "fixture-model", Instructions: "be brief"},
		Model:      model,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
		Metadata: map[string]string{"request_id": "req-1"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
	if len(result.Messages) != 3 {
		t.Fatalf("transcript length = %d, want 3", len(result.Messages))
	}
	if result.Messages[2].Content != "hello back" {
		t.Fatalf("assistant message = %#v", result.Messages[2])
	}
	if result.Metadata["request_id"] != "req-1" {
		t.Fatalf("metadata = %#v", result.Metadata)
	}
	if !strings.HasPrefix(string(result.ID), "agent-run-") {
		t.Fatalf("run ID = %q, want agent-run-*", result.ID)
	}
}

func TestAgentDeterministicEventsWithFixedClockAndIDs(t *testing.T) {
	t.Parallel()

	fixedTime := time.Unix(5000, 0)
	runIDs := []RunID{"det-run-1"}
	stepIDs := []StepID{"det-step-1", "det-step-2"}

	buildAgent := func() (*Agent, *RunRecorder) {
		recorder := NewRunRecorder()
		model := newScriptedModel(textResponse("hello back"))
		agent, err := NewAgent(AgentConfig{
			Definition: AgentDefinition{ID: "echo", Model: "fixture-model"},
			Model:      model,
			Listener:   recorder,
			Clock:      NewFixedClock(fixedTime),
			IDSource:   NewFixedIDSource(runIDs, stepIDs),
		})
		if err != nil {
			t.Fatal(err)
		}
		return agent, recorder
	}

	agent1, recorder1 := buildAgent()
	_, err := agent1.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	agent2, recorder2 := buildAgent()
	_, err = agent2.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	events1 := recorder1.Events()
	events2 := recorder2.Events()
	if len(events1) != len(events2) {
		t.Fatalf("event counts differ: %d vs %d", len(events1), len(events2))
	}
	for i := range events1 {
		if events1[i].Type != events2[i].Type {
			t.Fatalf("event %d type differs: %q vs %q", i, events1[i].Type, events2[i].Type)
		}
		if events1[i].Sequence != events2[i].Sequence {
			t.Fatalf("event %d sequence differs: %d vs %d", i, events1[i].Sequence, events2[i].Sequence)
		}
		if events1[i].RunID != events2[i].RunID {
			t.Fatalf("event %d run ID differs: %q vs %q", i, events1[i].RunID, events2[i].RunID)
		}
		if events1[i].StepID != events2[i].StepID {
			t.Fatalf("event %d step ID differs: %q vs %q", i, events1[i].StepID, events2[i].StepID)
		}
		if events1[i].Timestamp != events2[i].Timestamp {
			t.Fatalf("event %d timestamp differs: %v vs %v", i, events1[i].Timestamp, events2[i].Timestamp)
		}
	}
}

func TestAgentEveryTerminalResultHasTerminalEvent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		model    *scriptedModel
		registry func(t *testing.T) *ToolRegistry
		wantType RunEventType
		wantErr  bool
	}{
		{
			name:     "succeeded",
			model:    newScriptedModel(textResponse("ok")),
			registry: func(t *testing.T) *ToolRegistry { return nil },
			wantType: RunEventSucceeded,
		},
		{
			name:     "provider failure",
			model:    newScriptedModel(scriptedResponse{err: &ModelError{Kind: ModelErrorUnavailable}}),
			registry: func(t *testing.T) *ToolRegistry { return nil },
			wantType: RunEventFailed,
			wantErr:  true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			recorder := NewRunRecorder()
			cfg := AgentConfig{
				Definition: AgentDefinition{ID: "agent", Model: "fixture-model"},
				Model:      test.model,
				Listener:   recorder,
			}
			reg := test.registry(t)
			if reg != nil {
				cfg.Definition.Tools = []ToolID{"lookup"}
				cfg.Tools = reg
			}
			agent, err := NewAgent(cfg)
			if err != nil {
				t.Fatal(err)
			}

			_, _ = agent.Run(context.Background(), RunInput{
				Messages: []Message{{Role: RoleUser, Content: "hello"}},
			})

			terminal, ok := recorder.TerminalEvent()
			if !ok {
				t.Fatal("no terminal event emitted")
			}
			if terminal.Type != test.wantType {
				t.Fatalf("terminal type = %q, want %q", terminal.Type, test.wantType)
			}
			if !terminal.Type.IsTerminal() {
				t.Fatalf("terminal event %q is not terminal", terminal.Type)
			}
			if test.wantErr && terminal.Error == nil {
				t.Fatal("expected error on terminal event")
			}
		})
	}
}

func TestAgentEventSequencesAreMonotonic(t *testing.T) {
	t.Parallel()

	recorder := NewRunRecorder()
	model := newScriptedModel(textResponse("hello"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "echo", Model: "fixture-model"},
		Model:      model,
		Listener:   recorder,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	events := recorder.Events()
	for i, event := range events {
		if event.Sequence != i+1 {
			t.Fatalf("event %d sequence = %d, want %d", i, event.Sequence, i+1)
		}
	}
}

func TestAgentEmitsRunStartedBeforeSetupFailures(t *testing.T) {
	t.Parallel()

	recorder := NewRunRecorder()
	model := newScriptedModel(textResponse("unused"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model"},
		Model:      model,
		Listener:   recorder,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: "invalid", Content: "hello"}},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want validation failure")
	}

	events := recorder.Events()
	if len(events) < 2 {
		t.Fatalf("event count = %d, want at least 2", len(events))
	}
	if events[0].Type != RunEventStarted {
		t.Fatalf("first event type = %q, want run_started", events[0].Type)
	}
	if events[len(events)-1].Type != RunEventFailed {
		t.Fatalf("last event type = %q, want run_failed", events[len(events)-1].Type)
	}
}

func TestAgentNoListenerDoesNotInvokeClock(t *testing.T) {
	t.Parallel()

	panicClock := &panicClock{}
	model := newScriptedModel(textResponse("hello back"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "echo", Model: "fixture-model"},
		Model:      model,
		Clock:      panicClock,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
}

func TestAgentPreExecutionToolEventsHaveEmptyState(t *testing.T) {
	t.Parallel()

	recorder := NewRunRecorder()
	registry, handler := newAgentTestRegistry(t)
	handler.execute = func(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
		return append(json.RawMessage(nil), input...), nil
	}

	calls, err := NewModelToolCalls(
		ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{}`)},
	)
	if err != nil {
		t.Fatal(err)
	}
	model := newScriptedModel(
		scriptedResponse{response: ModelResponse{Message: Message{Role: RoleAssistant, ToolCalls: calls}, FinishReason: FinishReasonToolCalls}},
		textResponse("done"),
	)

	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "weather", Model: "fixture-model", Tools: []ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
		Listener:   recorder,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "weather?"}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	events := recorder.Events()
	for _, event := range events {
		if event.Type == RunEventToolRequested || event.Type == RunEventToolStarted {
			if event.ToolState != "" {
				t.Fatalf("%s event has ToolState %q, want empty", event.Type, event.ToolState)
			}
		}
		if event.Type == RunEventToolFinished {
			if event.ToolState != ToolExecutionSucceeded {
				t.Fatalf("tool_finished ToolState = %q, want succeeded", event.ToolState)
			}
		}
	}
}

func TestNewFixedIDSourceCopiesInputSlices(t *testing.T) {
	t.Parallel()

	runIDs := []RunID{"run-a", "run-b"}
	stepIDs := []StepID{"step-1", "step-2"}
	source := NewFixedIDSource(runIDs, stepIDs)

	runIDs[0] = "tampered"
	stepIDs[0] = "tampered"

	if got := source.NewRunID(); got != "run-a" {
		t.Fatalf("first run ID = %q, want run-a", got)
	}
	if got := source.NewStepID(); got != "step-1" {
		t.Fatalf("first step ID = %q, want step-1", got)
	}
}

type panicClock struct{}

func (panicClock) Now() time.Time {
	panic("clock should not be called when listener is nil")
}

func TestAgentRunIsConcurrencySafeWithListener(t *testing.T) {
	t.Parallel()

	recorder := NewRunRecorder()
	model := newScriptedModel(
		textResponse("a"),
		textResponse("b"),
		textResponse("c"),
		textResponse("d"),
	)
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model"},
		Model:      model,
		Listener:   recorder,
		IDSource:   &sequentialIDSource{},
	})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			_, err := agent.Run(context.Background(), RunInput{
				Messages: []Message{{Role: RoleUser, Content: "x"}},
			})
			done <- err
		}()
	}
	for i := 0; i < 4; i++ {
		if err := <-done; err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	}

	totalEvents := recorder.EventCount()
	if totalEvents != 4*4 {
		t.Fatalf("total events = %d, want %d", totalEvents, 4*4)
	}
}
