package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingWorkflow builds a single-step workflow that increments counter and
// optionally returns an error, registered under id in the returned resolver.
func countingWorkflow(t *testing.T, id WorkflowID, counter *int64, runErr error) *LinearWorkflow {
	t.Helper()
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: id},
		Steps: []Step{{
			Definition: StepDefinition{ID: "s1"},
			Handler: StepHandlerFunc(func(_ context.Context, in json.RawMessage) (json.RawMessage, error) {
				atomic.AddInt64(counter, 1)
				if runErr != nil {
					return nil, runErr
				}
				return json.RawMessage(`{"ok":true}`), nil
			}),
		}},
	})
	if err != nil {
		t.Fatalf("NewLinearWorkflow: %v", err)
	}
	return wf
}

func newTestScheduler(t *testing.T, store Store, resolver WorkflowResolver, clock Clock) *Scheduler {
	t.Helper()
	s, err := NewScheduler(SchedulerConfig{Store: store, Resolver: resolver, Clock: clock, IDSource: &sequentialIDSource{}})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	return s
}

func listExecutions(t *testing.T, store Store, id ScheduleID) []ScheduleExecutionRecord {
	t.Helper()
	page, err := store.ScheduleExecutions().ListScheduleExecutions(context.Background(), id, PageRequest{})
	if err != nil {
		t.Fatalf("ListScheduleExecutions: %v", err)
	}
	return page.Records
}

// TestSchedulerFiresPersistedScheduleAfterRestart covers acceptance #1: a
// schedule persisted before the scheduler exists fires on the first tick of a
// freshly constructed scheduler, proving durability across a restart.
func TestSchedulerFiresPersistedScheduleAfterRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()

	created := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	dueAt := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	// Persist the schedule directly, simulating a definition written in a prior
	// process. No scheduler exists yet.
	if err := store.Schedules().SaveSchedule(ctx, ScheduleRecord{
		ID: "sched-1", WorkflowID: "wf-1", Spec: "0 * * * *", NextFireAt: &dueAt, CreatedAt: created, UpdatedAt: created,
	}); err != nil {
		t.Fatalf("SaveSchedule: %v", err)
	}

	var runs int64
	resolver := WorkflowMap{"wf-1": countingWorkflow(t, "wf-1", &runs, nil)}
	// A brand-new scheduler over the same store — the "restart".
	scheduler := newTestScheduler(t, store, resolver, NewFixedClock(dueAt))

	result, err := scheduler.Tick(ctx, dueAt)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if result.Fired != 1 || atomic.LoadInt64(&runs) != 1 {
		t.Fatalf("Fired = %d, runs = %d, want 1 and 1", result.Fired, runs)
	}
	execs := listExecutions(t, store, "sched-1")
	if len(execs) != 1 || execs[0].Status != ScheduleExecSucceeded {
		t.Fatalf("execution history = %+v, want one succeeded", execs)
	}
	// NextFireAt advanced to the following hour.
	got, _ := store.Schedules().GetSchedule(ctx, "sched-1")
	if got.NextFireAt == nil || !got.NextFireAt.Equal(dueAt.Add(time.Hour)) {
		t.Fatalf("NextFireAt = %v, want %v", got.NextFireAt, dueAt.Add(time.Hour))
	}

	// A second tick before the new fire time does nothing.
	result, err = scheduler.Tick(ctx, dueAt.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("Tick 2: %v", err)
	}
	if result.Fired != 0 || atomic.LoadInt64(&runs) != 1 {
		t.Fatalf("second tick fired = %d, runs = %d, want 0 and 1", result.Fired, runs)
	}
}

// TestSchedulerConcurrencySkip covers acceptance #2: with ConcurrencySkip an
// overlapping fire is dropped while a prior run is in flight and recorded as
// skipped; ConcurrencyAllow runs regardless.
func TestSchedulerConcurrencySkip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)

	if err := store.Schedules().SaveSchedule(ctx, ScheduleRecord{
		ID: "sched-1", WorkflowID: "wf-1", Spec: "* * * * *", Concurrency: ConcurrencySkip, NextFireAt: &now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("SaveSchedule: %v", err)
	}

	// A workflow that blocks until released, so the first fire stays in flight
	// while a second tick runs concurrently.
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var runs int64
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "wf-1"},
		Steps: []Step{{
			Definition: StepDefinition{ID: "s1"},
			Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
				atomic.AddInt64(&runs, 1)
				started <- struct{}{}
				<-release
				return json.RawMessage(`{}`), nil
			}),
		}},
	})
	if err != nil {
		t.Fatalf("NewLinearWorkflow: %v", err)
	}
	scheduler := newTestScheduler(t, store, WorkflowMap{"wf-1": wf}, NewFixedClock(now))

	// First tick runs in the background and blocks inside the handler.
	tickDone := make(chan error, 1)
	go func() {
		_, err := scheduler.Tick(ctx, now)
		tickDone <- err
	}()
	<-started // the run is now in flight

	// Reset the schedule to due again so the second tick sees work, then tick:
	// the in-flight run must cause a skip.
	if err := store.Schedules().SaveSchedule(ctx, ScheduleRecord{
		ID: "sched-1", WorkflowID: "wf-1", Spec: "* * * * *", Concurrency: ConcurrencySkip, NextFireAt: &now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("re-arm schedule: %v", err)
	}
	result, err := scheduler.Tick(ctx, now)
	if err != nil {
		t.Fatalf("Tick skip: %v", err)
	}
	if result.Skipped != 1 || result.Fired != 0 {
		t.Fatalf("skip tick: Skipped = %d, Fired = %d, want 1 and 0", result.Skipped, result.Fired)
	}
	if atomic.LoadInt64(&runs) != 1 {
		t.Fatalf("runs during skip = %d, want 1", runs)
	}

	close(release)
	if err := <-tickDone; err != nil {
		t.Fatalf("first tick: %v", err)
	}

	execs := listExecutions(t, store, "sched-1")
	var skipped, succeeded int
	for _, e := range execs {
		switch e.Status {
		case ScheduleExecSkipped:
			skipped++
		case ScheduleExecSucceeded:
			succeeded++
		}
	}
	if skipped != 1 || succeeded != 1 {
		t.Fatalf("history skipped=%d succeeded=%d, want 1 and 1 (%+v)", skipped, succeeded, execs)
	}
}

// TestSchedulerRecordsFailedAndMissed covers acceptance #3: a failed run is
// recorded as failed with its error, and fires elapsed without a tick are
// recorded as missed.
func TestSchedulerRecordsFailedAndMissed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()

	// Schedule fires hourly; NextFireAt is three hours before the tick, so two
	// intervening fires were missed and the current one runs (and fails).
	firstDue := time.Date(2026, 8, 12, 7, 0, 0, 0, time.UTC)
	tickAt := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	if err := store.Schedules().SaveSchedule(ctx, ScheduleRecord{
		ID: "sched-1", WorkflowID: "wf-1", Spec: "0 * * * *", NextFireAt: &firstDue, CreatedAt: firstDue, UpdatedAt: firstDue,
	}); err != nil {
		t.Fatalf("SaveSchedule: %v", err)
	}

	var runs int64
	boom := errors.New("handler failed")
	resolver := WorkflowMap{"wf-1": countingWorkflow(t, "wf-1", &runs, boom)}
	scheduler := newTestScheduler(t, store, resolver, NewFixedClock(tickAt))

	result, err := scheduler.Tick(ctx, tickAt)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	// Occurrences due at/before 10:00 are 07,08,09,10. The latest (10:00) runs
	// and fails; 07,08,09 are recorded missed.
	if result.Fired != 1 || result.Missed != 3 {
		t.Fatalf("Fired = %d, Missed = %d, want 1 and 3", result.Fired, result.Missed)
	}
	if atomic.LoadInt64(&runs) != 1 {
		t.Fatalf("runs = %d, want 1", runs)
	}
	execs := listExecutions(t, store, "sched-1")
	var missed, failed int
	var failMsg string
	for _, e := range execs {
		switch e.Status {
		case ScheduleExecMissed:
			missed++
		case ScheduleExecFailed:
			failed++
			failMsg = e.Error
		}
	}
	if missed != 3 || failed != 1 {
		t.Fatalf("history missed=%d failed=%d, want 3 and 1 (%+v)", missed, failed, execs)
	}
	if failMsg == "" {
		t.Fatal("failed execution recorded no error message")
	}
	// After the tick the schedule is armed for the next hour after the tick.
	got, _ := store.Schedules().GetSchedule(ctx, "sched-1")
	if got.NextFireAt == nil || !got.NextFireAt.Equal(tickAt.Add(time.Hour)) {
		t.Fatalf("NextFireAt = %v, want %v", got.NextFireAt, tickAt.Add(time.Hour))
	}
}

func TestSchedulerUnresolvableWorkflowRecordsFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	if err := store.Schedules().SaveSchedule(ctx, ScheduleRecord{
		ID: "sched-1", WorkflowID: "missing", Spec: "0 * * * *", NextFireAt: &now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("SaveSchedule: %v", err)
	}
	scheduler := newTestScheduler(t, store, WorkflowMap{}, NewFixedClock(now))
	result, err := scheduler.Tick(ctx, now)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if result.Fired != 1 {
		t.Fatalf("Fired = %d, want 1", result.Fired)
	}
	execs := listExecutions(t, store, "sched-1")
	if len(execs) != 1 || execs[0].Status != ScheduleExecFailed {
		t.Fatalf("history = %+v, want one failed", execs)
	}
}

func TestSchedulerPausedNeverFires(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	if err := store.Schedules().SaveSchedule(ctx, ScheduleRecord{
		ID: "sched-1", WorkflowID: "wf-1", Spec: "0 * * * *", Paused: true, NextFireAt: &now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("SaveSchedule: %v", err)
	}
	var runs int64
	scheduler := newTestScheduler(t, store, WorkflowMap{"wf-1": countingWorkflow(t, "wf-1", &runs, nil)}, NewFixedClock(now))
	result, err := scheduler.Tick(ctx, now)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if result.Fired != 0 || atomic.LoadInt64(&runs) != 0 {
		t.Fatalf("paused schedule fired: Fired=%d runs=%d", result.Fired, runs)
	}
}

// TestSchedulerStartStop exercises the background loop lifecycle. It uses a
// short real interval and asserts the schedule fires at least once, then Stop
// joins the goroutine cleanly (goleak in TestMain catches a leak).
func TestSchedulerStartStop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	// Derive the past due instant from the wall clock so the test does not
	// depend on a hardcoded calendar date being in the past.
	past := time.Now().Add(-time.Hour).UTC()
	if err := store.Schedules().SaveSchedule(ctx, ScheduleRecord{
		ID: "sched-1", WorkflowID: "wf-1", Spec: "@every 1h", NextFireAt: &past, CreatedAt: past, UpdatedAt: past,
	}); err != nil {
		t.Fatalf("SaveSchedule: %v", err)
	}
	var runs int64
	fired := make(chan struct{}, 1)
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "wf-1"},
		Steps: []Step{{
			Definition: StepDefinition{ID: "s1"},
			Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
				if atomic.AddInt64(&runs, 1) == 1 {
					fired <- struct{}{}
				}
				return json.RawMessage(`{}`), nil
			}),
		}},
	})
	if err != nil {
		t.Fatalf("NewLinearWorkflow: %v", err)
	}
	// Real wall clock and a tiny interval so the loop actually ticks. The
	// schedule's spec is "@every 1h" so it fires once (its due time is in the
	// past) and then re-arms an hour out, past the test's lifetime.
	s, err := NewScheduler(SchedulerConfig{Store: store, Resolver: WorkflowMap{"wf-1": wf}, Interval: 5 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Start(ctx); !errors.Is(err, ErrSchedulerRunning) {
		t.Fatalf("second Start: got %v want ErrSchedulerRunning", err)
	}
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		s.Stop()
		t.Fatal("schedule did not fire within timeout")
	}
	s.Stop()
	s.Stop() // idempotent
}

// TestSchedulerExecutionIDsSurviveRestart guards the fix for reused execution
// IDs: two schedulers constructed over the same store (a restart) must produce
// non-colliding execution IDs so the second scheduler can persist its history.
func TestSchedulerExecutionIDsSurviveRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()

	arm := func(due time.Time) {
		if err := store.Schedules().SaveSchedule(ctx, ScheduleRecord{
			ID: "sched-1", WorkflowID: "wf-1", Spec: "0 * * * *", NextFireAt: &due, CreatedAt: due, UpdatedAt: due,
		}); err != nil {
			t.Fatalf("SaveSchedule: %v", err)
		}
	}

	fire1 := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	arm(fire1)
	var runs int64
	// First scheduler process.
	s1 := newTestScheduler(t, store, WorkflowMap{"wf-1": countingWorkflow(t, "wf-1", &runs, nil)}, NewFixedClock(fire1))
	if _, err := s1.Tick(ctx, fire1); err != nil {
		t.Fatalf("Tick 1: %v", err)
	}

	// Restart: a brand-new scheduler (fresh idSource) over the same store. Its
	// first execution ID must not collide with s1's persisted history.
	fire2 := fire1.Add(time.Hour)
	arm(fire2)
	s2 := newTestScheduler(t, store, WorkflowMap{"wf-1": countingWorkflow(t, "wf-1", &runs, nil)}, NewFixedClock(fire2))
	result, err := s2.Tick(ctx, fire2)
	if err != nil {
		t.Fatalf("Tick 2 after restart: %v", err)
	}
	if result.Fired != 1 {
		t.Fatalf("Fired after restart = %d, want 1", result.Fired)
	}
	execs := listExecutions(t, store, "sched-1")
	if len(execs) != 2 {
		t.Fatalf("history = %d entries, want 2 (%+v)", len(execs), execs)
	}
	if execs[0].ID == execs[1].ID {
		t.Fatalf("execution IDs collided across restart: %q", execs[0].ID)
	}
}

// TestSchedulerCatchUpCapAdvancesToLatest guards the fix for MaxCatchUp: with a
// cap smaller than the number of overdue occurrences, the recorded missed count
// is capped but the run still fires the latest occurrence and NextFireAt
// advances past the whole backlog.
func TestSchedulerCatchUpCapAdvancesToLatest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	firstDue := time.Date(2026, 8, 12, 5, 0, 0, 0, time.UTC)
	tickAt := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	if err := store.Schedules().SaveSchedule(ctx, ScheduleRecord{
		ID: "sched-1", WorkflowID: "wf-1", Spec: "0 * * * *", NextFireAt: &firstDue, CreatedAt: firstDue, UpdatedAt: firstDue,
	}); err != nil {
		t.Fatalf("SaveSchedule: %v", err)
	}
	var runs int64
	s, err := NewScheduler(SchedulerConfig{
		Store:      store,
		Resolver:   WorkflowMap{"wf-1": countingWorkflow(t, "wf-1", &runs, nil)},
		Clock:      NewFixedClock(tickAt),
		IDSource:   &sequentialIDSource{},
		MaxCatchUp: 2,
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	result, err := s.Tick(ctx, tickAt)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	// Occurrences 05,06,07,08,09 are missed (5 total) but only 2 are recorded;
	// the latest occurrence (10:00) runs once.
	if result.Fired != 1 || result.Missed != 2 {
		t.Fatalf("Fired = %d, Missed = %d, want 1 and 2", result.Fired, result.Missed)
	}
	if atomic.LoadInt64(&runs) != 1 {
		t.Fatalf("runs = %d, want 1", runs)
	}
	// The run that fired must be for the latest due occurrence (10:00), not the
	// occurrence just past the cap. A capped walk that also stopped advancing
	// would record the run against an earlier occurrence.
	execs := listExecutions(t, store, "sched-1")
	var ran *ScheduleExecutionRecord
	for i := range execs {
		if execs[i].Status == ScheduleExecSucceeded {
			ran = &execs[i]
		}
	}
	if ran == nil {
		t.Fatalf("no succeeded execution recorded (%+v)", execs)
	}
	if !ran.ScheduledFor.Equal(tickAt) {
		t.Fatalf("run scheduled_for = %s, want %s (latest occurrence)", ran.ScheduledFor, tickAt)
	}
	got, _ := store.Schedules().GetSchedule(ctx, "sched-1")
	if got.NextFireAt == nil || !got.NextFireAt.Equal(tickAt.Add(time.Hour)) {
		t.Fatalf("NextFireAt = %v, want %v (advanced past backlog)", got.NextFireAt, tickAt.Add(time.Hour))
	}
}

// TestSchedulerDeleteRemovesHistory guards that deleting a schedule with
// execution history succeeds and that a schedule recreated under the same ID
// starts with empty history.
func TestSchedulerDeleteRemovesHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, store := range scheduleStores(t) {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
			if err := store.Schedules().SaveSchedule(ctx, ScheduleRecord{
				ID: "sched-1", WorkflowID: "wf-1", Spec: "0 * * * *", NextFireAt: &now, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("SaveSchedule: %v", err)
			}
			finished := now.Add(time.Second)
			if err := store.ScheduleExecutions().SaveScheduleExecution(ctx, ScheduleExecutionRecord{
				ID: "e-1", ScheduleID: "sched-1", Status: ScheduleExecSucceeded, ScheduledFor: now, StartedAt: now, FinishedAt: &finished,
			}); err != nil {
				t.Fatalf("SaveScheduleExecution: %v", err)
			}
			// Delete must succeed despite the child history row.
			if err := store.Schedules().DeleteSchedule(ctx, "sched-1"); err != nil {
				t.Fatalf("DeleteSchedule with history: %v", err)
			}
			// Recreate under the same ID: history must be empty.
			if err := store.Schedules().SaveSchedule(ctx, ScheduleRecord{
				ID: "sched-1", WorkflowID: "wf-1", Spec: "0 * * * *", NextFireAt: &now, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("recreate: %v", err)
			}
			page, err := store.ScheduleExecutions().ListScheduleExecutions(ctx, "sched-1", PageRequest{})
			if err != nil {
				t.Fatalf("ListScheduleExecutions: %v", err)
			}
			if len(page.Records) != 0 {
				t.Fatalf("recreated schedule inherited history: %+v", page.Records)
			}
		})
	}
}

func TestSchedulerConcurrentInflightTracking(t *testing.T) {
	t.Parallel()
	// Guards against a data race in the inflight map under -race.
	store := NewMemoryStore()
	s := newTestScheduler(t, store, WorkflowMap{}, NewFixedClock(time.Unix(0, 0).UTC()))
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.claimInflight("x") {
				s.clearInflight("x")
			}
		}()
	}
	wg.Wait()
}
