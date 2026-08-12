package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// WorkflowResolver returns the runnable workflow for a schedule's WorkflowID.
// It reports ok=false when no workflow is registered for the ID, which the
// Scheduler records as a failed execution rather than a panic. Implementations
// must be safe for concurrent use.
type WorkflowResolver interface {
	ResolveWorkflow(WorkflowID) (*LinearWorkflow, bool)
}

// WorkflowMap is a static WorkflowResolver backed by a map. It is the common
// case: an application registers its workflows once at startup. A nil or absent
// entry resolves to ok=false.
type WorkflowMap map[WorkflowID]*LinearWorkflow

// ResolveWorkflow implements WorkflowResolver.
func (m WorkflowMap) ResolveWorkflow(id WorkflowID) (*LinearWorkflow, bool) {
	w, ok := m[id]
	return w, ok && w != nil
}

// SchedulerConfig configures a Scheduler. Store is required and must be
// migrated before use; it is the durable home for schedule definitions and
// execution history, so a schedule persisted before a restart is reloaded from
// it. Resolver maps a schedule's WorkflowID to a runnable workflow. Clock and
// IDSource default to the wall clock and a monotonic source when nil. Interval
// is the tick period used by Start; it defaults to one minute (cron precision)
// when zero and is ignored by direct Tick callers. MaxCatchUp bounds how many
// missed occurrences a single schedule records per tick so a schedule idle for
// a long outage does not write an unbounded history; it defaults to 100 when
// zero. A negative MaxCatchUp disables missed-occurrence recording entirely.
type SchedulerConfig struct {
	Store      Store
	Resolver   WorkflowResolver
	Clock      Clock
	IDSource   IDSource
	Interval   time.Duration
	MaxCatchUp int
}

// Scheduler fires durable schedules whose next fire time has arrived, reusing
// LinearWorkflow.Run for each execution and recording the outcome in the
// store's schedule execution history. It is safe for concurrent use. The zero
// value is not usable; construct one with NewScheduler.
//
// Tick is the testable core: it fires everything due at or before a caller
// supplied instant and returns what happened. Start wraps Tick in a background
// goroutine driven by the configured Clock; Stop halts it and joins the
// goroutine. Because due work is loaded from the store on every tick, a
// scheduler constructed over a store that already holds schedules resumes them
// after a process restart with no extra registration.
type Scheduler struct {
	store      Store
	resolver   WorkflowResolver
	clock      Clock
	idSource   IDSource
	interval   time.Duration
	maxCatchUp int

	mu       sync.Mutex
	inflight map[ScheduleID]struct{}

	startMu sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
}

// defaultMaxCatchUp bounds recorded missed occurrences per schedule per tick.
const defaultMaxCatchUp = 100

// NewScheduler validates the configuration and returns a scheduler ready to
// tick. It requires a non-nil Store and Resolver.
func NewScheduler(config SchedulerConfig) (*Scheduler, error) {
	if config.Store == nil || isNilInterface(config.Store) {
		return nil, errors.New("lebro: scheduler requires a store")
	}
	if config.Resolver == nil || isNilInterface(config.Resolver) {
		return nil, errors.New("lebro: scheduler requires a workflow resolver")
	}
	clock := config.Clock
	if clock == nil || isNilInterface(clock) {
		clock = defaultClock{}
	}
	idSource := config.IDSource
	if idSource == nil || isNilInterface(idSource) {
		idSource = &sequentialIDSource{}
	}
	interval := config.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	maxCatchUp := config.MaxCatchUp
	if maxCatchUp == 0 {
		maxCatchUp = defaultMaxCatchUp
	}
	return &Scheduler{
		store:      config.Store,
		resolver:   config.Resolver,
		clock:      clock,
		idSource:   idSource,
		interval:   interval,
		maxCatchUp: maxCatchUp,
		inflight:   make(map[ScheduleID]struct{}),
	}, nil
}

// TickResult reports what a Tick did. Fired is the number of schedules whose
// workflow was started this tick (whether the run then succeeded or failed);
// Skipped counts schedules dropped by their concurrency policy; Missed counts
// missed-occurrence history entries recorded for elapsed fires. Executions
// holds every execution record written during the tick, newest work last, so a
// caller can inspect outcomes without re-reading the store.
type TickResult struct {
	Fired      int
	Skipped    int
	Missed     int
	Executions []ScheduleExecutionRecord
}

// Tick fires every schedule that is due at or before now, in schedule order. It
// loads due schedules from the store, so it observes any schedule persisted
// before this process started. For each due schedule it:
//
//   - records missed occurrences for any fires strictly between the schedule's
//     current NextFireAt and the latest due occurrence (bounded by MaxCatchUp);
//   - applies the concurrency policy: under ConcurrencySkip a schedule whose
//     prior run is still in flight records a skipped execution and advances;
//   - otherwise runs the resolved workflow, recording a succeeded or failed
//     execution with the resulting run ID;
//   - advances NextFireAt to the first fire strictly after now (nil when the
//     spec has no further fire) and persists the updated schedule.
//
// A store error while loading due schedules fails the tick. A per-schedule
// error (unresolvable workflow, run failure) is recorded in history and does
// not abort the remaining schedules. now is interpreted in UTC.
func (s *Scheduler) Tick(ctx context.Context, now time.Time) (TickResult, error) {
	if err := ctx.Err(); err != nil {
		return TickResult{}, err
	}
	now = now.UTC()
	due, err := s.loadDue(ctx, now)
	if err != nil {
		return TickResult{}, err
	}
	var result TickResult
	for _, schedule := range due {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := s.fireSchedule(ctx, now, schedule, &result); err != nil {
			return result, err
		}
	}
	return result, nil
}

// loadDue reads every non-paused schedule due at or before now, following the
// store's pagination so a large fleet is fully loaded.
func (s *Scheduler) loadDue(ctx context.Context, now time.Time) ([]ScheduleRecord, error) {
	var schedules []ScheduleRecord
	filter := ScheduleFilter{DueBy: &now}
	page := PageRequest{}
	for {
		result, err := s.store.Schedules().ListSchedules(ctx, filter, page)
		if err != nil {
			return nil, fmt.Errorf("lebro: scheduler: load due schedules: %w", err)
		}
		schedules = append(schedules, result.Records...)
		if result.NextCursor == "" {
			break
		}
		page.Cursor = result.NextCursor
	}
	return schedules, nil
}

// fireSchedule processes one due schedule: catch-up, concurrency, run, advance.
//
// When a schedule is overdue by more than one interval (for example after a
// process outage), only the most recent due occurrence runs; every earlier due
// occurrence is recorded as missed so the gap is visible without replaying a
// backlog of runs. scheduledFor is that most-recent occurrence.
func (s *Scheduler) fireSchedule(ctx context.Context, now time.Time, schedule ScheduleRecord, result *TickResult) error {
	compiled, parseErr := ParseCronSpec(schedule.Spec)
	// A nil NextFireAt would not have been returned as due, so this deref is
	// safe. It is the earliest due occurrence.
	firstDue := now
	if schedule.NextFireAt != nil {
		firstDue = schedule.NextFireAt.UTC()
	}

	// The occurrence that runs this tick is the latest due one; earlier due
	// occurrences are recorded as missed.
	scheduledFor := firstDue
	if parseErr == nil {
		latest, err := s.recordMissed(ctx, schedule, firstDue, now, compiled, result)
		if err != nil {
			return err
		}
		scheduledFor = latest
	}

	// Concurrency: under Skip, a still-running prior run drops this fire.
	if schedule.Concurrency.normalized() == ConcurrencySkip && s.isInflight(schedule.ID) {
		exec := s.newExecution(schedule.ID, "", ScheduleExecSkipped, scheduledFor, now, &now, "")
		if err := s.advance(ctx, schedule, compiled, parseErr, now, exec, result); err != nil {
			return err
		}
		result.Skipped++
		return nil
	}

	if parseErr != nil {
		// An unparseable spec cannot advance; record the failure and clear the
		// next fire so it is not reloaded every tick as due.
		msg := fmt.Sprintf("invalid schedule spec %q: %v", schedule.Spec, parseErr)
		exec := s.newExecution(schedule.ID, "", ScheduleExecFailed, scheduledFor, now, &now, msg)
		return s.advanceCleared(ctx, schedule, now, exec, result)
	}

	workflow, ok := s.resolver.ResolveWorkflow(schedule.WorkflowID)
	if !ok {
		msg := fmt.Sprintf("no workflow registered for %q", schedule.WorkflowID)
		exec := s.newExecution(schedule.ID, "", ScheduleExecFailed, scheduledFor, now, &now, msg)
		if err := s.advance(ctx, schedule, compiled, parseErr, now, exec, result); err != nil {
			return err
		}
		result.Fired++
		return nil
	}

	runID, runErr := s.runWorkflow(ctx, schedule, workflow)
	finished := s.clock.Now().UTC()
	status := ScheduleExecSucceeded
	errMsg := ""
	if runErr != nil {
		status = ScheduleExecFailed
		errMsg = runErr.Error()
	}
	exec := s.newExecution(schedule.ID, runID, status, scheduledFor, now, &finished, errMsg)
	if err := s.advance(ctx, schedule, compiled, parseErr, now, exec, result); err != nil {
		return err
	}
	result.Fired++
	return nil
}

// runWorkflow marks the schedule in flight for the duration of the run so an
// overlapping tick under ConcurrencySkip observes it, then runs the workflow.
func (s *Scheduler) runWorkflow(ctx context.Context, schedule ScheduleRecord, workflow *LinearWorkflow) (RunID, error) {
	s.markInflight(schedule.ID)
	defer s.clearInflight(schedule.ID)

	input := WorkflowRunInput{Input: append(json.RawMessage(nil), schedule.Input...)}
	if len(schedule.Metadata) > 0 {
		meta := map[string]string{}
		if err := json.Unmarshal(schedule.Metadata, &meta); err == nil {
			input.Metadata = meta
		}
	}
	runResult, err := workflow.Run(ctx, input)
	return runResult.ID, err
}

// recordMissed records every due occurrence between firstDue and the latest
// occurrence at or before now as missed, and returns that latest occurrence —
// the one that actually runs this tick. When the schedule is due exactly once
// (the common case) nothing is recorded and firstDue is returned unchanged.
//
// The walk is bounded by MaxCatchUp so a schedule idle across a long outage
// records at most that many missed entries; when the cap is hit the latest
// reachable occurrence is used and the remaining backlog is dropped (logged via
// the returned count, not replayed). A negative MaxCatchUp disables recording
// but still advances to the latest occurrence so no backlog of runs replays.
func (s *Scheduler) recordMissed(ctx context.Context, schedule ScheduleRecord, firstDue, now time.Time, compiled CronSchedule, result *TickResult) (time.Time, error) {
	latest := firstDue
	// Walk forward from firstDue. Each next fire still at or before now is a
	// later due occurrence; the previous one it superseded was missed.
	for count := 0; s.maxCatchUp < 0 || count < s.maxCatchUp; count++ {
		next, ok := compiled.Next(latest)
		if !ok || next.After(now) {
			break
		}
		if s.maxCatchUp >= 0 {
			exec := s.newExecution(schedule.ID, "", ScheduleExecMissed, latest, now, &now, "")
			if err := s.store.ScheduleExecutions().SaveScheduleExecution(ctx, exec); err != nil {
				return latest, fmt.Errorf("lebro: scheduler: record missed occurrence: %w", err)
			}
			result.Missed++
			result.Executions = append(result.Executions, exec)
		}
		latest = next
	}
	return latest, nil
}

// advance persists the execution record and the schedule with LastFireAt set to
// the occurrence just processed and NextFireAt advanced to the first fire
// strictly after now (nil when the spec is exhausted).
func (s *Scheduler) advance(ctx context.Context, schedule ScheduleRecord, compiled CronSchedule, parseErr error, now time.Time, exec ScheduleExecutionRecord, result *TickResult) error {
	scheduledFor := exec.ScheduledFor
	var nextFire *time.Time
	if parseErr == nil {
		if next, ok := compiled.Next(now); ok {
			nextFire = &next
		}
	}
	updated := schedule
	updated.LastFireAt = &scheduledFor
	updated.NextFireAt = nextFire
	updated.UpdatedAt = now
	return s.persist(ctx, updated, exec, result)
}

// advanceCleared persists the execution and a schedule whose NextFireAt is
// cleared, used when the spec cannot produce further fires.
func (s *Scheduler) advanceCleared(ctx context.Context, schedule ScheduleRecord, now time.Time, exec ScheduleExecutionRecord, result *TickResult) error {
	scheduledFor := exec.ScheduledFor
	updated := schedule
	updated.LastFireAt = &scheduledFor
	updated.NextFireAt = nil
	updated.UpdatedAt = now
	if err := s.persist(ctx, updated, exec, result); err != nil {
		return err
	}
	result.Fired++
	return nil
}

// persist writes the execution record and the updated schedule atomically so a
// crash cannot leave a schedule advanced without its history entry, or a
// history entry without the advance.
func (s *Scheduler) persist(ctx context.Context, schedule ScheduleRecord, exec ScheduleExecutionRecord, result *TickResult) error {
	err := s.store.Transaction(ctx, func(txCtx context.Context, repos Repositories) error {
		if err := repos.ScheduleExecutions().SaveScheduleExecution(txCtx, exec); err != nil {
			return err
		}
		return repos.Schedules().SaveSchedule(txCtx, schedule)
	})
	if err != nil {
		return fmt.Errorf("lebro: scheduler: persist schedule %q: %w", schedule.ID, err)
	}
	result.Executions = append(result.Executions, exec)
	return nil
}

func (s *Scheduler) newExecution(scheduleID ScheduleID, runID RunID, status ScheduleExecStatus, scheduledFor, startedAt time.Time, finishedAt *time.Time, errMsg string) ScheduleExecutionRecord {
	return ScheduleExecutionRecord{
		ID:           string(s.idSource.NewRunID()),
		ScheduleID:   scheduleID,
		RunID:        runID,
		Status:       status,
		ScheduledFor: scheduledFor.UTC(),
		StartedAt:    startedAt.UTC(),
		FinishedAt:   finishedAt,
		Error:        errMsg,
	}
}

func (s *Scheduler) isInflight(id ScheduleID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.inflight[id]
	return ok
}

func (s *Scheduler) markInflight(id ScheduleID) {
	s.mu.Lock()
	s.inflight[id] = struct{}{}
	s.mu.Unlock()
}

func (s *Scheduler) clearInflight(id ScheduleID) {
	s.mu.Lock()
	delete(s.inflight, id)
	s.mu.Unlock()
}

// ErrSchedulerRunning is returned by Start when the scheduler already has a
// running background loop.
var ErrSchedulerRunning = errors.New("lebro: scheduler already running")

// Start launches a background goroutine that calls Tick every Interval using
// the configured Clock, until Stop is called or ctx is cancelled. It returns
// ErrSchedulerRunning if a loop is already active. Tick errors from the loop
// are dropped (the next tick retries); callers that need per-tick error
// handling should call Tick directly.
func (s *Scheduler) Start(ctx context.Context) error {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if s.done != nil {
		return ErrSchedulerRunning
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.cancel = cancel
	s.done = done
	go s.loop(loopCtx, done)
	return nil
}

// Stop halts the background loop started by Start and waits for its goroutine
// to exit. It is a no-op when the scheduler is not running and is safe to call
// more than once.
func (s *Scheduler) Stop() {
	s.startMu.Lock()
	cancel := s.cancel
	done := s.done
	s.cancel = nil
	s.done = nil
	s.startMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

func (s *Scheduler) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Ignore per-tick errors: the loop is best-effort and the next tick
			// reloads due work from the store. A cancelled context exits above.
			_, _ = s.Tick(ctx, s.clock.Now())
		}
	}
}
