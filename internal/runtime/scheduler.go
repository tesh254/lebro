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
	Store Store
	// RuntimeStore is the capability-based alternative to Store. Exactly one
	// may be supplied. Schedules require both WorkflowState and Schedules;
	// Transactions remains optional and uses the documented sequential fallback.
	RuntimeStore RuntimeStore
	Resolver     WorkflowResolver
	Clock        Clock
	IDSource     IDSource
	Interval     time.Duration
	MaxCatchUp   int
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
	storeCaps  StoreCapabilities
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
	store := config.Store
	var caps StoreCapabilities
	if config.RuntimeStore != nil && !isNilInterface(config.RuntimeStore) {
		if store != nil && !isNilInterface(store) {
			return nil, errors.New("lebro: scheduler store and runtime store are mutually exclusive")
		}
		bridged, err := bridgeRuntimeStore(config.RuntimeStore)
		if err != nil {
			return nil, err
		}
		store, caps = bridged, config.RuntimeStore.Capabilities()
		if err := requireCapability(caps, StoreCapabilitySchedules, "scheduler"); err != nil {
			return nil, err
		}
		if err := requireCapability(caps, StoreCapabilityWorkflowState, "scheduler"); err != nil {
			return nil, err
		}
	} else {
		var err error
		caps, err = storeCapabilitiesOf(store)
		if err != nil {
			return nil, err
		}
	}
	if store == nil || isNilInterface(store) {
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
		idSource = NewUUIDIDSource()
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
		store:      store,
		storeCaps:  caps,
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
	if schedule.WakeRunID != "" {
		return s.fireWakeup(ctx, now, schedule, result)
	}
	compiled, parseErr := ParseCronSpec(schedule.Spec)
	// A nil NextFireAt would not have been returned as due, so this deref is
	// safe. It is the earliest due occurrence.
	firstDue := now
	if schedule.NextFireAt != nil {
		firstDue = schedule.NextFireAt.UTC()
	}

	// The occurrence that runs this tick is the latest due one; earlier due
	// occurrences are collected as missed. The missed records are persisted in
	// the same transaction as the advance below so a retry after a crash cannot
	// duplicate them.
	scheduledFor := firstDue
	var missed []ScheduleExecutionRecord
	if parseErr == nil {
		scheduledFor, missed = s.collectMissed(schedule, firstDue, now, compiled)
	}

	// Concurrency: under Skip, atomically claim the schedule. If a prior run is
	// still in flight the claim fails and this fire is skipped; otherwise the
	// claim marks it in flight for the run below. The claim is released after
	// the run (or immediately for the skip path).
	claimed := false
	if schedule.Concurrency.normalized() == ConcurrencySkip {
		if !s.claimInflight(schedule.ID) {
			exec := s.newExecution(schedule.ID, "", ScheduleExecSkipped, scheduledFor, now, &now, "")
			if err := s.advance(ctx, schedule, compiled, parseErr, now, exec, missed, result); err != nil {
				return err
			}
			result.Skipped++
			return nil
		}
		claimed = true
		defer s.clearInflight(schedule.ID)
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
		if err := s.advance(ctx, schedule, compiled, parseErr, now, exec, missed, result); err != nil {
			return err
		}
		result.Fired++
		return nil
	}

	runID, runErr := s.runWorkflow(ctx, schedule, workflow, claimed)
	finished := s.clock.Now().UTC()
	status := ScheduleExecSucceeded
	errMsg := ""
	if runErr != nil {
		status = ScheduleExecFailed
		errMsg = runErr.Error()
	}
	exec := s.newExecution(schedule.ID, runID, status, scheduledFor, now, &finished, errMsg)
	if err := s.advance(ctx, schedule, compiled, parseErr, now, exec, missed, result); err != nil {
		return err
	}
	result.Fired++
	return nil
}

// fireWakeup consumes one durable sleep wakeup. Wakeups deliberately bypass
// schedule concurrency because each one is fenced to one run snapshot. The
// token makes retrying a crash-interrupted tick safe: an already-resumed or
// later-suspended run is a successful no-op, never an accidental second resume.
func (s *Scheduler) fireWakeup(ctx context.Context, now time.Time, schedule ScheduleRecord, result *TickResult) error {
	scheduledFor := now
	if schedule.NextFireAt != nil {
		scheduledFor = schedule.NextFireAt.UTC()
	}
	workflow, ok := s.resolver.ResolveWorkflow(schedule.WorkflowID)
	status, message := ScheduleExecSucceeded, ""
	runID := schedule.WakeRunID
	if !ok {
		status, message = ScheduleExecFailed, fmt.Sprintf("no workflow registered for %q", schedule.WorkflowID)
	} else if resumeResult, err := workflow.resumeWake(ctx, schedule.WakeRunID, schedule.WakeToken); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if !errors.Is(err, errWorkflowWakeupStale) {
			terminal, stateErr := s.wakeupRunTerminal(ctx, schedule.WakeRunID)
			if stateErr != nil {
				return fmt.Errorf("lebro: scheduler: inspect wakeup run %q after resume error: %w", schedule.WakeRunID, stateErr)
			}
			if !terminal {
				return err
			}
			status, message = ScheduleExecFailed, err.Error()
		}
	} else if resumeResult.Status != RunStatusSucceeded && resumeResult.Status != RunStatusFailed && resumeResult.Status != RunStatusCancelled && resumeResult.Status != RunStatusSuspended {
		return fmt.Errorf("lebro: scheduler: wakeup run %q returned non-terminal status %q", schedule.WakeRunID, resumeResult.Status)
	}
	finished := s.clock.Now().UTC()
	exec := s.newExecution(schedule.ID, runID, status, scheduledFor, now, &finished, message)
	updated := schedule
	updated.LastFireAt = &scheduledFor
	updated.NextFireAt = nil
	updated.Paused = true
	updated.UpdatedAt = now
	if err := s.persist(ctx, updated, exec, nil, result); err != nil {
		return err
	}
	result.Fired++
	return nil
}

// wakeupRunTerminal reports whether a failed wake attempt committed a terminal
// run result. A persistence failure can leave the run Suspended; keeping its
// schedule due lets the next tick retry using the same fenced wake token.
func (s *Scheduler) wakeupRunTerminal(ctx context.Context, runID RunID) (bool, error) {
	if err := requireCapability(s.storeCaps, StoreCapabilityWorkflowState, "durable workflow wakeup"); err != nil {
		return false, err
	}
	run, err := s.store.WorkflowRuns().GetWorkflowRun(ctx, runID)
	if err != nil {
		return false, err
	}
	switch run.Status {
	case RunStatusSucceeded, RunStatusFailed, RunStatusCancelled:
		return true, nil
	default:
		return false, nil
	}
}

// runWorkflow runs the workflow for a due schedule. When the schedule was not
// already claimed under ConcurrencySkip, it marks the schedule in flight for
// the run duration so a concurrent tick observes it; claimed is true when the
// caller already holds the claim (ConcurrencySkip path) and must not double
// mark or clear it.
func (s *Scheduler) runWorkflow(ctx context.Context, schedule ScheduleRecord, workflow *LinearWorkflow, claimed bool) (RunID, error) {
	if !claimed {
		s.markInflight(schedule.ID)
		defer s.clearInflight(schedule.ID)
	}

	input := WorkflowRunInput{Input: append(json.RawMessage(nil), schedule.Input...), Scope: RuntimeScope{Namespace: schedule.Namespace, OwnerID: schedule.OwnerID}}
	if len(schedule.Metadata) > 0 {
		meta := map[string]string{}
		if err := json.Unmarshal(schedule.Metadata, &meta); err == nil {
			input.Metadata = meta
		}
	}
	runResult, err := workflow.Run(ctx, input)
	return runResult.ID, err
}

// collectMissed returns the latest due occurrence at or before now (the one
// that actually runs this tick) and the missed-execution records for the
// earlier due occurrences. The walk to the latest occurrence is unbounded so a
// long outage still advances correctly; only the number of recorded missed
// entries is capped at MaxCatchUp (a negative MaxCatchUp records none). When
// the schedule is due exactly once, latest is firstDue and missed is empty.
func (s *Scheduler) collectMissed(schedule ScheduleRecord, firstDue, now time.Time, compiled CronSchedule) (latest time.Time, missed []ScheduleExecutionRecord) {
	latest = firstDue
	for {
		next, ok := compiled.Next(latest)
		if !ok || next.After(now) {
			break
		}
		// latest is superseded by a later due occurrence, so it was missed.
		if s.maxCatchUp >= 0 && len(missed) < s.maxCatchUp {
			missed = append(missed, s.newExecution(schedule.ID, "", ScheduleExecMissed, latest, now, &now, ""))
		}
		latest = next
	}
	return latest, missed
}

// advance persists the missed records, the terminal execution record, and the
// schedule with LastFireAt set to the occurrence just processed and NextFireAt
// advanced to the first fire strictly after now (nil when the spec is
// exhausted) — all in one transaction.
func (s *Scheduler) advance(ctx context.Context, schedule ScheduleRecord, compiled CronSchedule, parseErr error, now time.Time, exec ScheduleExecutionRecord, missed []ScheduleExecutionRecord, result *TickResult) error {
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
	return s.persist(ctx, updated, exec, missed, result)
}

// advanceCleared persists the execution and a schedule whose NextFireAt is
// cleared, used when the spec cannot produce further fires.
func (s *Scheduler) advanceCleared(ctx context.Context, schedule ScheduleRecord, now time.Time, exec ScheduleExecutionRecord, result *TickResult) error {
	scheduledFor := exec.ScheduledFor
	updated := schedule
	updated.LastFireAt = &scheduledFor
	updated.NextFireAt = nil
	updated.UpdatedAt = now
	if err := s.persist(ctx, updated, exec, nil, result); err != nil {
		return err
	}
	result.Fired++
	return nil
}

// persist writes the missed records, the terminal execution record, and the
// updated schedule in one transaction so a crash cannot leave a schedule
// advanced without its history entries, or history entries without the advance,
// and a retry cannot duplicate the missed records.
func (s *Scheduler) persist(ctx context.Context, schedule ScheduleRecord, exec ScheduleExecutionRecord, missed []ScheduleExecutionRecord, result *TickResult) error {
	exec.Namespace, exec.OwnerID = schedule.Namespace, schedule.OwnerID
	for i := range missed {
		missed[i].Namespace, missed[i].OwnerID = schedule.Namespace, schedule.OwnerID
	}
	err := s.store.Transaction(ctx, func(txCtx context.Context, repos Repositories) error {
		for _, m := range missed {
			if err := repos.ScheduleExecutions().SaveScheduleExecution(txCtx, m); err != nil {
				return err
			}
		}
		if err := repos.ScheduleExecutions().SaveScheduleExecution(txCtx, exec); err != nil {
			return err
		}
		return repos.Schedules().SaveSchedule(txCtx, schedule)
	})
	if err != nil {
		return fmt.Errorf("lebro: scheduler: persist schedule %q: %w", schedule.ID, err)
	}
	result.Missed += len(missed)
	result.Executions = append(result.Executions, missed...)
	result.Executions = append(result.Executions, exec)
	return nil
}

// newExecution builds an execution record with a globally unique, durable ID
// derived from the schedule, the scheduled occurrence, and the status. This is
// stable across process restarts (unlike a per-scheduler sequential source,
// which would repeat IDs after a restart and collide with persisted history)
// and unique because a schedule has at most one execution of a given status at
// a given occurrence instant.
func (s *Scheduler) newExecution(scheduleID ScheduleID, runID RunID, status ScheduleExecStatus, scheduledFor, startedAt time.Time, finishedAt *time.Time, errMsg string) ScheduleExecutionRecord {
	id := fmt.Sprintf("%s-%d-%s", scheduleID, scheduledFor.UTC().UnixNano(), status)
	return ScheduleExecutionRecord{
		ID:           ScheduleExecutionID(id),
		ScheduleID:   scheduleID,
		RunID:        runID,
		Status:       status,
		ScheduledFor: scheduledFor.UTC(),
		StartedAt:    startedAt.UTC(),
		FinishedAt:   finishedAt,
		Error:        errMsg,
	}
}

// claimInflight atomically marks the schedule in flight and reports whether the
// claim succeeded. It fails (returns false) when the schedule is already in
// flight, so the check-and-mark that ConcurrencySkip relies on is a single
// atomic operation even when two ticks race on the same schedule.
func (s *Scheduler) claimInflight(id ScheduleID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.inflight[id]; ok {
		return false
	}
	s.inflight[id] = struct{}{}
	return true
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
	// When the loop exits — including because the parent context passed to Start
	// was cancelled without a Stop call — clear the lifecycle state so the
	// scheduler can be started again. The identity check ensures a loop that is
	// being torn down does not clobber the state of a newer loop that a
	// concurrent Start already installed.
	defer func() {
		s.startMu.Lock()
		if s.done == done {
			s.cancel = nil
			s.done = nil
		}
		s.startMu.Unlock()
		close(done)
	}()
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
