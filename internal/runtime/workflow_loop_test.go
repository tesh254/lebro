package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

func loopIncrement() Step {
	return Step{Definition: StepDefinition{ID: "increment"}, Handler: StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
		var v struct {
			Value int `json:"value"`
		}
		if err := json.Unmarshal(input, &v); err != nil {
			return nil, err
		}
		v.Value++
		return json.Marshal(v)
	})}
}

func loopValue(input json.RawMessage) int {
	var v struct {
		Value int `json:"value"`
	}
	_ = json.Unmarshal(input, &v)
	return v.Value
}

func doWhileDefinition(condition LoopPredicate, max int) StepDefinition {
	return StepDefinition{ID: "loop", DoWhile: &DoWhile{Steps: []Step{loopIncrement()}, MaxIterations: max, Condition: condition}}
}

func TestDoWhileExecutesPostConditionLoop(t *testing.T) {
	t.Parallel()
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{Definition: WorkflowDefinition{ID: "do-while"}, Steps: []Step{{Definition: doWhileDefinition(func(_ context.Context, out json.RawMessage, _ int) (bool, error) {
		return loopValue(out) < 3, nil
	}, 5)}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := wf.Run(context.Background(), WorkflowRunInput{Input: json.RawMessage(`{"value":0}`)})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Output) != `{"value":3}` {
		t.Fatalf("output = %s", result.Output)
	}
}

func TestDoUntilStopsWhenConditionMatches(t *testing.T) {
	t.Parallel()
	until := &DoUntil{Steps: []Step{loopIncrement()}, MaxIterations: 5, Condition: func(_ context.Context, out json.RawMessage, _ int) (bool, error) {
		return loopValue(out) >= 2, nil
	}}
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{Definition: WorkflowDefinition{ID: "do-until"}, Steps: []Step{{Definition: StepDefinition{ID: "loop", DoUntil: until}}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := wf.Run(context.Background(), WorkflowRunInput{Input: json.RawMessage(`{"value":0}`)})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Output) != `{"value":2}` {
		t.Fatalf("output = %s", result.Output)
	}
}

func TestLoopRejectsUnboundedDefinitions(t *testing.T) {
	t.Parallel()
	_, err := NewLinearWorkflow(LinearWorkflowConfig{Definition: WorkflowDefinition{ID: "bad-loop"}, Steps: []Step{{Definition: doWhileDefinition(func(context.Context, json.RawMessage, int) (bool, error) { return false, nil }, 0)}}})
	if err == nil || !strings.Contains(err.Error(), "MaxIterations must be between 1") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoopRejectsSuspendableChild(t *testing.T) {
	t.Parallel()
	child := loopIncrement()
	child.Definition.SuspendSchema = json.RawMessage(`{"type":"object"}`)
	loop := &DoWhile{MaxIterations: 1, Condition: func(context.Context, json.RawMessage, int) (bool, error) { return false, nil }, Steps: []Step{child}}
	_, err := NewLinearWorkflow(LinearWorkflowConfig{Definition: WorkflowDefinition{ID: "suspend-loop"}, Steps: []Step{{Definition: StepDefinition{ID: "loop", DoWhile: loop}}}})
	if err == nil || !strings.Contains(err.Error(), "cannot contain suspendable child") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoopReportsMaxIterations(t *testing.T) {
	t.Parallel()
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{Definition: WorkflowDefinition{ID: "bounded-loop"}, Steps: []Step{{Definition: doWhileDefinition(func(context.Context, json.RawMessage, int) (bool, error) { return true, nil }, 2)}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = wf.Run(context.Background(), WorkflowRunInput{Input: json.RawMessage(`{"value":0}`)})
	if !errors.Is(err, ErrWorkflowMaxIterations) {
		t.Fatalf("error = %v", err)
	}
}

func TestLoopPersistsEveryIterationAndEmitsEvents(t *testing.T) {
	t.Parallel()
	ctx, store, recorder := context.Background(), NewMemoryStore(), NewRunRecorder()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	until := &DoUntil{Steps: []Step{loopIncrement()}, MaxIterations: 3, Condition: func(_ context.Context, out json.RawMessage, _ int) (bool, error) {
		return loopValue(out) == 2, nil
	}}
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{Definition: WorkflowDefinition{ID: "durable-loop"}, Store: store, Listener: recorder, IDSource: NewFixedIDSource([]RunID{"loop-run"}, nil), Steps: []Step{{Definition: StepDefinition{ID: "loop", DoUntil: until}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`{"value":0}`)}); err != nil {
		t.Fatal(err)
	}
	snapshots, err := store.WorkflowSnapshots().ListWorkflowSnapshots(ctx, "loop-run", PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots.Records) != 3 {
		t.Fatalf("snapshots = %d, want 3", len(snapshots.Records))
	}
	var started, finished int
	for _, e := range recorder.Events() {
		if e.Type == RunEventLoopIterationStarted {
			started++
		}
		if e.Type == RunEventLoopIterationFinished {
			finished++
		}
	}
	if started != 2 || finished != 2 {
		t.Fatalf("events started=%d finished=%d", started, finished)
	}
}

func TestLoopChecksCancellationAfterPredicate(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{Definition: WorkflowDefinition{ID: "cancel-loop"}, Steps: []Step{{Definition: doWhileDefinition(func(context.Context, json.RawMessage, int) (bool, error) { calls.Add(1); cancel(); return false, nil }, 3)}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`{"value":0}`)})
	if !errors.Is(err, ErrWorkflowCancelled) || calls.Load() != 1 {
		t.Fatalf("error = %v calls = %d", err, calls.Load())
	}
}
