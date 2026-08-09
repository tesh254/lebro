package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// equalsCompiledSchema validates that the input JSON deep-equals expected.
// It is a minimal CompiledSchema used by suspend/resume tests to exercise
// the SuspendSchema and resume-input validation paths without pulling in the
// jsonschema package from the runtime layer.
type equalsCompiledSchema struct {
	expected json.RawMessage
}

func (s equalsCompiledSchema) Validate(value json.RawMessage) *ValidationError {
	var got, want any
	if err := json.Unmarshal(value, &got); err != nil {
		return &ValidationError{Target: ValidationTargetSuspendContract, Issues: []ValidationIssue{{Path: "", Keyword: "type", Message: "invalid JSON: " + err.Error()}}}
	}
	if err := json.Unmarshal(s.expected, &want); err != nil {
		return &ValidationError{Target: ValidationTargetSuspendContract, Issues: []ValidationIssue{{Path: "", Keyword: "type", Message: "schema invalid: " + err.Error()}}}
	}
	if !deepEqualJSON(got, want) {
		return &ValidationError{Target: ValidationTargetSuspendContract, Issues: []ValidationIssue{{Path: "", Keyword: "const", Message: "resume input does not match suspend contract"}}}
	}
	return nil
}

func deepEqualJSON(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			if !deepEqualJSON(v, bv[k]) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !deepEqualJSON(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}

// contractCompiler hands each compiled schema an equalsCompiledSchema anchored
// on the raw schema document's "const" field so a single stub compiler can
// validate both the suspend contract and the resume input against the same
// rule. Schemas without a const field accept everything.
func contractCompiler() SchemaCompiler {
	return stubSchemaCompiler{compile: func(schema json.RawMessage) (CompiledSchema, error) {
		var doc struct {
			Const json.RawMessage `json:"const"`
		}
		if err := json.Unmarshal(schema, &doc); err != nil {
			return nil, err
		}
		if len(doc.Const) == 0 {
			return stubCompiledSchema{}, nil
		}
		return equalsCompiledSchema{expected: doc.Const}, nil
	}}
}

func TestSuspendReturnsSuspendedResultWithoutRunningRemainingSteps(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	var laterCalls int32
	suspendHandler := StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
		return nil, &SuspendError{Signal: SuspendSignal{
			StepID:   "await-approval",
			Contract: json.RawMessage(`{"approved":true}`),
			Payload:  json.RawMessage(`{"reason":"needs human"}`),
		}}
	})
	laterHandler := StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
		atomic.AddInt32(&laterCalls, 1)
		return json.RawMessage(`"done"`), nil
	})

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition:     WorkflowDefinition{ID: "suspend-wf", Version: "v1"},
		SchemaCompiler: contractCompiler(),
		Steps: []Step{
			{Definition: StepDefinition{ID: "await-approval", SuspendSchema: json.RawMessage(`{"const":{"approved":true}}`)}, Handler: suspendHandler},
			{Definition: StepDefinition{ID: "after-approval"}, Handler: laterHandler},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`"start"`)})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Status != RunStatusSuspended {
		t.Fatalf("Status = %q, want %q", result.Status, RunStatusSuspended)
	}
	if result.Suspend == nil {
		t.Fatal("Suspend result is nil")
	}
	if result.Suspend.Step != 1 || result.Suspend.StepID != "await-approval" {
		t.Fatalf("Suspend = %+v, want step 1 await-approval", result.Suspend)
	}
	if string(result.Suspend.Contract) != `{"approved":true}` {
		t.Fatalf("Contract = %s, want {\"approved\":true}", result.Suspend.Contract)
	}
	if string(result.Suspend.Payload) != `{"reason":"needs human"}` {
		t.Fatalf("Payload = %s, want {\"reason\":\"needs human\"}", result.Suspend.Payload)
	}
	if got := atomic.LoadInt32(&laterCalls); got != 0 {
		t.Fatalf("after-approval ran %d times, want 0 (suspend must not run later steps)", got)
	}
}

func TestSuspendWithoutSuspendSchemaIsRejected(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	suspendHandler := StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
		return nil, &SuspendError{Signal: SuspendSignal{Contract: json.RawMessage(`{}`)}}
	})

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition:     WorkflowDefinition{ID: "suspend-no-schema", Version: "v1"},
		SchemaCompiler: contractCompiler(),
		Steps: []Step{
			{Definition: StepDefinition{ID: "suspend-step"}, Handler: suspendHandler},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`"start"`)})
	if err == nil {
		t.Fatal("Run returned nil error, want failure for suspend without SuspendSchema")
	}
	var wfErr *WorkflowError
	if !errors.As(err, &wfErr) || wfErr.Kind != WorkflowErrorInvalidStepOutput {
		t.Fatalf("err = %v, want WorkflowErrorInvalidStepOutput", err)
	}
}

func TestSuspendContractFailingSuspendSchemaIsRejected(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	suspendHandler := StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
		return nil, &SuspendError{Signal: SuspendSignal{Contract: json.RawMessage(`{"approved":false}`)}}
	})

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition:     WorkflowDefinition{ID: "suspend-bad-contract", Version: "v1"},
		SchemaCompiler: contractCompiler(),
		Steps: []Step{
			{Definition: StepDefinition{ID: "suspend-step", SuspendSchema: json.RawMessage(`{"const":{"approved":true}}`)}, Handler: suspendHandler},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`"start"`)})
	if err == nil {
		t.Fatal("Run returned nil error, want failure for contract failing SuspendSchema")
	}
	var wfErr *WorkflowError
	if !errors.As(err, &wfErr) || wfErr.Kind != WorkflowErrorInvalidStepOutput {
		t.Fatalf("err = %v, want WorkflowErrorInvalidStepOutput", err)
	}
}

func TestResumeContinuesFromSnapshotWithoutReExecutingCompletedSteps(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	clock := NewFixedClock(time.Unix(9000, 0))
	ids := NewFixedIDSource([]RunID{"resume-run-1"}, nil)
	recorder := NewRunRecorder()

	var beforeCalls int32
	var afterCalls int32
	beforeHandler := StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
		atomic.AddInt32(&beforeCalls, 1)
		return json.RawMessage(`{"step":"before"}`), nil
	})
	suspendHandler := StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
		return nil, &SuspendError{Signal: SuspendSignal{
			StepID:   "await",
			Contract: json.RawMessage(`{"approved":true}`),
			Payload:  json.RawMessage(`{"pending":"human"}`),
		}}
	})
	afterHandler := StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
		atomic.AddInt32(&afterCalls, 1)
		return json.RawMessage(`{"step":"after"}`), nil
	})

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition:     WorkflowDefinition{ID: "resume-wf", Version: "v1"},
		SchemaCompiler: contractCompiler(),
		Clock:          clock,
		IDSource:       ids,
		Listener:       recorder,
		Store:          store,
		Steps: []Step{
			{Definition: StepDefinition{ID: "before"}, Handler: beforeHandler},
			{Definition: StepDefinition{ID: "await", SuspendSchema: json.RawMessage(`{"const":{"approved":true}}`)}, Handler: suspendHandler},
			{Definition: StepDefinition{ID: "after"}, Handler: afterHandler},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	suspended, err := wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`"start"`)})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if suspended.Status != RunStatusSuspended {
		t.Fatalf("Status = %q, want suspended", suspended.Status)
	}
	if got := atomic.LoadInt32(&beforeCalls); got != 1 {
		t.Fatalf("before handler ran %d times on first run, want 1", got)
	}
	if got := atomic.LoadInt32(&afterCalls); got != 0 {
		t.Fatalf("after handler ran %d times on suspend, want 0", got)
	}

	// Resuming must reject invalid input without touching state.
	_, err = wf.Resume(ctx, WorkflowResumeInput{RunID: suspended.ID, Input: json.RawMessage(`{"approved":false}`)})
	if err == nil {
		t.Fatal("Resume with invalid input returned nil error")
	}
	if !errors.Is(err, ErrInvalidResumeInput) {
		t.Fatalf("invalid resume err = %v, want ErrInvalidResumeInput", err)
	}
	// State untouched: run still suspended, after still not run.
	stored, err := store.WorkflowRuns().GetWorkflowRun(ctx, suspended.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != RunStatusSuspended {
		t.Fatalf("after invalid resume, stored status = %q, want suspended", stored.Status)
	}
	if got := atomic.LoadInt32(&afterCalls); got != 0 {
		t.Fatalf("after handler ran %d times after invalid resume, want 0", got)
	}

	resumed, err := wf.Resume(ctx, WorkflowResumeInput{RunID: suspended.ID, Input: json.RawMessage(`{"approved":true}`)})
	if err != nil {
		t.Fatalf("Resume returned error: %v", err)
	}
	if resumed.Status != RunStatusSucceeded {
		t.Fatalf("resumed Status = %q, want succeeded", resumed.Status)
	}
	var out any
	if err := json.Unmarshal(resumed.Output, &out); err != nil {
		t.Fatal(err)
	}
	if !deepEqualJSON(out, map[string]any{"step": "after"}) {
		t.Fatalf("resumed Output = %s, want {\"step\":\"after\"}", resumed.Output)
	}
	if got := atomic.LoadInt32(&beforeCalls); got != 1 {
		t.Fatalf("before handler ran %d times total, want 1 (resume must not re-execute completed steps)", got)
	}
	if got := atomic.LoadInt32(&afterCalls); got != 1 {
		t.Fatalf("after handler ran %d times, want 1", got)
	}

	// Run history identifies suspended and resumed transitions.
	events := recorder.Events()
	foundSuspended := false
	foundResumed := false
	for _, ev := range events {
		if ev.Type == RunEventSuspended {
			foundSuspended = true
		}
		if ev.Type == RunEventResumed {
			foundResumed = true
		}
	}
	if !foundSuspended {
		t.Error("no run_suspended event recorded")
	}
	if !foundResumed {
		t.Error("no run_resumed event recorded")
	}
}

func TestResumeWithoutStoreErrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	suspendHandler := StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
		return nil, &SuspendError{Signal: SuspendSignal{Contract: json.RawMessage(`{}`)}}
	})
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition:     WorkflowDefinition{ID: "no-store-suspend", Version: "v1"},
		SchemaCompiler: contractCompiler(),
		Steps: []Step{
			{Definition: StepDefinition{ID: "s1", SuspendSchema: json.RawMessage(`{}`)}, Handler: suspendHandler},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	suspended, err := wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`"start"`)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = wf.Resume(ctx, WorkflowResumeInput{RunID: suspended.ID, Input: json.RawMessage(`{}`)})
	if !errors.Is(err, ErrWorkflowResumeRequiresStore) {
		t.Fatalf("err = %v, want ErrWorkflowResumeRequiresStore", err)
	}
}

func TestResumeOnNonSuspendedRunErrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ids := NewFixedIDSource([]RunID{"succeeded-run-1"}, nil)
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition:     WorkflowDefinition{ID: "completed-wf", Version: "v1"},
		SchemaCompiler: contractCompiler(),
		IDSource:       ids,
		Store:          store,
		Steps: []Step{
			{Definition: StepDefinition{ID: "s1"}, Handler: StepHandlerFunc(func(_ context.Context, in json.RawMessage) (json.RawMessage, error) { return in, nil })},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`"x"`)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunStatusSucceeded {
		t.Fatalf("setup Status = %q, want succeeded", res.Status)
	}
	_, err = wf.Resume(ctx, WorkflowResumeInput{RunID: res.ID, Input: json.RawMessage(`"x"`)})
	if !errors.Is(err, ErrNotSuspended) {
		t.Fatalf("err = %v, want ErrNotSuspended", err)
	}
}

func TestSuspendPersistsAcrossReopenAndResumes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "suspend.db")

	firstStore, err := NewSQLiteStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := firstStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	var beforeCalls int32
	var afterCalls int32
	beforeHandler := StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
		atomic.AddInt32(&beforeCalls, 1)
		return json.RawMessage(`{"step":"before"}`), nil
	})
	suspendHandler := StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
		return nil, &SuspendError{Signal: SuspendSignal{
			StepID:   "await",
			Contract: json.RawMessage(`{"approved":true}`),
			Payload:  json.RawMessage(`{"pending":"human"}`),
		}}
	})
	afterHandler := StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
		atomic.AddInt32(&afterCalls, 1)
		return json.RawMessage(`{"step":"after"}`), nil
	})

	buildWorkflow := func(store Store) *LinearWorkflow {
		wf, err := NewLinearWorkflow(LinearWorkflowConfig{
			Definition:     WorkflowDefinition{ID: "durable-suspend-wf", Version: "v1"},
			SchemaCompiler: contractCompiler(),
			IDSource:       NewFixedIDSource([]RunID{"durable-suspend-1"}, nil),
			Store:          store,
			Steps: []Step{
				{Definition: StepDefinition{ID: "before"}, Handler: beforeHandler},
				{Definition: StepDefinition{ID: "await", SuspendSchema: json.RawMessage(`{"const":{"approved":true}}`)}, Handler: suspendHandler},
				{Definition: StepDefinition{ID: "after"}, Handler: afterHandler},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return wf
	}

	wf1 := buildWorkflow(firstStore)
	suspended, err := wf1.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`"start"`)})
	if err != nil {
		t.Fatal(err)
	}
	if suspended.Status != RunStatusSuspended {
		t.Fatalf("Status = %q, want suspended", suspended.Status)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen the same file and resume from a fresh process.
	secondStore, err := NewSQLiteStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = secondStore.Close() }()
	if err := secondStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	wf2 := buildWorkflow(secondStore)

	// Invalid resume input rejected, snapshot untouched.
	_, err = wf2.Resume(ctx, WorkflowResumeInput{RunID: suspended.ID, Input: json.RawMessage(`{"approved":false}`)})
	if !errors.Is(err, ErrInvalidResumeInput) {
		t.Fatalf("invalid resume err = %v, want ErrInvalidResumeInput", err)
	}
	stored, err := secondStore.WorkflowRuns().GetWorkflowRun(ctx, suspended.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != RunStatusSuspended {
		t.Fatalf("after invalid resume stored status = %q, want suspended", stored.Status)
	}

	resumed, err := wf2.Resume(ctx, WorkflowResumeInput{RunID: suspended.ID, Input: json.RawMessage(`{"approved":true}`)})
	if err != nil {
		t.Fatalf("Resume returned error: %v", err)
	}
	if resumed.Status != RunStatusSucceeded {
		t.Fatalf("resumed Status = %q, want succeeded", resumed.Status)
	}
	if got := atomic.LoadInt32(&beforeCalls); got != 1 {
		t.Fatalf("before handler ran %d times across reopen, want 1", got)
	}
	if got := atomic.LoadInt32(&afterCalls); got != 1 {
		t.Fatalf("after handler ran %d times, want 1", got)
	}

	// Persisted run record now reflects success.
	finalRun, err := secondStore.WorkflowRuns().GetWorkflowRun(ctx, suspended.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finalRun.Status != RunStatusSucceeded {
		t.Fatalf("final stored status = %q, want succeeded", finalRun.Status)
	}
	if !bytes.Equal(finalRun.Output, json.RawMessage(`{"step":"after"}`)) {
		t.Fatalf("final stored output = %s, want {\"step\":\"after\"}", finalRun.Output)
	}
}
