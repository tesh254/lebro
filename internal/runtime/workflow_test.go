package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewLinearWorkflowValidatesConfiguration(t *testing.T) {
	t.Parallel()

	echoHandler := StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
		return append(json.RawMessage(nil), input...), nil
	})

	tests := []struct {
		name string
		cfg  LinearWorkflowConfig
		want string
	}{
		{
			name: "missing definition ID",
			cfg: LinearWorkflowConfig{
				Steps: []Step{{Definition: StepDefinition{ID: "s1"}, Handler: echoHandler}},
			},
			want: "workflow definition ID is required",
		},
		{
			name: "no steps",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
			},
			want: "workflow must have at least one step",
		},
		{
			name: "step missing ID",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
				Steps:      []Step{{Handler: echoHandler}},
			},
			want: "workflow step ID is required",
		},
		{
			name: "step missing handler",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
				Steps:      []Step{{Definition: StepDefinition{ID: "s1"}}},
			},
			want: "handler is required",
		},
		{
			name: "duplicate step ID",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
				Steps: []Step{
					{Definition: StepDefinition{ID: "s1"}, Handler: echoHandler},
					{Definition: StepDefinition{ID: "s1"}, Handler: echoHandler},
				},
			},
			want: "step ID \"s1\" is already registered",
		},
		{
			name: "schema without compiler",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
				Steps: []Step{{
					Definition: StepDefinition{ID: "s1", OutputSchema: json.RawMessage(`{"type":"object"}`)},
					Handler:    echoHandler,
				}},
			},
			want: "schema compiler is required",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewLinearWorkflow(test.cfg)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewLinearWorkflow() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestNewLinearWorkflowRejectsInvalidSchemaJSON(t *testing.T) {
	t.Parallel()

	_, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition:     WorkflowDefinition{ID: "wf"},
		SchemaCompiler: stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) { return stubCompiledSchema{}, nil }},
		Steps: []Step{{
			Definition: StepDefinition{ID: "s1", OutputSchema: json.RawMessage(`{broken`)},
			Handler:    StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) { return input, nil }),
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "output schema must be valid JSON") {
		t.Fatalf("error = %v, want valid JSON message", err)
	}
}

func TestNewLinearWorkflowCompilesSchemasOnce(t *testing.T) {
	t.Parallel()

	compiles := 0
	compiler := stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		compiles++
		return stubCompiledSchema{}, nil
	}}
	handler := StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) { return input, nil })
	_, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition:     WorkflowDefinition{ID: "wf"},
		SchemaCompiler: compiler,
		Steps: []Step{
			{Definition: StepDefinition{ID: "s1", InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`)}, Handler: handler},
			{Definition: StepDefinition{ID: "s2", InputSchema: json.RawMessage(`{"type":"object"}`)}, Handler: handler},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if compiles != 3 {
		t.Fatalf("compiles = %d, want 3", compiles)
	}
}

func TestLinearWorkflowTwoStepsPassValidatedOutput(t *testing.T) {
	t.Parallel()

	compiler := stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		return stubCompiledSchema{}, nil
	}}
	step1Output := json.RawMessage(`{"value":21}`)
	step2Received := make(chan json.RawMessage, 1)
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition:     WorkflowDefinition{ID: "double"},
		SchemaCompiler: compiler,
		Steps: []Step{
			{
				Definition: StepDefinition{ID: "produce", OutputSchema: json.RawMessage(`{"type":"object"}`)},
				Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
					return step1Output, nil
				}),
			},
			{
				Definition: StepDefinition{ID: "consume", InputSchema: json.RawMessage(`{"type":"object"}`)},
				Handler: StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
					step2Received <- append(json.RawMessage(nil), input...)
					return json.RawMessage(`{"result":"ok"}`), nil
				}),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := wf.Run(context.Background(), WorkflowRunInput{
		Input: json.RawMessage(`{"start":true}`),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}

	received := <-step2Received
	if string(received) != string(step1Output) {
		t.Fatalf("step 2 received = %s, want %s", received, step1Output)
	}
	if string(result.Output) != `{"result":"ok"}` {
		t.Fatalf("output = %s, want {\"result\":\"ok\"}", result.Output)
	}
}

func TestLinearWorkflowFailedStepStopsAndIdentifiesStep(t *testing.T) {
	t.Parallel()

	handlerErr := errors.New("downstream unavailable")
	step1Called := false
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fail-wf"},
		Steps: []Step{
			{
				Definition: StepDefinition{ID: "first"},
				Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
					step1Called = true
					return nil, handlerErr
				}),
			},
			{
				Definition: StepDefinition{ID: "second"},
				Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
					t.Fatal("second step should not run after first step fails")
					return nil, nil
				}),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := wf.Run(context.Background(), WorkflowRunInput{
		Input: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("Run() error = nil, want step failure")
	}
	if result.Status != RunStatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}

	var wfErr *WorkflowError
	if !errors.As(err, &wfErr) {
		t.Fatalf("error = %T, want *WorkflowError", err)
	}
	if wfErr.Kind != WorkflowErrorStepFailed {
		t.Fatalf("kind = %q, want step_failed", wfErr.Kind)
	}
	if wfErr.Step != 1 {
		t.Fatalf("step = %d, want 1", wfErr.Step)
	}
	if wfErr.StepID != "first" {
		t.Fatalf("step ID = %q, want first", wfErr.StepID)
	}
	if !errors.Is(wfErr.Err, handlerErr) {
		t.Fatalf("wrapped error = %v, want %v", wfErr.Err, handlerErr)
	}
	if !errors.Is(err, ErrWorkflowStepFailure) {
		t.Fatalf("errors.Is(err, ErrWorkflowStepFailure) = false")
	}
	if !step1Called {
		t.Fatal("first step handler was not called")
	}
}

func TestLinearWorkflowEventsOrderedAndCorrelated(t *testing.T) {
	t.Parallel()

	recorder := NewRunRecorder()
	clock := NewFixedClock(time.Unix(7000, 0))
	ids := NewFixedIDSource([]RunID{"wf-run-1"}, nil)

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "events-wf"},
		Listener:   recorder,
		Clock:      clock,
		IDSource:   ids,
		Steps: []Step{
			{Definition: StepDefinition{ID: "a"}, Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`1`), nil })},
			{Definition: StepDefinition{ID: "b"}, Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`2`), nil })},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := wf.Run(context.Background(), WorkflowRunInput{Input: json.RawMessage(`0`)})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ID != "wf-run-1" {
		t.Fatalf("run ID = %q, want wf-run-1", result.ID)
	}

	events := recorder.Events()
	expectedTypes := []RunEventType{
		RunEventStarted,
		RunEventStepStarted, RunEventStepFinished,
		RunEventStepStarted, RunEventStepFinished,
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
		if events[i].RunID != "wf-run-1" {
			t.Fatalf("event %d run ID = %q, want wf-run-1", i, events[i].RunID)
		}
		if events[i].Timestamp != time.Unix(7000, 0) {
			t.Fatalf("event %d timestamp = %v, want epoch 7000", i, events[i].Timestamp)
		}
	}

	if events[1].Step != 1 || events[1].StepID != "a" {
		t.Fatalf("step 1 started event = %#v", events[1])
	}
	if events[3].Step != 2 || events[3].StepID != "b" {
		t.Fatalf("step 2 started event = %#v", events[3])
	}
	if events[2].Duration < 0 {
		t.Fatalf("step 1 finished duration = %v, want >= 0", events[2].Duration)
	}

	terminal, ok := recorder.TerminalEvent()
	if !ok || terminal.Type != RunEventSucceeded {
		t.Fatalf("terminal = %#v, want run_succeeded", terminal)
	}
}

func TestLinearWorkflowExecutesOrdinaryFunctions(t *testing.T) {
	t.Parallel()

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "plain-fns"},
		Steps: []Step{
			{Definition: StepDefinition{ID: "double"}, Handler: StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
				var n int
				if err := json.Unmarshal(input, &n); err != nil {
					return nil, err
				}
				return json.Marshal(n * 2)
			})},
			{Definition: StepDefinition{ID: "add-one"}, Handler: StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
				var n int
				if err := json.Unmarshal(input, &n); err != nil {
					return nil, err
				}
				return json.Marshal(n + 1)
			})},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := wf.Run(context.Background(), WorkflowRunInput{Input: json.RawMessage(`5`)})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}

	var final int
	if err := result.DecodeOutput(&final); err != nil {
		t.Fatal(err)
	}
	if final != 11 {
		t.Fatalf("output = %d, want 11", final)
	}
}

func TestLinearWorkflowInputSchemaValidationFails(t *testing.T) {
	t.Parallel()

	compiler := stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		return stubCompiledSchema{validationErr: &ValidationError{Issues: []ValidationIssue{{Path: "", Keyword: "type", Message: "must be number"}}}}, nil
	}}
	step2Called := false
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition:     WorkflowDefinition{ID: "handoff-wf"},
		SchemaCompiler: compiler,
		Steps: []Step{
			{Definition: StepDefinition{ID: "s1"}, Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`"not-a-number"`), nil
			})},
			{
				Definition: StepDefinition{ID: "s2", InputSchema: json.RawMessage(`{"type":"number"}`)},
				Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
					step2Called = true
					return nil, nil
				}),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = wf.Run(context.Background(), WorkflowRunInput{Input: json.RawMessage(`0`)})
	if err == nil {
		t.Fatal("Run() error = nil, want invalid step input")
	}
	if !errors.Is(err, ErrWorkflowInvalidStepInput) {
		t.Fatalf("error = %v, want ErrWorkflowInvalidStepInput", err)
	}
	var wfErr *WorkflowError
	if errors.As(err, &wfErr); wfErr.StepID != "s2" || wfErr.Step != 2 {
		t.Fatalf("workflow error = %#v, want step 2 s2", wfErr)
	}
	if step2Called {
		t.Fatal("step 2 handler should not run after handoff validation fails")
	}
}

func TestLinearWorkflowOutputSchemaValidationFails(t *testing.T) {
	t.Parallel()

	compiler := stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		return stubCompiledSchema{validationErr: &ValidationError{Issues: []ValidationIssue{{Path: "", Keyword: "type", Message: "must be object"}}}}, nil
	}}
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition:     WorkflowDefinition{ID: "output-wf"},
		SchemaCompiler: compiler,
		Steps: []Step{
			{
				Definition: StepDefinition{ID: "s1", OutputSchema: json.RawMessage(`{"type":"object"}`)},
				Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
					return json.RawMessage(`"string-not-object"`), nil
				}),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = wf.Run(context.Background(), WorkflowRunInput{Input: json.RawMessage(`{}`)})
	if err == nil {
		t.Fatal("Run() error = nil, want invalid step output")
	}
	if !errors.Is(err, ErrWorkflowInvalidStepOutput) {
		t.Fatalf("error = %v, want ErrWorkflowInvalidStepOutput", err)
	}
	var wfErr *WorkflowError
	if errors.As(err, &wfErr); wfErr.StepID != "s1" || wfErr.Step != 1 {
		t.Fatalf("workflow error = %#v, want step 1 s1", wfErr)
	}
}

func TestLinearWorkflowStepPanicRecovered(t *testing.T) {
	t.Parallel()

	recorder := NewRunRecorder()
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "panic-wf"},
		Listener:   recorder,
		Steps: []Step{
			{Definition: StepDefinition{ID: "boom"}, Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) { panic("kaboom") })},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = wf.Run(context.Background(), WorkflowRunInput{Input: json.RawMessage(`{}`)})
	if err == nil {
		t.Fatal("Run() error = nil, want step panic")
	}
	if !errors.Is(err, ErrWorkflowStepPanicked) {
		t.Fatalf("error = %v, want ErrWorkflowStepPanicked", err)
	}
	if !strings.Contains(err.Error(), "kaboom") {
		t.Fatalf("error = %v, want kaboom", err)
	}

	terminal, ok := recorder.TerminalEvent()
	if !ok || terminal.Type != RunEventFailed {
		t.Fatalf("terminal = %#v, want run_failed", terminal)
	}
}

func TestLinearWorkflowContextCancellation(t *testing.T) {
	t.Parallel()

	recorder := NewRunRecorder()
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "cancel-wf"},
		Listener:   recorder,
		Steps: []Step{
			{Definition: StepDefinition{ID: "block"}, Handler: StepHandlerFunc(func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			})},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`{}`)})
	if err == nil {
		t.Fatal("Run() error = nil, want cancellation")
	}
	if !errors.Is(err, ErrWorkflowCancelled) {
		t.Fatalf("error = %v, want ErrWorkflowCancelled", err)
	}

	terminal, ok := recorder.TerminalEvent()
	if !ok || terminal.Type != RunEventCancelled {
		t.Fatalf("terminal = %#v, want run_cancelled", terminal)
	}
}

func TestLinearWorkflowNoListenerDoesNotAlterBehavior(t *testing.T) {
	t.Parallel()

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "no-listener"},
		Steps: []Step{
			{Definition: StepDefinition{ID: "s1"}, Handler: StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) { return input, nil })},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := wf.Run(context.Background(), WorkflowRunInput{
		Input:    json.RawMessage(`{"ok":true}`),
		Metadata: map[string]string{"request_id": "req-1"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
	if string(result.Output) != `{"ok":true}` {
		t.Fatalf("output = %s, want {\"ok\":true}", result.Output)
	}
	if result.Metadata["request_id"] != "req-1" {
		t.Fatalf("metadata = %#v", result.Metadata)
	}
	if !strings.HasPrefix(string(result.ID), "agent-run-") {
		t.Fatalf("run ID = %q, want agent-run-*", result.ID)
	}
}

func TestLinearWorkflowNoListenerDoesNotInvokeClock(t *testing.T) {
	t.Parallel()

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "no-clock"},
		Clock:      panicClock{},
		Steps: []Step{
			{Definition: StepDefinition{ID: "s1"}, Handler: StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) { return input, nil })},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := wf.Run(context.Background(), WorkflowRunInput{Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
}

func TestLinearWorkflowDeterministicEventsWithFixedClockAndIDs(t *testing.T) {
	t.Parallel()

	fixedTime := time.Unix(8000, 0)
	runIDs := []RunID{"det-wf-1"}

	build := func() (*LinearWorkflow, *RunRecorder) {
		recorder := NewRunRecorder()
		wf, err := NewLinearWorkflow(LinearWorkflowConfig{
			Definition: WorkflowDefinition{ID: "det-wf"},
			Listener:   recorder,
			Clock:      NewFixedClock(fixedTime),
			IDSource:   NewFixedIDSource(runIDs, nil),
			Steps: []Step{
				{Definition: StepDefinition{ID: "a"}, Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`1`), nil })},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return wf, recorder
	}

	wf1, rec1 := build()
	if _, err := wf1.Run(context.Background(), WorkflowRunInput{Input: json.RawMessage(`0`)}); err != nil {
		t.Fatal(err)
	}

	wf2, rec2 := build()
	if _, err := wf2.Run(context.Background(), WorkflowRunInput{Input: json.RawMessage(`0`)}); err != nil {
		t.Fatal(err)
	}

	events1, events2 := rec1.Events(), rec2.Events()
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
		if events1[i].Timestamp != events2[i].Timestamp {
			t.Fatalf("event %d timestamp differs: %v vs %v", i, events1[i].Timestamp, events2[i].Timestamp)
		}
	}
}

func TestLinearWorkflowEveryTerminalResultHasTerminalEvent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		handler  StepHandler
		wantType RunEventType
	}{
		{
			name:     "succeeded",
			handler:  StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) { return input, nil }),
			wantType: RunEventSucceeded,
		},
		{
			name:     "step failed",
			handler:  StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) { return nil, errors.New("boom") }),
			wantType: RunEventFailed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			recorder := NewRunRecorder()
			wf, err := NewLinearWorkflow(LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "terminal-wf"},
				Listener:   recorder,
				Steps:      []Step{{Definition: StepDefinition{ID: "s1"}, Handler: test.handler}},
			})
			if err != nil {
				t.Fatal(err)
			}

			_, _ = wf.Run(context.Background(), WorkflowRunInput{Input: json.RawMessage(`{}`)})

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
		})
	}
}

func TestLinearWorkflowEventSequencesAreMonotonic(t *testing.T) {
	t.Parallel()

	recorder := NewRunRecorder()
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "mono-wf"},
		Listener:   recorder,
		Steps: []Step{
			{Definition: StepDefinition{ID: "a"}, Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`1`), nil })},
			{Definition: StepDefinition{ID: "b"}, Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`2`), nil })},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := wf.Run(context.Background(), WorkflowRunInput{Input: json.RawMessage(`0`)}); err != nil {
		t.Fatal(err)
	}

	events := recorder.Events()
	for i, event := range events {
		if event.Sequence != i+1 {
			t.Fatalf("event %d sequence = %d, want %d", i, event.Sequence, i+1)
		}
	}
}

func TestLinearWorkflowRunIsConcurrencySafeWithListener(t *testing.T) {
	t.Parallel()

	recorder := NewRunRecorder()
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "concurrent-wf"},
		Listener:   recorder,
		IDSource:   &sequentialIDSource{},
		Steps: []Step{
			{Definition: StepDefinition{ID: "echo"}, Handler: StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) { return input, nil })},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	const goroutines = 8
	done := make(chan error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := wf.Run(context.Background(), WorkflowRunInput{Input: json.RawMessage(`{}`)})
			done <- err
		}()
	}
	wg.Wait()
	close(done)
	for err := range done {
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	}

	// Each run emits: run_started, step_started, step_finished, run_succeeded = 4 events.
	totalEvents := recorder.EventCount()
	if totalEvents != goroutines*4 {
		t.Fatalf("total events = %d, want %d", totalEvents, goroutines*4)
	}
}

func TestLinearWorkflowMetadataPropagated(t *testing.T) {
	t.Parallel()

	var captured map[string]string
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "meta-wf"},
		Steps: []Step{
			{Definition: StepDefinition{ID: "capture"}, Handler: StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
				captured = map[string]string{"trace": "set"}
				return input, nil
			})},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := wf.Run(context.Background(), WorkflowRunInput{
		Input:    json.RawMessage(`{}`),
		Metadata: map[string]string{"request_id": "req-9"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Metadata["request_id"] != "req-9" {
		t.Fatalf("metadata = %#v", result.Metadata)
	}
	_ = captured
}

func TestLinearWorkflowDecodeOutputEmpty(t *testing.T) {
	t.Parallel()

	result := WorkflowRunResult{Status: RunStatusFailed}
	var v map[string]any
	if err := result.DecodeOutput(&v); err == nil {
		t.Fatal("DecodeOutput() error = nil, want error on empty output")
	}
}

func TestLinearWorkflowNilWorkflowReturnsError(t *testing.T) {
	t.Parallel()

	var wf *LinearWorkflow
	_, err := wf.Run(context.Background(), WorkflowRunInput{})
	if err == nil {
		t.Fatal("Run() error = nil, want error on nil workflow")
	}
}

func TestLinearWorkflowDefinitionReturnsConfigured(t *testing.T) {
	t.Parallel()

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "def-wf", Name: "Definition", Description: "desc"},
		Steps: []Step{
			{Definition: StepDefinition{ID: "s1"}, Handler: StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) { return input, nil })},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	def := wf.Definition()
	if def.ID != WorkflowID("def-wf") || def.Name != "Definition" || def.Description != "desc" {
		t.Fatalf("Definition() = %#v", def)
	}
}
