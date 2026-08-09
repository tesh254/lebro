package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// failingRunRepo wraps a WorkflowRunRepository and fails the SaveWorkflowRun
// call at failOn (1-indexed) so persistence hard-failure paths can be
// exercised at a chosen persistence point.
type failingRunRepo struct {
	WorkflowRunRepository
	calls  int
	failOn int
	fail   error
}

func (r *failingRunRepo) SaveWorkflowRun(ctx context.Context, v WorkflowRunRecord) error {
	r.calls++
	if r.fail != nil && r.calls == r.failOn {
		return r.fail
	}
	return r.WorkflowRunRepository.SaveWorkflowRun(ctx, v)
}

// failingSnapshotRepo wraps a WorkflowSnapshotRepository and fails the
// SaveWorkflowSnapshot call at failOn (1-indexed) so the step-boundary
// transaction rollback path can be exercised.
type failingSnapshotRepo struct {
	WorkflowSnapshotRepository
	calls  int
	failOn int
	fail   error
}

func (r *failingSnapshotRepo) SaveWorkflowSnapshot(ctx context.Context, v WorkflowSnapshotRecord) error {
	r.calls++
	if r.fail != nil && r.calls == r.failOn {
		return r.fail
	}
	return r.WorkflowSnapshotRepository.SaveWorkflowSnapshot(ctx, v)
}

// failingRepositoriesStore wraps a Store and swaps in failing repositories
// so the LinearWorkflow persistence hard-failure path surfaces a
// WorkflowErrorStepFailed wrapping the storage error. Transaction is
// overridden so the failing snapshot repo is also visible inside the
// caller's transaction; the underlying Store still owns the atomic commit
// boundary, so a snapshot-write failure rolls the whole boundary back.
type failingRepositoriesStore struct {
	Store
	runRepo      WorkflowRunRepository
	snapshotRepo WorkflowSnapshotRepository
}

func (s *failingRepositoriesStore) WorkflowRuns() WorkflowRunRepository { return s.runRepo }
func (s *failingRepositoriesStore) WorkflowSnapshots() WorkflowSnapshotRepository {
	return s.snapshotRepo
}

func (s *failingRepositoriesStore) Transaction(ctx context.Context, fn func(context.Context, Repositories) error) error {
	return s.Store.Transaction(ctx, func(ctx context.Context, repos Repositories) error {
		wrapped := &failingTxRepositories{
			Repositories: repos,
			runRepo:      s.runRepo,
			snapshotRepo: s.snapshotRepo,
		}
		return fn(ctx, wrapped)
	})
}

type failingTxRepositories struct {
	Repositories
	runRepo      WorkflowRunRepository
	snapshotRepo WorkflowSnapshotRepository
}

func (r *failingTxRepositories) WorkflowRuns() WorkflowRunRepository           { return r.runRepo }
func (r *failingTxRepositories) WorkflowSnapshots() WorkflowSnapshotRepository { return r.snapshotRepo }

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

	// The Running record written at each step boundary must carry the
	// anchor's stable fields (input, thread ID, metadata, original start
	// time) and the completed outputs through the current step, so a
	// process stop after a committed step leaves a resumable, inspectable
	// record. The final terminal record supersedes it; we assert the
	// boundary shape by reading the snapshot envelope's outputs and by
	// exercising the step-boundary failure path below.
	if string(run.Metadata) != `{"source":"test"}` {
		t.Fatalf("run metadata = %q, want preserved from anchor", run.Metadata)
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
		Store:        store,
		runRepo:      &failingRunRepo{WorkflowRunRepository: store.WorkflowRuns(), failOn: 1, fail: storageErr},
		snapshotRepo: store.WorkflowSnapshots(),
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

// TestLinearWorkflowStepBoundaryFailureRollsBackTransaction asserts the
// PR's core guarantee: when the step-boundary Transaction fails, neither the
// snapshot nor the updated run record is persisted, so a restart never
// observes a partially persisted step. The failure is injected at the
// snapshot write so the run-record update inside the same Transaction never
// commits either.
func TestLinearWorkflowStepBoundaryFailureRollsBackTransaction(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	storageErr := errors.New("lebro: snapshot write failed")
	wrapped := &failingRepositoriesStore{
		Store:        store,
		runRepo:      store.WorkflowRuns(),
		snapshotRepo: &failingSnapshotRepo{WorkflowSnapshotRepository: store.WorkflowSnapshots(), failOn: 1, fail: storageErr},
	}

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "rollback-wf", Version: "v1"},
		Steps: []Step{
			{Definition: StepDefinition{ID: "s1"}, Handler: StepHandlerFunc(func(_ context.Context, in json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`{"after":"s1"}`), nil
			})},
			{Definition: StepDefinition{ID: "s2"}, Handler: StepHandlerFunc(func(_ context.Context, in json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`{"after":"s2"}`), nil
			})},
		},
		IDSource: NewFixedIDSource([]RunID{"rollback-run-1"}, nil),
		Clock:    NewFixedClock(time.Unix(9400, 0)),
		Store:    wrapped,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, runErr := wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`{"start":true}`)})
	if runErr == nil {
		t.Fatal("Run() error = nil, want step-boundary persistence failure")
	}
	var wfErr *WorkflowError
	if !errors.As(runErr, &wfErr) || wfErr.Kind != WorkflowErrorStepFailed {
		t.Fatalf("Run() error = %v, want *WorkflowError{step_failed}", runErr)
	}
	if !errors.Is(runErr, storageErr) {
		t.Fatalf("error = %v, want it to wrap storage error %v", runErr, storageErr)
	}

	// The run-start record committed before the first step, but the
	// step-1 boundary Transaction must have rolled back entirely: no
	// snapshot and no step output persisted. The terminal Failed record
	// is written best-effort on the failure path, so the run reflects the
	// failure with no completed step outputs.
	run, err := store.WorkflowRuns().GetWorkflowRun(ctx, "rollback-run-1")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunStatusFailed || run.CurrentStep != 0 || run.CurrentStepID != "" {
		t.Fatalf("run after rolled-back boundary = %#v, want Failed at step 0", run)
	}
	if len(run.StepOutputs) != 0 {
		t.Fatalf("step outputs = %d, want 0 (boundary rolled back)", len(run.StepOutputs))
	}
	snapshots, err := store.WorkflowSnapshots().ListWorkflowSnapshots(ctx, "rollback-run-1", PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots.Records) != 0 {
		t.Fatalf("snapshots = %d, want 0 (boundary rolled back)", len(snapshots.Records))
	}
}

// TestLinearWorkflowTerminalPersistenceFailureFailsRun asserts the
// hard-fail policy extends to the terminal record: if durable storage still
// reports Running after a successful workflow, the caller observes a
// WorkflowErrorStepFailed rather than a completed run.
func TestLinearWorkflowTerminalPersistenceFailureFailsRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	// failOn=2: the first SaveWorkflowRun (run start) succeeds, the
	// step-boundary update (none here — single step — so no snapshot) is
	// skipped, and the terminal SaveWorkflowRun fails. With a single step
	// the save sequence is: run start, terminal.
	storageErr := errors.New("lebro: terminal write failed")
	wrapped := &failingRepositoriesStore{
		Store:        store,
		runRepo:      &failingRunRepo{WorkflowRunRepository: store.WorkflowRuns(), failOn: 2, fail: storageErr},
		snapshotRepo: store.WorkflowSnapshots(),
	}

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "terminal-fail-wf"},
		Steps: []Step{
			{Definition: StepDefinition{ID: "s1"}, Handler: StepHandlerFunc(func(_ context.Context, in json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`{"ok":true}`), nil
			})},
		},
		IDSource: NewFixedIDSource([]RunID{"terminal-fail-run-1"}, nil),
		Clock:    NewFixedClock(time.Unix(9500, 0)),
		Store:    wrapped,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, runErr := wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`{}`)})
	if runErr == nil {
		t.Fatal("Run() error = nil, want terminal persistence failure")
	}
	_ = result
	var wfErr *WorkflowError
	if !errors.As(runErr, &wfErr) || wfErr.Kind != WorkflowErrorStepFailed {
		t.Fatalf("Run() error = %v, want *WorkflowError{step_failed}", runErr)
	}
	if !errors.Is(runErr, storageErr) {
		t.Fatalf("error = %v, want it to wrap storage error %v", runErr, storageErr)
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
