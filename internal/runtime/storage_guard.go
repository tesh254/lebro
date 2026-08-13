package runtime

import (
	"context"
	"errors"
)

// PolicyStore wraps a Store and authorizes every repository operation against a
// Policy before delegating to the underlying store. The caller Identity is read
// from the operation context (see WithIdentity), so a single wrapped store
// enforces per-caller access without changing any repository call site.
//
// Authorization is applied at method granularity: each read requests
// ActionStorageRead and each write requests ActionStorageWrite against a
// resource identifying the record's kind and ID. A denied operation returns a
// *PolicyDenial and the underlying store is never touched. Migrate is
// infrastructure, not a per-caller operation, so it is delegated without a
// policy check.
//
// A nil Policy authorizes nothing away: PolicyStore then behaves exactly like
// the store it wraps, so wrapping is safe even before a policy is chosen. The
// zero value is not usable; construct one with NewPolicyStore.
type PolicyStore struct {
	store  Store
	policy Policy
}

var (
	_ Store        = (*PolicyStore)(nil)
	_ Repositories = (*PolicyStore)(nil)
)

// NewPolicyStore returns a Store that authorizes every repository operation
// against policy before delegating to store. A nil policy leaves the store's
// behavior unchanged. It returns an error when store is nil.
func NewPolicyStore(store Store, policy Policy) (*PolicyStore, error) {
	if store == nil || isNilInterface(store) {
		return nil, errors.New("lebro: policy store requires a store")
	}
	return &PolicyStore{store: store, policy: policy}, nil
}

// Migrate delegates to the underlying store. Schema migration is an
// infrastructure concern rather than a per-caller operation, so it is not
// policy-checked.
func (s *PolicyStore) Migrate(ctx context.Context) error {
	return s.store.Migrate(ctx)
}

// Transaction runs fn against policy-guarded, transaction-scoped repositories.
// The repositories handed to fn enforce the same policy as the top-level store,
// so a callback cannot bypass authorization by reaching the raw repositories.
func (s *PolicyStore) Transaction(ctx context.Context, fn func(context.Context, Repositories) error) error {
	return s.store.Transaction(ctx, func(txCtx context.Context, repos Repositories) error {
		return fn(txCtx, &guardedRepositories{repos: repos, policy: s.policy})
	})
}

// Threads returns a policy-guarded thread repository.
func (s *PolicyStore) Threads() ThreadRepository {
	return &guardedThreadRepository{inner: s.store.Threads(), policy: s.policy}
}

// Messages returns a policy-guarded message repository.
func (s *PolicyStore) Messages() MessageRepository {
	return &guardedMessageRepository{inner: s.store.Messages(), policy: s.policy}
}

// WorkflowRuns returns a policy-guarded workflow-run repository.
func (s *PolicyStore) WorkflowRuns() WorkflowRunRepository {
	return &guardedWorkflowRunRepository{inner: s.store.WorkflowRuns(), policy: s.policy}
}

// WorkflowSnapshots returns a policy-guarded workflow-snapshot repository.
func (s *PolicyStore) WorkflowSnapshots() WorkflowSnapshotRepository {
	return &guardedWorkflowSnapshotRepository{inner: s.store.WorkflowSnapshots(), policy: s.policy}
}

// Schedules returns a policy-guarded schedule repository.
func (s *PolicyStore) Schedules() ScheduleRepository {
	return &guardedScheduleRepository{inner: s.store.Schedules(), policy: s.policy}
}

// ScheduleExecutions returns a policy-guarded schedule-execution repository.
func (s *PolicyStore) ScheduleExecutions() ScheduleExecutionRepository {
	return &guardedScheduleExecutionRepository{inner: s.store.ScheduleExecutions(), policy: s.policy}
}
func (s *PolicyStore) WorkingMemory() WorkingMemoryRepository {
	return &guardedWorkingMemoryRepository{inner: s.store.WorkingMemory(), policy: s.policy}
}

// guardedRepositories applies the store's policy to a transaction-scoped set of
// repositories.
type guardedRepositories struct {
	repos  Repositories
	policy Policy
}

func (r *guardedRepositories) Threads() ThreadRepository {
	return &guardedThreadRepository{inner: r.repos.Threads(), policy: r.policy}
}

func (r *guardedRepositories) Messages() MessageRepository {
	return &guardedMessageRepository{inner: r.repos.Messages(), policy: r.policy}
}

func (r *guardedRepositories) WorkflowRuns() WorkflowRunRepository {
	return &guardedWorkflowRunRepository{inner: r.repos.WorkflowRuns(), policy: r.policy}
}

func (r *guardedRepositories) WorkflowSnapshots() WorkflowSnapshotRepository {
	return &guardedWorkflowSnapshotRepository{inner: r.repos.WorkflowSnapshots(), policy: r.policy}
}

func (r *guardedRepositories) Schedules() ScheduleRepository {
	return &guardedScheduleRepository{inner: r.repos.Schedules(), policy: r.policy}
}

func (r *guardedRepositories) ScheduleExecutions() ScheduleExecutionRepository {
	return &guardedScheduleExecutionRepository{inner: r.repos.ScheduleExecutions(), policy: r.policy}
}
func (r *guardedRepositories) WorkingMemory() WorkingMemoryRepository {
	return &guardedWorkingMemoryRepository{inner: r.repos.WorkingMemory(), policy: r.policy}
}

type guardedWorkingMemoryRepository struct {
	inner  WorkingMemoryRepository
	policy Policy
}

func (r *guardedWorkingMemoryRepository) UpsertWorkingMemoryFact(ctx context.Context, v WorkingMemoryFact, e int64) (WorkingMemoryFact, error) {
	if err := authorize(ctx, r.policy, ActionStorageWrite, Resource{Kind: ResourceKindWorkingMemory, ID: v.Key, Tenant: v.Namespace}); err != nil {
		return WorkingMemoryFact{}, err
	}
	return r.inner.UpsertWorkingMemoryFact(ctx, v, e)
}
func (r *guardedWorkingMemoryRepository) GetWorkingMemoryFact(ctx context.Context, s WorkingMemoryScope, k string) (WorkingMemoryFact, error) {
	if err := authorize(ctx, r.policy, ActionStorageRead, Resource{Kind: ResourceKindWorkingMemory, ID: k, Tenant: s.Namespace}); err != nil {
		return WorkingMemoryFact{}, err
	}
	return r.inner.GetWorkingMemoryFact(ctx, s, k)
}
func (r *guardedWorkingMemoryRepository) ListWorkingMemoryFacts(ctx context.Context, s WorkingMemoryScope, p PageRequest) (Page[WorkingMemoryFact], error) {
	if err := authorize(ctx, r.policy, ActionStorageRead, Resource{Kind: ResourceKindWorkingMemory, ID: s.OwnerID, Tenant: s.Namespace}); err != nil {
		return Page[WorkingMemoryFact]{}, err
	}
	return r.inner.ListWorkingMemoryFacts(ctx, s, p)
}
func (r *guardedWorkingMemoryRepository) DeleteWorkingMemoryFact(ctx context.Context, s WorkingMemoryScope, k string, e int64) error {
	if err := authorize(ctx, r.policy, ActionStorageWrite, Resource{Kind: ResourceKindWorkingMemory, ID: k, Tenant: s.Namespace}); err != nil {
		return err
	}
	return r.inner.DeleteWorkingMemoryFact(ctx, s, k, e)
}
func (r *guardedWorkingMemoryRepository) ClearWorkingMemory(ctx context.Context, s WorkingMemoryScope) error {
	if err := authorize(ctx, r.policy, ActionStorageWrite, Resource{Kind: ResourceKindWorkingMemory, ID: s.OwnerID, Tenant: s.Namespace}); err != nil {
		return err
	}
	return r.inner.ClearWorkingMemory(ctx, s)
}

type guardedThreadRepository struct {
	inner  ThreadRepository
	policy Policy
}

func (r *guardedThreadRepository) CreateThread(ctx context.Context, record ThreadRecord) error {
	// The Tenant is intentionally not taken from record.Namespace: it is
	// caller-supplied and unverified, so asserting it as the authorized scope
	// would let a caller pass the check with a namespace it does not own.
	// Tenant-scoped enforcement for a known ID needs a scope-aware repository
	// read, which is deferred; the guard authorizes the operation by ID only.
	if err := authorize(ctx, r.policy, ActionStorageWrite, Resource{Kind: ResourceKindThread, ID: string(record.ID)}); err != nil {
		return err
	}
	return r.inner.CreateThread(ctx, record)
}

func (r *guardedThreadRepository) GetThread(ctx context.Context, id ThreadID) (ThreadRecord, error) {
	if err := authorize(ctx, r.policy, ActionStorageRead, Resource{Kind: ResourceKindThread, ID: string(id)}); err != nil {
		return ThreadRecord{}, err
	}
	return r.inner.GetThread(ctx, id)
}

func (r *guardedThreadRepository) UpdateThread(ctx context.Context, record ThreadRecord) error {
	// Authorize by ID only, not by record.Namespace: the namespace on the
	// incoming record is caller-supplied and may differ from the stored thread's
	// namespace, so trusting it would let a caller who knows another tenant's
	// thread ID pass this check with its own namespace and then overwrite the
	// stored scope. Verifying against the existing scope needs a scope-aware
	// repository read, which is deferred.
	if err := authorize(ctx, r.policy, ActionStorageWrite, Resource{Kind: ResourceKindThread, ID: string(record.ID)}); err != nil {
		return err
	}
	return r.inner.UpdateThread(ctx, record)
}

type guardedMessageRepository struct {
	inner  MessageRepository
	policy Policy
}

func (r *guardedMessageRepository) AppendMessages(ctx context.Context, records []MessageRecord) error {
	// A batch may target more than one thread. Authorize every distinct thread
	// it touches, in first-seen order, so a policy that permits one thread but
	// denies another in the same batch is not bypassed by only checking the
	// first record.
	seen := make(map[ThreadID]struct{}, len(records))
	for _, record := range records {
		if _, ok := seen[record.ThreadID]; ok {
			continue
		}
		seen[record.ThreadID] = struct{}{}
		if err := authorize(ctx, r.policy, ActionStorageWrite, Resource{Kind: ResourceKindMessage, ID: string(record.ThreadID)}); err != nil {
			return err
		}
	}
	return r.inner.AppendMessages(ctx, records)
}

func (r *guardedMessageRepository) ListMessages(ctx context.Context, id ThreadID, page PageRequest) (Page[MessageRecord], error) {
	if err := authorize(ctx, r.policy, ActionStorageRead, Resource{Kind: ResourceKindMessage, ID: string(id)}); err != nil {
		return Page[MessageRecord]{}, err
	}
	return r.inner.ListMessages(ctx, id, page)
}

type guardedWorkflowRunRepository struct {
	inner  WorkflowRunRepository
	policy Policy
}

func (r *guardedWorkflowRunRepository) SaveWorkflowRun(ctx context.Context, record WorkflowRunRecord) error {
	if err := authorize(ctx, r.policy, ActionStorageWrite, Resource{Kind: ResourceKindWorkflowRun, ID: string(record.ID)}); err != nil {
		return err
	}
	return r.inner.SaveWorkflowRun(ctx, record)
}

func (r *guardedWorkflowRunRepository) GetWorkflowRun(ctx context.Context, id RunID) (WorkflowRunRecord, error) {
	if err := authorize(ctx, r.policy, ActionStorageRead, Resource{Kind: ResourceKindWorkflowRun, ID: string(id)}); err != nil {
		return WorkflowRunRecord{}, err
	}
	return r.inner.GetWorkflowRun(ctx, id)
}

func (r *guardedWorkflowRunRepository) ListWorkflowRuns(ctx context.Context, filter WorkflowRunFilter, page PageRequest) (Page[WorkflowRunRecord], error) {
	if err := authorize(ctx, r.policy, ActionStorageRead, Resource{Kind: ResourceKindWorkflowRun, ID: string(filter.WorkflowID)}); err != nil {
		return Page[WorkflowRunRecord]{}, err
	}
	return r.inner.ListWorkflowRuns(ctx, filter, page)
}

type guardedWorkflowSnapshotRepository struct {
	inner  WorkflowSnapshotRepository
	policy Policy
}

func (r *guardedWorkflowSnapshotRepository) SaveWorkflowSnapshot(ctx context.Context, record WorkflowSnapshotRecord) error {
	if err := authorize(ctx, r.policy, ActionStorageWrite, Resource{Kind: ResourceKindWorkflowSnapshot, ID: string(record.RunID)}); err != nil {
		return err
	}
	return r.inner.SaveWorkflowSnapshot(ctx, record)
}

func (r *guardedWorkflowSnapshotRepository) ListWorkflowSnapshots(ctx context.Context, id RunID, page PageRequest) (Page[WorkflowSnapshotRecord], error) {
	if err := authorize(ctx, r.policy, ActionStorageRead, Resource{Kind: ResourceKindWorkflowSnapshot, ID: string(id)}); err != nil {
		return Page[WorkflowSnapshotRecord]{}, err
	}
	return r.inner.ListWorkflowSnapshots(ctx, id, page)
}

type guardedScheduleRepository struct {
	inner  ScheduleRepository
	policy Policy
}

func (r *guardedScheduleRepository) SaveSchedule(ctx context.Context, record ScheduleRecord) error {
	if err := authorize(ctx, r.policy, ActionStorageWrite, Resource{Kind: ResourceKindSchedule, ID: string(record.ID)}); err != nil {
		return err
	}
	return r.inner.SaveSchedule(ctx, record)
}

func (r *guardedScheduleRepository) GetSchedule(ctx context.Context, id ScheduleID) (ScheduleRecord, error) {
	if err := authorize(ctx, r.policy, ActionStorageRead, Resource{Kind: ResourceKindSchedule, ID: string(id)}); err != nil {
		return ScheduleRecord{}, err
	}
	return r.inner.GetSchedule(ctx, id)
}

func (r *guardedScheduleRepository) ListSchedules(ctx context.Context, filter ScheduleFilter, page PageRequest) (Page[ScheduleRecord], error) {
	if err := authorize(ctx, r.policy, ActionStorageRead, Resource{Kind: ResourceKindSchedule, ID: string(filter.WorkflowID)}); err != nil {
		return Page[ScheduleRecord]{}, err
	}
	return r.inner.ListSchedules(ctx, filter, page)
}

func (r *guardedScheduleRepository) DeleteSchedule(ctx context.Context, id ScheduleID) error {
	if err := authorize(ctx, r.policy, ActionStorageWrite, Resource{Kind: ResourceKindSchedule, ID: string(id)}); err != nil {
		return err
	}
	return r.inner.DeleteSchedule(ctx, id)
}

type guardedScheduleExecutionRepository struct {
	inner  ScheduleExecutionRepository
	policy Policy
}

func (r *guardedScheduleExecutionRepository) SaveScheduleExecution(ctx context.Context, record ScheduleExecutionRecord) error {
	if err := authorize(ctx, r.policy, ActionStorageWrite, Resource{Kind: ResourceKindSchedule, ID: string(record.ScheduleID)}); err != nil {
		return err
	}
	return r.inner.SaveScheduleExecution(ctx, record)
}

func (r *guardedScheduleExecutionRepository) ListScheduleExecutions(ctx context.Context, id ScheduleID, page PageRequest) (Page[ScheduleExecutionRecord], error) {
	if err := authorize(ctx, r.policy, ActionStorageRead, Resource{Kind: ResourceKindSchedule, ID: string(id)}); err != nil {
		return Page[ScheduleExecutionRecord]{}, err
	}
	return r.inner.ListScheduleExecutions(ctx, id, page)
}
