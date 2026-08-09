package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// failingRunRepo wraps a WorkflowRunRepository and fails its first
// SaveWorkflowRun call so persistence hard-failure paths can be exercised.
type failingRunRepo struct {
	WorkflowRunRepository
	calls int
	fail  error
}

func (r *failingRunRepo) SaveWorkflowRun(ctx context.Context, v WorkflowRunRecord) error {
	r.calls++
	if r.fail != nil && r.calls == 1 {
		return r.fail
	}
	return r.WorkflowRunRepository.SaveWorkflowRun(ctx, v)
}

// failingRepositoriesStore wraps a Store and swaps in a failing
// WorkflowRunRepository so the LinearWorkflow persistence hard-failure path
// surfaces a WorkflowErrorStepFailed wrapping the storage error.
type failingRepositoriesStore struct {
	Store
	runRepo WorkflowRunRepository
}

func (s *failingRepositoriesStore) WorkflowRuns() WorkflowRunRepository { return s.runRepo }

func TestLinearWorkflowPersistsRunAndSnapshotsAtStepBoundaries(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	clock := NewFixedClock(time.Unix(9000, 0))
	ids := NewFixedIDSource([]RunID{"persist-run-1"}, nil)
	recorder := NewRunRecorder()

	hits := 0
	handler := StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
		hits++
		return json.RawMessage(`{"step":` + string([]byte{byte('0' + hits)}) + `}`), nil
	})

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "durable-wf", Name: "Durable", Version: "v1"},
		Steps: []Step{
			{Definition: StepDefinition{ID: "s1"}, Handler: handler},
			{Definition: StepDefinition{ID: "s2"}, Handler: handler},
			{Definition: StepDefinition{ID: "s3"}, Handler: handler},
		},
		Listener: recorder,
		Clock:    clock,
		IDSource: ids,
		Store:    store,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := wf.Run(ctx, WorkflowRunInput{
		Input:    json.RawMessage(`{"start":true}`),
		ThreadID: "thread-1",
		Metadata: map[string]string{"source": "test"},
	})
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
	if string(result.Output) != `{"step":3}` {
		t.Fatalf("output = %q, want {\"step\":3}", result.Output)
	}

	run, err := store.WorkflowRuns().GetWorkflowRun(ctx, "persist-run-1")
	if err != nil {
		t.Fatal(err)
	}
	if run.WorkflowID != "durable-wf" || run.WorkflowVersion != "v1" || run.Status != RunStatusSucceeded {
		t.Fatalf("run header = %#v, want durable-wf/v1/succeeded", run)
	}
	if string(run.Input) != `{"start":true}` {
		t.Fatalf("run input = %q, want preserved", run.Input)
	}
	if run.ThreadID != "thread-1" {
		t.Fatalf("run thread = %q, want thread-1", run.ThreadID)
	}
	if run.CurrentStep != 3 || run.CurrentStepID != "s3" {
		t.Fatalf("current step = %d/%q, want 3/s3", run.CurrentStep, run.CurrentStepID)
	}
	if len(run.StepOutputs) != 3 {
		t.Fatalf("step outputs = %d, want 3", len(run.StepOutputs))
	}
	for i, want := range []string{`{"step":1}`, `{"step":2}`, `{"step":3}`} {
		if string(run.StepOutputs[i]) != want {
			t.Fatalf("step output %d = %q, want %q", i, run.StepOutputs[i], want)
		}
	}
	if run.Failure != nil {
		t.Fatalf("failure = %#v, want nil", run.Failure)
	}
	if run.FinishedAt == nil {
		t.Fatal("finished at = nil, want set on terminal run")
	}

	snapshots, err := store.WorkflowSnapshots().ListWorkflowSnapshots(ctx, "persist-run-1", PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots.Records) != 3 {
		t.Fatalf("snapshots = %d, want one per step (3)", len(snapshots.Records))
	}
	for i, snapshot := range snapshots.Records {
		if snapshot.SchemaVersion != workflowSnapshotSchemaVersion {
			t.Fatalf("snapshot %d schema version = %d, want %d", i, snapshot.SchemaVersion, workflowSnapshotSchemaVersion)
		}
		if snapshot.Sequence != int64(i+1) {
			t.Fatalf("snapshot %d sequence = %d, want %d", i, snapshot.Sequence, i+1)
		}
		var envelope workflowSnapshotEnvelope
		if err := json.Unmarshal(snapshot.State, &envelope); err != nil {
			t.Fatalf("snapshot %d state decode: %v", i, err)
		}
		if envelope.Step != i+1 {
			t.Fatalf("snapshot %d envelope step = %d, want %d", i, envelope.Step, i+1)
		}
		if len(envelope.Outputs) != i+1 {
			t.Fatalf("snapshot %d envelope outputs = %d, want %d", i, len(envelope.Outputs), i+1)
		}
	}
}

func TestLinearWorkflowPersistenceFailureFailsRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	storageErr := errors.New("lebro: simulated disk full")
	wrapped := &failingRepositoriesStore{
		Store:   store,
		runRepo: &failingRunRepo{WorkflowRunRepository: store.WorkflowRuns(), fail: storageErr},
	}

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fail-wf"},
		Steps: []Step{
			{Definition: StepDefinition{ID: "s1"}, Handler: StepHandlerFunc(func(_ context.Context, in json.RawMessage) (json.RawMessage, error) {
				return in, nil
			})},
		},
		IDSource: NewFixedIDSource([]RunID{"fail-run-1"}, nil),
		Clock:    NewFixedClock(time.Unix(9100, 0)),
		Store:    wrapped,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`{}`)})
	if err == nil {
		t.Fatal("Run() error = nil, want persistence failure")
	}
	var wfErr *WorkflowError
	if !errors.As(err, &wfErr) {
		t.Fatalf("Run() error = %v, want *WorkflowError", err)
	}
	if wfErr.Kind != WorkflowErrorStepFailed {
		t.Fatalf("kind = %q, want step_failed", wfErr.Kind)
	}
	if !errors.Is(err, storageErr) {
		t.Fatalf("error = %v, want it to wrap storage error %v", err, storageErr)
	}
}

func TestLinearWorkflowFailedRunPersistsFailureData(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "failed-wf", Version: "v2"},
		Steps: []Step{
			{Definition: StepDefinition{ID: "s1"}, Handler: StepHandlerFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
				return nil, errors.New("handler blew up")
			})},
		},
		IDSource: NewFixedIDSource([]RunID{"failed-run-1"}, nil),
		Clock:    NewFixedClock(time.Unix(9200, 0)),
		Store:    store,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, runErr := wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`{}`)})
	if runErr == nil {
		t.Fatal("Run() error = nil, want handler failure")
	}

	run, err := store.WorkflowRuns().GetWorkflowRun(ctx, "failed-run-1")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunStatusFailed {
		t.Fatalf("status = %q, want failed", run.Status)
	}
	if run.Failure == nil {
		t.Fatal("failure = nil, want recorded")
	}
	if run.Failure.Kind != WorkflowErrorStepFailed || run.Failure.StepID != "s1" || run.Failure.Message != "handler blew up" {
		t.Fatalf("failure = %#v, want step_failed at s1", run.Failure)
	}
	if run.WorkflowVersion != "v2" {
		t.Fatalf("version = %q, want v2", run.WorkflowVersion)
	}
}

func TestLinearWorkflowPersistenceSurvivesProcessRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("sqlite restart test opens a file-backed database")
	}
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "lebro-mad26-restart.db")

	first, err := NewSQLiteStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	if err := first.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "restart-wf", Version: "v1"},
		Steps: []Step{
			{Definition: StepDefinition{ID: "s1"}, Handler: StepHandlerFunc(func(_ context.Context, in json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`{"after":"s1"}`), nil
			})},
			{Definition: StepDefinition{ID: "s2"}, Handler: StepHandlerFunc(func(_ context.Context, in json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`{"after":"s2"}`), nil
			})},
		},
		IDSource: NewFixedIDSource([]RunID{"restart-run-1"}, nil),
		Clock:    NewFixedClock(time.Unix(9300, 0)),
		Store:    first,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`{"start":true}`)})
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen the same file: a process restart must not lose the latest
	// committed snapshot or the terminal run record.
	reopened, err := NewSQLiteStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	run, err := reopened.WorkflowRuns().GetWorkflowRun(ctx, "restart-run-1")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunStatusSucceeded || run.CurrentStep != 2 || run.CurrentStepID != "s2" {
		t.Fatalf("reopened run = %#v, want succeeded at s2", run)
	}
	if len(run.StepOutputs) != 2 {
		t.Fatalf("reopened step outputs = %d, want 2", len(run.StepOutputs))
	}
	snapshots, err := reopened.WorkflowSnapshots().ListWorkflowSnapshots(ctx, "restart-run-1", PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots.Records) != 2 {
		t.Fatalf("reopened snapshots = %d, want 2", len(snapshots.Records))
	}
	if snapshots.Records[1].SchemaVersion != workflowSnapshotSchemaVersion {
		t.Fatalf("last snapshot schema version = %d, want %d", snapshots.Records[1].SchemaVersion, workflowSnapshotSchemaVersion)
	}
}
