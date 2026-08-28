package runtime

import (
	"context"
)

// runtimeStoreBridge adapts a capability-based RuntimeStore to the internal
// Store interface so existing consumers (agent runs, the run journal, and
// other repository call sites) work unchanged. Repository accessors for
// capabilities the adapter does not advertise return stubs that fail with a
// *StoreCapabilityError instead of a nil-interface panic, and feature code is
// expected to gate on capabilities before reaching them.
//
// Migrate is a no-op: custom adapters own their backing schema and require no
// Lebro migration or predefined table names. Transaction delegates to the
// adapter's TransactionalStore when the transactions capability is advertised;
// otherwise it runs the callback with sequential-write fallback semantics
// documented on TransactionalStore.
//
// The observability methods live on observableRuntimeStoreBridge so the
// ObservabilityRepositories opt-in stays exactly as for Store adapters: a
// RuntimeStore without the observability capability does not satisfy the
// interface and its observability records are simply not persisted.
type runtimeStoreBridge struct {
	rs   RuntimeStore
	tx   TransactionalStore
	caps StoreCapabilities
}

var (
	_ Store        = (*runtimeStoreBridge)(nil)
	_ Repositories = (*runtimeReposBridge)(nil)
)

// bridgeRuntimeStore validates the adapter contract and wraps it for internal
// use. It fails with a *StoreCapabilityError when the advertisement and the
// implemented interfaces disagree.
func bridgeRuntimeStore(rs RuntimeStore) (Store, error) {
	caps, err := validateRuntimeStore(rs)
	if err != nil {
		return nil, err
	}
	bridge := &runtimeStoreBridge{rs: rs, caps: caps}
	if caps.Has(StoreCapabilityTransactions) {
		bridge.tx = rs.(TransactionalStore)
	}
	if caps.Has(StoreCapabilityObservability) {
		return &observableRuntimeStoreBridge{bridge}, nil
	}
	return bridge, nil
}

// storeCapabilitiesOf resolves the capability set of any configured store.
// RuntimeStore implementations are validated through the contract; plain
// Store implementations support every base capability, with observability
// still governed by the existing ObservabilityRepositories opt-in.
func storeCapabilitiesOf(store Store) (StoreCapabilities, error) {
	if store == nil || isNilInterface(store) {
		return StoreCapabilities{}, nil
	}
	if rs, ok := store.(RuntimeStore); ok {
		return validateRuntimeStore(rs)
	}
	return AllStoreCapabilities(), nil
}

func (b *runtimeStoreBridge) Capabilities() StoreCapabilities { return b.caps }

func (b *runtimeStoreBridge) Threads() ThreadRepository { return bridgeThreads(b.rs) }
func (b *runtimeStoreBridge) Messages() MessageRepository {
	return bridgeMessages(b.rs)
}
func (b *runtimeStoreBridge) WorkingMemory() WorkingMemoryRepository {
	return bridgeWorkingMemory(b.rs)
}
func (b *runtimeStoreBridge) WorkflowRuns() WorkflowRunRepository {
	return bridgeWorkflowRuns(b.rs)
}
func (b *runtimeStoreBridge) WorkflowSnapshots() WorkflowSnapshotRepository {
	return bridgeWorkflowSnapshots(b.rs)
}
func (b *runtimeStoreBridge) Schedules() ScheduleRepository {
	return bridgeSchedules(b.rs)
}
func (b *runtimeStoreBridge) ScheduleExecutions() ScheduleExecutionRepository {
	return bridgeScheduleExecutions(b.rs)
}

// Migrate is a no-op for custom adapters: they own their backing schema.
func (b *runtimeStoreBridge) Migrate(context.Context) error { return nil }

// Transaction runs fn inside the adapter's transaction when it supports one,
// and sequentially otherwise. In the fallback, writes apply in call order and
// already-written records persist when a later write fails; no rollback and no
// ErrConflict occurs.
func (b *runtimeStoreBridge) Transaction(ctx context.Context, fn func(context.Context, Repositories) error) error {
	if b.tx != nil {
		return b.tx.InTransaction(ctx, func(ctx context.Context, view RuntimeStore) error {
			return fn(ctx, b.repositories(view))
		})
	}
	return fn(ctx, b.repositories(b.rs))
}

// repositories builds the Repositories view over a RuntimeStore (the store
// itself, or its transaction-scoped view) with observability methods present
// only when the capability is advertised.
func (b *runtimeStoreBridge) repositories(view RuntimeStore) Repositories {
	inner := &runtimeReposBridge{rs: view, caps: b.caps}
	if b.caps.Has(StoreCapabilityObservability) {
		return &observableRuntimeReposBridge{inner}
	}
	return inner
}

// observableRuntimeStoreBridge adds the observability capability to a bridge.
// Wrapping keeps the type assertion in writeObservability meaningful: the base
// bridge intentionally does not satisfy ObservabilityRepositories.
type observableRuntimeStoreBridge struct {
	*runtimeStoreBridge
}

func (b *observableRuntimeStoreBridge) RunEvents() RunEventRepository {
	return bridgeRunEvents(b.rs)
}
func (b *observableRuntimeStoreBridge) ModelAttempts() ModelAttemptRepository {
	return bridgeModelAttempts(b.rs)
}
func (b *observableRuntimeStoreBridge) ToolExecutions() ToolExecutionRepository {
	return bridgeToolExecutions(b.rs)
}

// runtimeReposBridge is the Repositories view over a RuntimeStore, used both
// for direct access and inside Transaction callbacks.
type runtimeReposBridge struct {
	rs   RuntimeStore
	caps StoreCapabilities
}

func (r *runtimeReposBridge) Threads() ThreadRepository { return bridgeThreads(r.rs) }
func (r *runtimeReposBridge) Messages() MessageRepository {
	return bridgeMessages(r.rs)
}
func (r *runtimeReposBridge) WorkingMemory() WorkingMemoryRepository {
	return bridgeWorkingMemory(r.rs)
}
func (r *runtimeReposBridge) WorkflowRuns() WorkflowRunRepository {
	return bridgeWorkflowRuns(r.rs)
}
func (r *runtimeReposBridge) WorkflowSnapshots() WorkflowSnapshotRepository {
	return bridgeWorkflowSnapshots(r.rs)
}
func (r *runtimeReposBridge) Schedules() ScheduleRepository {
	return bridgeSchedules(r.rs)
}
func (r *runtimeReposBridge) ScheduleExecutions() ScheduleExecutionRepository {
	return bridgeScheduleExecutions(r.rs)
}

// observableRuntimeReposBridge adds the observability repositories to a
// transaction-scoped Repositories view.
type observableRuntimeReposBridge struct {
	*runtimeReposBridge
}

func (r *observableRuntimeReposBridge) RunEvents() RunEventRepository {
	return bridgeRunEvents(r.rs)
}
func (r *observableRuntimeReposBridge) ModelAttempts() ModelAttemptRepository {
	return bridgeModelAttempts(r.rs)
}
func (r *observableRuntimeReposBridge) ToolExecutions() ToolExecutionRepository {
	return bridgeToolExecutions(r.rs)
}

// bridge accessors resolve a capability repository from a RuntimeStore or
// return a stub that fails with a *StoreCapabilityError. The stubs exist so an
// accidental reach-through fails typed instead of panicking on a nil
// interface.

func bridgeThreads(rs RuntimeStore) ThreadRepository {
	if s, ok := rs.(TranscriptStore); ok {
		return s.Threads()
	}
	return uncapableThreads{}
}

func bridgeMessages(rs RuntimeStore) MessageRepository {
	if s, ok := rs.(TranscriptStore); ok {
		return s.Messages()
	}
	return uncapableMessages{}
}

func bridgeWorkingMemory(rs RuntimeStore) WorkingMemoryRepository {
	if s, ok := rs.(WorkingMemoryStore); ok {
		return s.WorkingMemory()
	}
	return uncapableWorkingMemory{}
}

func bridgeWorkflowRuns(rs RuntimeStore) WorkflowRunRepository {
	if s, ok := rs.(WorkflowStateStore); ok {
		return s.WorkflowRuns()
	}
	return uncapableWorkflowRuns{}
}

func bridgeWorkflowSnapshots(rs RuntimeStore) WorkflowSnapshotRepository {
	if s, ok := rs.(WorkflowStateStore); ok {
		return s.WorkflowSnapshots()
	}
	return uncapableWorkflowSnapshots{}
}

func bridgeSchedules(rs RuntimeStore) ScheduleRepository {
	if s, ok := rs.(ScheduleStore); ok {
		return s.Schedules()
	}
	return uncapableSchedules{}
}

func bridgeScheduleExecutions(rs RuntimeStore) ScheduleExecutionRepository {
	if s, ok := rs.(ScheduleStore); ok {
		return s.ScheduleExecutions()
	}
	return uncapableScheduleExecutions{}
}

func bridgeRunEvents(rs RuntimeStore) RunEventRepository {
	if s, ok := rs.(ObservabilityStore); ok {
		return s.RunEvents()
	}
	return uncapableRunEvents{}
}

func bridgeModelAttempts(rs RuntimeStore) ModelAttemptRepository {
	if s, ok := rs.(ObservabilityStore); ok {
		return s.ModelAttempts()
	}
	return uncapableModelAttempts{}
}

func bridgeToolExecutions(rs RuntimeStore) ToolExecutionRepository {
	if s, ok := rs.(ObservabilityStore); ok {
		return s.ToolExecutions()
	}
	return uncapableToolExecutions{}
}

// capabilityFailure builds the error every uncapable repository method
// returns. Reach-through is a programming error the setup validation should
// have prevented; the typed error keeps the failure diagnosable.
func capabilityFailure(capability StoreCapability) error {
	return &StoreCapabilityError{
		Capability: capability,
		Feature:    "runtime storage",
		Reason:     "the attached storage adapter does not support it",
	}
}

type uncapableThreads struct{}

func (uncapableThreads) CreateThread(context.Context, ThreadRecord) error {
	return capabilityFailure(StoreCapabilityTranscript)
}
func (uncapableThreads) GetThread(context.Context, ThreadID) (ThreadRecord, error) {
	return ThreadRecord{}, capabilityFailure(StoreCapabilityTranscript)
}
func (uncapableThreads) UpdateThread(context.Context, ThreadRecord) error {
	return capabilityFailure(StoreCapabilityTranscript)
}

type uncapableMessages struct{}

func (uncapableMessages) AppendMessages(context.Context, []MessageRecord) error {
	return capabilityFailure(StoreCapabilityTranscript)
}
func (uncapableMessages) UpdateMessages(context.Context, []MessageRecord) error {
	return capabilityFailure(StoreCapabilityTranscript)
}
func (uncapableMessages) DeleteMessages(context.Context, ThreadID, []string) error {
	return capabilityFailure(StoreCapabilityTranscript)
}
func (uncapableMessages) ListMessages(context.Context, ThreadID, PageRequest) (Page[MessageRecord], error) {
	return Page[MessageRecord]{}, capabilityFailure(StoreCapabilityTranscript)
}

type uncapableWorkingMemory struct{}

func (uncapableWorkingMemory) UpsertWorkingMemoryFact(context.Context, WorkingMemoryFact, int64) (WorkingMemoryFact, error) {
	return WorkingMemoryFact{}, capabilityFailure(StoreCapabilityWorkingMemory)
}
func (uncapableWorkingMemory) GetWorkingMemoryFact(context.Context, WorkingMemoryScope, string) (WorkingMemoryFact, error) {
	return WorkingMemoryFact{}, capabilityFailure(StoreCapabilityWorkingMemory)
}
func (uncapableWorkingMemory) ListWorkingMemoryFacts(context.Context, WorkingMemoryScope, PageRequest) (Page[WorkingMemoryFact], error) {
	return Page[WorkingMemoryFact]{}, capabilityFailure(StoreCapabilityWorkingMemory)
}
func (uncapableWorkingMemory) DeleteWorkingMemoryFact(context.Context, WorkingMemoryScope, string, int64) error {
	return capabilityFailure(StoreCapabilityWorkingMemory)
}
func (uncapableWorkingMemory) ClearWorkingMemory(context.Context, WorkingMemoryScope) error {
	return capabilityFailure(StoreCapabilityWorkingMemory)
}

type uncapableWorkflowRuns struct{}

func (uncapableWorkflowRuns) SaveWorkflowRun(context.Context, WorkflowRunRecord) error {
	return capabilityFailure(StoreCapabilityWorkflowState)
}
func (uncapableWorkflowRuns) GetWorkflowRun(context.Context, RunID) (WorkflowRunRecord, error) {
	return WorkflowRunRecord{}, capabilityFailure(StoreCapabilityWorkflowState)
}
func (uncapableWorkflowRuns) ListWorkflowRuns(context.Context, WorkflowRunFilter, PageRequest) (Page[WorkflowRunRecord], error) {
	return Page[WorkflowRunRecord]{}, capabilityFailure(StoreCapabilityWorkflowState)
}

type uncapableWorkflowSnapshots struct{}

func (uncapableWorkflowSnapshots) SaveWorkflowSnapshot(context.Context, WorkflowSnapshotRecord) error {
	return capabilityFailure(StoreCapabilityWorkflowState)
}
func (uncapableWorkflowSnapshots) ListWorkflowSnapshots(context.Context, RunID, PageRequest) (Page[WorkflowSnapshotRecord], error) {
	return Page[WorkflowSnapshotRecord]{}, capabilityFailure(StoreCapabilityWorkflowState)
}

type uncapableSchedules struct{}

func (uncapableSchedules) SaveSchedule(context.Context, ScheduleRecord) error {
	return capabilityFailure(StoreCapabilitySchedules)
}
func (uncapableSchedules) GetSchedule(context.Context, ScheduleID) (ScheduleRecord, error) {
	return ScheduleRecord{}, capabilityFailure(StoreCapabilitySchedules)
}
func (uncapableSchedules) ListSchedules(context.Context, ScheduleFilter, PageRequest) (Page[ScheduleRecord], error) {
	return Page[ScheduleRecord]{}, capabilityFailure(StoreCapabilitySchedules)
}
func (uncapableSchedules) DeleteSchedule(context.Context, ScheduleID) error {
	return capabilityFailure(StoreCapabilitySchedules)
}

type uncapableScheduleExecutions struct{}

func (uncapableScheduleExecutions) SaveScheduleExecution(context.Context, ScheduleExecutionRecord) error {
	return capabilityFailure(StoreCapabilitySchedules)
}
func (uncapableScheduleExecutions) ListScheduleExecutions(context.Context, ScheduleID, PageRequest) (Page[ScheduleExecutionRecord], error) {
	return Page[ScheduleExecutionRecord]{}, capabilityFailure(StoreCapabilitySchedules)
}

type uncapableRunEvents struct{}

func (uncapableRunEvents) AppendRunEvents(context.Context, []RunEventRecord) error {
	return capabilityFailure(StoreCapabilityObservability)
}
func (uncapableRunEvents) ListRunEvents(context.Context, RunEventFilter, PageRequest) (Page[RunEventRecord], error) {
	return Page[RunEventRecord]{}, capabilityFailure(StoreCapabilityObservability)
}

type uncapableModelAttempts struct{}

func (uncapableModelAttempts) SaveModelAttempts(context.Context, []ModelAttemptRecord) error {
	return capabilityFailure(StoreCapabilityObservability)
}
func (uncapableModelAttempts) ListModelAttempts(context.Context, ModelAttemptFilter, PageRequest) (Page[ModelAttemptRecord], error) {
	return Page[ModelAttemptRecord]{}, capabilityFailure(StoreCapabilityObservability)
}

type uncapableToolExecutions struct{}

func (uncapableToolExecutions) SaveToolExecutions(context.Context, []ToolExecutionRecord) error {
	return capabilityFailure(StoreCapabilityObservability)
}
func (uncapableToolExecutions) ListToolExecutions(context.Context, ToolExecutionFilter, PageRequest) (Page[ToolExecutionRecord], error) {
	return Page[ToolExecutionRecord]{}, capabilityFailure(StoreCapabilityObservability)
}
