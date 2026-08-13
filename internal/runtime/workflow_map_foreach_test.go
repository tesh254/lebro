package runtime

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"
)

func TestMapTransformsDeclaredFields(t *testing.T) {
	t.Parallel()
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "map-fields"},
		Steps: []Step{{Definition: StepDefinition{ID: "map", Map: &Map{Fields: map[string]string{
			"id": "customer.id", "name": "customer.name",
		}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := wf.Run(context.Background(), WorkflowRunInput{Input: json.RawMessage(`{"customer":{"id":"c-1","name":"Ada","unused":true}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Output) != `{"id":"c-1","name":"Ada"}` {
		t.Fatalf("output = %s", result.Output)
	}
}

func TestForEachBoundsConcurrencyAndPreservesOrder(t *testing.T) {
	t.Parallel()
	var active, maximum int32
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "foreach-order"},
		Steps: []Step{{Definition: StepDefinition{ID: "each", ForEach: &ForEach{MaxParallel: 2, Steps: []Step{{
			Definition: StepDefinition{ID: "work"},
			Handler: StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
				current := atomic.AddInt32(&active, 1)
				for {
					old := atomic.LoadInt32(&maximum)
					if current <= old || atomic.CompareAndSwapInt32(&maximum, old, current) {
						break
					}
				}
				defer atomic.AddInt32(&active, -1)
				var value int
				if err := json.Unmarshal(input, &value); err != nil {
					return nil, err
				}
				time.Sleep(time.Duration(4-value) * 10 * time.Millisecond)
				return json.RawMessage(`{"value":` + string(input) + `}`), nil
			}),
		}}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := wf.Run(context.Background(), WorkflowRunInput{Input: json.RawMessage(`[1,2,3]`)})
	if err != nil {
		t.Fatal(err)
	}
	if maximum != 2 {
		t.Fatalf("maximum active = %d, want 2", maximum)
	}
	if string(result.Output) != `[{"value":1},{"value":2},{"value":3}]` {
		t.Fatalf("output = %s", result.Output)
	}
	if len(result.ForEach) != 1 || len(result.ForEach[0].Items) != 3 {
		t.Fatalf("foreach result = %#v", result.ForEach)
	}
}

func TestForEachRejectsNonArrayInput(t *testing.T) {
	t.Parallel()
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{Definition: WorkflowDefinition{ID: "foreach-array"}, Steps: []Step{{Definition: StepDefinition{ID: "each", ForEach: &ForEach{Steps: []Step{{Definition: StepDefinition{ID: "work"}, Handler: StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) { return input, nil })}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = wf.Run(context.Background(), WorkflowRunInput{Input: json.RawMessage(`{"not":"array"}`)})
	if err == nil {
		t.Fatal("Run() error = nil, want non-array rejection")
	}
	_, err = wf.Run(context.Background(), WorkflowRunInput{Input: json.RawMessage(`null`)})
	if err == nil {
		t.Fatal("Run() error = nil, want null rejection")
	}
}

func TestForEachPersistsOrderedItemState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "foreach-persist"}, Store: store, IDSource: NewFixedIDSource([]RunID{"foreach-run"}, nil),
		Steps: []Step{{Definition: StepDefinition{ID: "each", ForEach: &ForEach{Steps: []Step{{Definition: StepDefinition{ID: "work"}, Handler: StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) { return input, nil })}}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`["a","b"]`)}); err != nil {
		t.Fatal(err)
	}
	snapshots, err := store.WorkflowSnapshots().ListWorkflowSnapshots(ctx, "foreach-run", PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots.Records) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(snapshots.Records))
	}
	var envelope workflowSnapshotEnvelope
	if err := json.Unmarshal(snapshots.Records[0].State, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.ForEach) != 1 || len(envelope.ForEach[0].Items) != 2 {
		t.Fatalf("foreach snapshot = %#v", envelope.ForEach)
	}
	if envelope.ForEach[0].Items[0].Index != 0 || string(envelope.ForEach[0].Items[1].Output) != `"b"` {
		t.Fatalf("foreach items = %#v", envelope.ForEach[0].Items)
	}
}
