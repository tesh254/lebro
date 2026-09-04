package runtime

import (
	"context"
	"errors"
	"fmt"
)

func policyScope(ctx context.Context, claimed RuntimeScope) (RuntimeScope, error) {
	if verified, ok := RuntimeScopeFromContext(ctx); ok {
		if verified != claimed {
			return RuntimeScope{}, fmt.Errorf("lebro: record scope does not match verified runtime scope")
		}
		return verified, nil
	}
	return claimed, nil
}

func policyFilterScope(ctx context.Context, claimed RuntimeScope) (RuntimeScope, error) {
	if verified, ok := RuntimeScopeFromContext(ctx); ok {
		if (claimed.Namespace != "" && claimed.Namespace != verified.Namespace) || (claimed.OwnerID != "" && claimed.OwnerID != verified.OwnerID) {
			return RuntimeScope{}, fmt.Errorf("lebro: filter scope does not match verified runtime scope")
		}
		return verified, nil
	}
	return claimed, nil
}

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
	return &guardedMessageRepository{inner: s.store.Messages(), threads: s.store.Threads(), policy: s.policy}
}

// WorkflowRuns returns a policy-guarded workflow-run repository.
func (s *PolicyStore) WorkflowRuns() WorkflowRunRepository {
	return &guardedWorkflowRunRepository{inner: s.store.WorkflowRuns(), policy: s.policy}
}

// WorkflowSnapshots returns a policy-guarded workflow-snapshot repository.
func (s *PolicyStore) WorkflowSnapshots() WorkflowSnapshotRepository {
	return &guardedWorkflowSnapshotRepository{inner: s.store.WorkflowSnapshots(), runs: s.store.WorkflowRuns(), policy: s.policy}
}

// Schedules returns a policy-guarded schedule repository.
func (s *PolicyStore) Schedules() ScheduleRepository {
	return &guardedScheduleRepository{inner: s.store.Schedules(), policy: s.policy}
}

// ScheduleExecutions returns a policy-guarded schedule-execution repository.
func (s *PolicyStore) ScheduleExecutions() ScheduleExecutionRepository {
	return &guardedScheduleExecutionRepository{inner: s.store.ScheduleExecutions(), schedules: s.store.Schedules(), policy: s.policy}
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
	return &guardedMessageRepository{inner: r.repos.Messages(), threads: r.repos.Threads(), policy: r.policy}
}

func (r *guardedRepositories) WorkflowRuns() WorkflowRunRepository {
	return &guardedWorkflowRunRepository{inner: r.repos.WorkflowRuns(), policy: r.policy}
}

func (r *guardedRepositories) WorkflowSnapshots() WorkflowSnapshotRepository {
	return &guardedWorkflowSnapshotRepository{inner: r.repos.WorkflowSnapshots(), runs: r.repos.WorkflowRuns(), policy: r.policy}
}

func (r *guardedRepositories) Schedules() ScheduleRepository {
	return &guardedScheduleRepository{inner: r.repos.Schedules(), policy: r.policy}
}

func (r *guardedRepositories) ScheduleExecutions() ScheduleExecutionRepository {
	return &guardedScheduleExecutionRepository{inner: r.repos.ScheduleExecutions(), schedules: r.repos.Schedules(), policy: r.policy}
}
func (r *guardedRepositories) WorkingMemory() WorkingMemoryRepository {
	return &guardedWorkingMemoryRepository{inner: r.repos.WorkingMemory(), policy: r.policy}
}

type guardedWorkingMemoryRepository struct {
	inner  WorkingMemoryRepository
	policy Policy
}

func (r *guardedWorkingMemoryRepository) UpsertWorkingMemoryFact(ctx context.Context, v WorkingMemoryFact, e int64) (WorkingMemoryFact, error) {
	scope, err := policyScope(ctx, RuntimeScope{Namespace: v.Namespace, OwnerID: v.OwnerID})
	if err != nil {
		return WorkingMemoryFact{}, err
	}
	if err := authorize(ctx, r.policy, ActionStorageWrite, Resource{Kind: ResourceKindWorkingMemory, ID: v.Key, Tenant: scope.Namespace, OwnerID: scope.OwnerID}); err != nil {
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
	scope, err := policyScope(ctx, RuntimeScope{Namespace: record.Namespace, OwnerID: record.OwnerID})
	if err != nil {
		return err
	}
	if err := authorize(ctx, r.policy, ActionStorageWrite, Resource{Kind: ResourceKindThread, ID: string(record.ID), Tenant: scope.Namespace, OwnerID: scope.OwnerID}); err != nil {
		return err
	}
	return r.inner.CreateThread(ctx, record)
}

func (r *guardedThreadRepository) GetThread(ctx context.Context, id ThreadID) (ThreadRecord, error) {
	if _, verified := RuntimeScopeFromContext(ctx); !verified {
		// Preserve the legacy policy behavior for callers that have not opted
		// into verified scopes: denial must not reveal whether an ID exists.
		if err := authorize(ctx, r.policy, ActionStorageRead, Resource{Kind: ResourceKindThread, ID: string(id)}); err != nil {
			return ThreadRecord{}, err
		}
		return r.inner.GetThread(ctx, id)
	}
	record, err := r.inner.GetThread(ctx, id)
	if err != nil {
		return ThreadRecord{}, err
	}
	scope, err := policyScope(ctx, RuntimeScope{Namespace: record.Namespace, OwnerID: record.OwnerID})
	if err != nil {
		return ThreadRecord{}, err
	}
	if err := authorize(ctx, r.policy, ActionStorageRead, Resource{Kind: ResourceKindThread, ID: string(id), Tenant: scope.Namespace, OwnerID: scope.OwnerID}); err != nil {
		return ThreadRecord{}, err
	}
	return record, nil
}

func (r *guardedThreadRepository) UpdateThread(ctx context.Context, record ThreadRecord) error {
	stored, err := r.inner.GetThread(ctx, record.ID)
	if err != nil {
		return err
	}
	if stored.Namespace != record.Namespace || stored.OwnerID != record.OwnerID {
		return errors.New("lebro: thread scope is immutable")
	}
	scope, err := policyScope(ctx, RuntimeScope{Namespace: stored.Namespace, OwnerID: stored.OwnerID})
	if err != nil {
		return err
	}
	if err := authorize(ctx, r.policy, ActionStorageWrite, Resource{Kind: ResourceKindThread, ID: string(record.ID), Tenant: scope.Namespace, OwnerID: scope.OwnerID}); err != nil {
		return err
	}
	return r.inner.UpdateThread(ctx, record)
}

type guardedMessageRepository struct {
	inner   MessageRepository
	threads ThreadRepository
	policy  Policy
}

func (r *guardedMessageRepository) authorizeThread(ctx context.Context, action Action, id ThreadID) error {
	if _, verified := RuntimeScopeFromContext(ctx); !verified {
		return authorize(ctx, r.policy, action, Resource{Kind: ResourceKindMessage, ID: string(id)})
	}
	thread, err := r.threads.GetThread(ctx, id)
	if err != nil {
		return err
	}
	scope, err := policyScope(ctx, RuntimeScope{Namespace: thread.Namespace, OwnerID: thread.OwnerID})
	if err != nil {
		return err
	}
	return authorize(ctx, r.policy, action, Resource{Kind: ResourceKindMessage, ID: string(id), Tenant: scope.Namespace, OwnerID: scope.OwnerID})
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
		if err := r.authorizeThread(ctx, ActionStorageWrite, record.ThreadID); err != nil {
			return err
		}
	}
	return r.inner.AppendMessages(ctx, records)
}

func (r *guardedMessageRepository) UpdateMessages(ctx context.Context, records []MessageRecord) error {
	seen := make(map[ThreadID]struct{}, len(records))
	for _, record := range records {
		if _, ok := seen[record.ThreadID]; ok {
			continue
		}
		seen[record.ThreadID] = struct{}{}
		if err := r.authorizeThread(ctx, ActionStorageWrite, record.ThreadID); err != nil {
			return err
		}
	}
	return r.inner.UpdateMessages(ctx, records)
}

func (r *guardedMessageRepository) DeleteMessages(ctx context.Context, id ThreadID, ids []string) error {
	if err := r.authorizeThread(ctx, ActionStorageWrite, id); err != nil {
		return err
	}
	return r.inner.DeleteMessages(ctx, id, ids)
}

func (r *guardedMessageRepository) ListMessages(ctx context.Context, id ThreadID, page PageRequest) (Page[MessageRecord], error) {
	if err := r.authorizeThread(ctx, ActionStorageRead, id); err != nil {
		return Page[MessageRecord]{}, err
	}
	return r.inner.ListMessages(ctx, id, page)
}

type guardedWorkflowRunRepository struct {
	inner  WorkflowRunRepository
	policy Policy
}

func (r *guardedWorkflowRunRepository) SaveWorkflowRun(ctx context.Context, record WorkflowRunRecord) error {
	if stored, err := r.inner.GetWorkflowRun(ctx, record.ID); err == nil {
		if stored.Namespace != record.Namespace || stored.OwnerID != record.OwnerID {
			return errors.New("lebro: workflow run scope is immutable")
		}
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	scope, err := policyScope(ctx, RuntimeScope{Namespace: record.Namespace, OwnerID: record.OwnerID})
	if err != nil {
		return err
	}
	if err := authorize(ctx, r.policy, ActionStorageWrite, Resource{Kind: ResourceKindWorkflowRun, ID: string(record.ID), Tenant: scope.Namespace, OwnerID: scope.OwnerID}); err != nil {
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
	scope, err := policyFilterScope(ctx, RuntimeScope{Namespace: filter.Namespace, OwnerID: filter.OwnerID})
	if err != nil {
		return Page[WorkflowRunRecord]{}, err
	}
	filter.Namespace, filter.OwnerID = scope.Namespace, scope.OwnerID
	if err := authorize(ctx, r.policy, ActionStorageRead, Resource{Kind: ResourceKindWorkflowRun, ID: string(filter.WorkflowID), Tenant: scope.Namespace, OwnerID: scope.OwnerID}); err != nil {
		return Page[WorkflowRunRecord]{}, err
	}
	return r.inner.ListWorkflowRuns(ctx, filter, page)
}

type guardedWorkflowSnapshotRepository struct {
	inner  WorkflowSnapshotRepository
	runs   WorkflowRunRepository
	policy Policy
}

func (r *guardedWorkflowSnapshotRepository) authorizeRun(ctx context.Context, action Action, id RunID) error {
	if _, verified := RuntimeScopeFromContext(ctx); !verified {
		return authorize(ctx, r.policy, action, Resource{Kind: ResourceKindWorkflowSnapshot, ID: string(id)})
	}
	run, err := r.runs.GetWorkflowRun(ctx, id)
	if err != nil {
		return err
	}
	scope, err := policyScope(ctx, RuntimeScope{Namespace: run.Namespace, OwnerID: run.OwnerID})
	if err != nil {
		return err
	}
	return authorize(ctx, r.policy, action, Resource{Kind: ResourceKindWorkflowSnapshot, ID: string(id), Tenant: scope.Namespace, OwnerID: scope.OwnerID})
}

func (r *guardedWorkflowSnapshotRepository) SaveWorkflowSnapshot(ctx context.Context, record WorkflowSnapshotRecord) error {
	if err := r.authorizeRun(ctx, ActionStorageWrite, record.RunID); err != nil {
		return err
	}
	return r.inner.SaveWorkflowSnapshot(ctx, record)
}

func (r *guardedWorkflowSnapshotRepository) ListWorkflowSnapshots(ctx context.Context, id RunID, page PageRequest) (Page[WorkflowSnapshotRecord], error) {
	if err := r.authorizeRun(ctx, ActionStorageRead, id); err != nil {
		return Page[WorkflowSnapshotRecord]{}, err
	}
	return r.inner.ListWorkflowSnapshots(ctx, id, page)
}

type guardedScheduleRepository struct {
	inner  ScheduleRepository
	policy Policy
}

func (r *guardedScheduleRepository) SaveSchedule(ctx context.Context, record ScheduleRecord) error {
	if stored, err := r.inner.GetSchedule(ctx, record.ID); err == nil {
		if stored.Namespace != record.Namespace || stored.OwnerID != record.OwnerID {
			return errors.New("lebro: schedule scope is immutable")
		}
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	scope, err := policyScope(ctx, RuntimeScope{Namespace: record.Namespace, OwnerID: record.OwnerID})
	if err != nil {
		return err
	}
	if err := authorize(ctx, r.policy, ActionStorageWrite, Resource{Kind: ResourceKindSchedule, ID: string(record.ID), Tenant: scope.Namespace, OwnerID: scope.OwnerID}); err != nil {
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
	scope, err := policyFilterScope(ctx, RuntimeScope{Namespace: filter.Namespace, OwnerID: filter.OwnerID})
	if err != nil {
		return Page[ScheduleRecord]{}, err
	}
	filter.Namespace, filter.OwnerID = scope.Namespace, scope.OwnerID
	if err := authorize(ctx, r.policy, ActionStorageRead, Resource{Kind: ResourceKindSchedule, ID: string(filter.WorkflowID), Tenant: scope.Namespace, OwnerID: scope.OwnerID}); err != nil {
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
	inner     ScheduleExecutionRepository
	schedules ScheduleRepository
	policy    Policy
}

func (r *guardedScheduleExecutionRepository) authorizeSchedule(ctx context.Context, action Action, id ScheduleID) error {
	if _, verified := RuntimeScopeFromContext(ctx); !verified {
		return authorize(ctx, r.policy, action, Resource{Kind: ResourceKindSchedule, ID: string(id)})
	}
	schedule, err := r.schedules.GetSchedule(ctx, id)
	if err != nil {
		return err
	}
	scope, err := policyScope(ctx, RuntimeScope{Namespace: schedule.Namespace, OwnerID: schedule.OwnerID})
	if err != nil {
		return err
	}
	return authorize(ctx, r.policy, action, Resource{Kind: ResourceKindSchedule, ID: string(id), Tenant: scope.Namespace, OwnerID: scope.OwnerID})
}

func (r *guardedScheduleExecutionRepository) SaveScheduleExecution(ctx context.Context, record ScheduleExecutionRecord) error {
	if err := r.authorizeSchedule(ctx, ActionStorageWrite, record.ScheduleID); err != nil {
		return err
	}
	return r.inner.SaveScheduleExecution(ctx, record)
}

func (r *guardedScheduleExecutionRepository) ListScheduleExecutions(ctx context.Context, id ScheduleID, page PageRequest) (Page[ScheduleExecutionRecord], error) {
	if err := r.authorizeSchedule(ctx, ActionStorageRead, id); err != nil {
		return Page[ScheduleExecutionRecord]{}, err
	}
	return r.inner.ListScheduleExecutions(ctx, id, page)
}
