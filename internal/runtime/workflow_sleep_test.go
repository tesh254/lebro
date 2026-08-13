package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type wakeupFailureState struct {
	runSaves      int
	snapshotSaves int
	enabled       bool
	err           error
}

type wakeupFailureStore struct {
	Store
	state *wakeupFailureState
}

func (s *wakeupFailureStore) WorkflowRuns() WorkflowRunRepository {
	return &wakeupFailureRunRepo{WorkflowRunRepository: s.Store.WorkflowRuns(), state: s.state}
}

func (s *wakeupFailureStore) WorkflowSnapshots() WorkflowSnapshotRepository {
	return &wakeupFailureSnapshotRepo{WorkflowSnapshotRepository: s.Store.WorkflowSnapshots(), state: s.state}
}

func (s *wakeupFailureStore) Transaction(ctx context.Context, fn func(context.Context, Repositories) error) error {
	return s.Store.Transaction(ctx, func(ctx context.Context, repos Repositories) error {
		return fn(ctx, &failingTxRepositories{
			Repositories: repos,
			runRepo:      &wakeupFailureRunRepo{WorkflowRunRepository: repos.WorkflowRuns(), state: s.state},
			snapshotRepo: &wakeupFailureSnapshotRepo{WorkflowSnapshotRepository: repos.WorkflowSnapshots(), state: s.state},
		})
	})
}

type wakeupFailureRunRepo struct {
	WorkflowRunRepository
	state *wakeupFailureState
}

func (r *wakeupFailureRunRepo) SaveWorkflowRun(ctx context.Context, record WorkflowRunRecord) error {
	r.state.runSaves++
	if r.state.enabled && r.state.runSaves == 3 {
		return r.state.err
	}
	return r.WorkflowRunRepository.SaveWorkflowRun(ctx, record)
}

type wakeupFailureSnapshotRepo struct {
	WorkflowSnapshotRepository
	state *wakeupFailureState
}

func (r *wakeupFailureSnapshotRepo) SaveWorkflowSnapshot(ctx context.Context, record WorkflowSnapshotRecord) error {
	r.state.snapshotSaves++
	if r.state.enabled && r.state.snapshotSaves == 2 {
		return r.state.err
	}
	return r.WorkflowSnapshotRepository.SaveWorkflowSnapshot(ctx, record)
}

func TestSleepSurvivesReopenAndWakeupIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	start := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	due := start.Add(time.Hour)
	dsn := filepath.Join(t.TempDir(), "sleep.db")
	store, err := NewSQLiteStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	var afterCalls int32
	build := func(store Store, clock Clock) *LinearWorkflow {
		wf, err := NewLinearWorkflow(LinearWorkflowConfig{
			Definition: WorkflowDefinition{ID: "sleep-wf", Version: "v1"},
			Store:      store,
			Clock:      clock,
			IDSource:   NewFixedIDSource([]RunID{"sleep-run"}, nil),
			Steps: []Step{
				{Definition: StepDefinition{ID: "wait", Sleep: &Sleep{Duration: time.Hour}}},
				{Definition: StepDefinition{ID: "after"}, Handler: StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
					atomic.AddInt32(&afterCalls, 1)
					return input, nil
				})},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return wf
	}

	first := build(store, NewFixedClock(start))
	suspended, err := first.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`{"value":1}`)})
	if err != nil || suspended.Status != RunStatusSuspended {
		t.Fatalf("Run = (%+v, %v), want suspended", suspended, err)
	}
	if _, err := first.Resume(ctx, WorkflowResumeInput{RunID: suspended.ID}); !errors.Is(err, ErrWorkflowSleepRequiresScheduler) {
		t.Fatalf("Resume sleep error = %v, want ErrWorkflowSleepRequiresScheduler", err)
	}
	wakeup, err := store.Schedules().GetSchedule(ctx, "sleep-run-wake-1")
	if err != nil || wakeup.NextFireAt == nil || !wakeup.NextFireAt.Equal(due) || wakeup.WakeRunID != suspended.ID || wakeup.WakeToken == "" {
		t.Fatalf("wakeup = (%+v, %v), want durable due wakeup", wakeup, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = NewSQLiteStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	resumed := build(store, NewFixedClock(due))
	scheduler := newTestScheduler(t, store, WorkflowMap{"sleep-wf": resumed}, NewFixedClock(due))

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := scheduler.Tick(cancelled, due); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Tick error = %v, want context.Canceled", err)
	}
	stillWaiting, err := store.WorkflowRuns().GetWorkflowRun(ctx, suspended.ID)
	if err != nil || stillWaiting.Status != RunStatusSuspended {
		t.Fatalf("run after cancelled Tick = (%+v, %v), want suspended", stillWaiting, err)
	}

	result, err := scheduler.Tick(ctx, due)
	if err != nil || result.Fired != 1 {
		t.Fatalf("Tick = (%+v, %v), want one fire", result, err)
	}
	if got := atomic.LoadInt32(&afterCalls); got != 1 {
		t.Fatalf("after calls = %d, want 1", got)
	}
	if _, err := scheduler.Tick(ctx, due); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if got := atomic.LoadInt32(&afterCalls); got != 1 {
		t.Fatalf("after calls after duplicate tick = %d, want 1", got)
	}
	completed, err := store.WorkflowRuns().GetWorkflowRun(ctx, suspended.ID)
	if err != nil || completed.Status != RunStatusSucceeded {
		t.Fatalf("resumed run = (%+v, %v), want succeeded", completed, err)
	}
}

func TestSleepUntilPersistsAbsoluteDeadline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	start := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	at := start.Add(2 * time.Hour)
	store := NewMemoryStore()
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "sleep-until-wf"}, Store: store, Clock: NewFixedClock(start), IDSource: NewFixedIDSource([]RunID{"until-run"}, nil),
		Steps: []Step{{Definition: StepDefinition{ID: "wait", SleepUntil: &SleepUntil{At: at}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`null`)}); err != nil {
		t.Fatal(err)
	}
	wakeup, err := store.Schedules().GetSchedule(ctx, "until-run-wake-1")
	if err != nil || wakeup.NextFireAt == nil || !wakeup.NextFireAt.Equal(at) {
		t.Fatalf("wakeup = (%+v, %v), want %s", wakeup, err, at)
	}
}

func TestSQLiteMigratesExistingScheduleStoreForWakeups(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "prior-schema.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	priorVersion := -1
	for i, migration := range sqliteSchemaMigrations {
		if strings.Contains(migration, "wake_run_id") {
			priorVersion = i
			break
		}
	}
	if priorVersion < 0 || priorVersion+1 >= len(sqliteSchemaMigrations) || !strings.Contains(sqliteSchemaMigrations[priorVersion+1], "wake_token") {
		t.Fatal("wake-up migrations must remain appended as wake_run_id then wake_token")
	}
	for i := 0; i < priorVersion; i++ {
		if _, err := store.db.ExecContext(ctx, sqliteSchemaMigrations[i]); err != nil {
			t.Fatalf("install prior migration %d: %v", i+1, err)
		}
	}
	if _, err := store.db.ExecContext(ctx, "PRAGMA user_version = "+fmt.Sprint(priorVersion)); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	if err := store.Schedules().SaveSchedule(ctx, ScheduleRecord{ID: "wake", WorkflowID: "wf", Spec: "@once", WakeRunID: "run", WakeToken: "token", NextFireAt: &now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
}

func TestWakeupRetriesWhenResumePersistenceFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	storageErr := errors.New("lebro: transient snapshot write")
	state := &wakeupFailureState{enabled: true, err: storageErr}
	wrapped := &wakeupFailureStore{Store: store, state: state}
	var afterCalls int32
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "retry-wakeup"}, Store: wrapped, Clock: NewFixedClock(now), IDSource: NewFixedIDSource([]RunID{"retry-run"}, nil),
		Steps: []Step{
			{Definition: StepDefinition{ID: "wait", Sleep: &Sleep{Duration: time.Hour}}},
			{Definition: StepDefinition{ID: "after"}, Handler: StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
				atomic.AddInt32(&afterCalls, 1)
				return input, nil
			})},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	suspended, err := wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`{"retry":true}`)})
	if err != nil || suspended.Status != RunStatusSuspended {
		t.Fatalf("Run = (%+v, %v), want suspended", suspended, err)
	}
	due := now.Add(time.Hour)
	scheduler := newTestScheduler(t, wrapped, WorkflowMap{"retry-wakeup": wf}, NewFixedClock(due))
	if _, err := scheduler.Tick(ctx, due); !errors.Is(err, storageErr) {
		t.Fatalf("first Tick error = %v, want %v", err, storageErr)
	}
	wakeup, err := store.Schedules().GetSchedule(ctx, "retry-run-wake-1")
	if err != nil || wakeup.NextFireAt == nil || wakeup.Paused {
		t.Fatalf("wakeup after transient failure = (%+v, %v), want rearmed", wakeup, err)
	}
	if got := atomic.LoadInt32(&afterCalls); got != 1 {
		t.Fatalf("after calls = %d, want 1", got)
	}
	state.enabled = false
	if _, err := scheduler.Tick(ctx, due); err != nil {
		t.Fatalf("retry Tick: %v", err)
	}
	if got := atomic.LoadInt32(&afterCalls); got != 2 {
		t.Fatalf("after calls after retry = %d, want 2", got)
	}
	completed, err := store.WorkflowRuns().GetWorkflowRun(ctx, suspended.ID)
	if err != nil || completed.Status != RunStatusSucceeded {
		t.Fatalf("retry run = (%+v, %v), want succeeded", completed, err)
	}
}
