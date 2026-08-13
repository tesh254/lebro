package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
)

// MemoryStore is a concurrency-safe Store intended for tests and local use.
// It preserves the same validation, pagination, and transaction semantics
// expected from durable adapters.
type MemoryStore struct {
	mu      sync.RWMutex
	state   memoryState
	version uint64
}

type memoryState struct {
	threads       map[ThreadID]ThreadRecord
	messages      map[ThreadID][]MessageRecord
	runs          map[RunID]WorkflowRunRecord
	snapshots     map[RunID][]WorkflowSnapshotRecord
	snapshotIDs   map[RunID]map[string]struct{}
	schedules     map[ScheduleID]ScheduleRecord
	executions    map[ScheduleID][]ScheduleExecutionRecord
	executionIDs  map[ScheduleID]map[string]struct{}
	workingMemory map[string]WorkingMemoryFact
}

// NewMemoryStore creates an empty in-memory storage implementation.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{state: newMemoryState()}
}

func newMemoryState() memoryState {
	return memoryState{
		threads:       map[ThreadID]ThreadRecord{},
		messages:      map[ThreadID][]MessageRecord{},
		runs:          map[RunID]WorkflowRunRecord{},
		snapshots:     map[RunID][]WorkflowSnapshotRecord{},
		snapshotIDs:   map[RunID]map[string]struct{}{},
		schedules:     map[ScheduleID]ScheduleRecord{},
		executions:    map[ScheduleID][]ScheduleExecutionRecord{},
		executionIDs:  map[ScheduleID]map[string]struct{}{},
		workingMemory: map[string]WorkingMemoryFact{},
	}
}

func (s *MemoryStore) Threads() ThreadRepository                     { return s }
func (s *MemoryStore) Messages() MessageRepository                   { return s }
func (s *MemoryStore) WorkflowRuns() WorkflowRunRepository           { return s }
func (s *MemoryStore) WorkflowSnapshots() WorkflowSnapshotRepository { return s }
func (s *MemoryStore) Schedules() ScheduleRepository                 { return s }
func (s *MemoryStore) ScheduleExecutions() ScheduleExecutionRepository {
	return s
}
func (s *MemoryStore) WorkingMemory() WorkingMemoryRepository { return s }

// Migrate is a no-op: MemoryStore has no external schema.
func (s *MemoryStore) Migrate(context.Context) error { return nil }

// Transaction runs fn against an isolated copy and commits it only if fn
// succeeds without a concurrent write. Callers may retry ErrConflict. The
// callback may read the outer store, but must not write to it or retain
// repositories after it returns. Outer-store writes are not transactional and
// persist even when fn returns an error or the transaction reports ErrConflict.
func (s *MemoryStore) Transaction(ctx context.Context, fn func(context.Context, Repositories) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.RLock()
	tx := &memoryRepositories{state: cloneMemoryState(s.state)}
	version := s.version
	s.mu.RUnlock()
	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !tx.dirty {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.version != version {
		return ErrConflict
	}
	s.state = tx.state
	s.version++
	return nil
}

func (s *MemoryStore) CreateThread(ctx context.Context, record ThreadRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := createThread(ctx, &s.state, record)
	if err == nil {
		s.version++
	}
	return err
}
func (s *MemoryStore) GetThread(ctx context.Context, id ThreadID) (ThreadRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return getThread(ctx, s.state, id)
}
func (s *MemoryStore) UpdateThread(ctx context.Context, record ThreadRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := updateThread(ctx, &s.state, record)
	if err == nil {
		s.version++
	}
	return err
}
func (s *MemoryStore) AppendMessages(ctx context.Context, records []MessageRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := appendMessages(ctx, &s.state, records)
	if err == nil {
		s.version++
	}
	return err
}
func (s *MemoryStore) ListMessages(ctx context.Context, id ThreadID, page PageRequest) (Page[MessageRecord], error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return listMessages(ctx, s.state, id, page)
}
func (s *MemoryStore) SaveWorkflowRun(ctx context.Context, record WorkflowRunRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := saveWorkflowRun(ctx, &s.state, record)
	if err == nil {
		s.version++
	}
	return err
}
func (s *MemoryStore) GetWorkflowRun(ctx context.Context, id RunID) (WorkflowRunRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return getWorkflowRun(ctx, s.state, id)
}
func (s *MemoryStore) ListWorkflowRuns(ctx context.Context, filter WorkflowRunFilter, page PageRequest) (Page[WorkflowRunRecord], error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return listWorkflowRuns(ctx, s.state, filter, page)
}
func (s *MemoryStore) SaveWorkflowSnapshot(ctx context.Context, record WorkflowSnapshotRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := saveWorkflowSnapshot(ctx, &s.state, record)
	if err == nil {
		s.version++
	}
	return err
}
func (s *MemoryStore) ListWorkflowSnapshots(ctx context.Context, id RunID, page PageRequest) (Page[WorkflowSnapshotRecord], error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return listWorkflowSnapshots(ctx, s.state, id, page)
}
func (s *MemoryStore) SaveSchedule(ctx context.Context, record ScheduleRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := saveSchedule(ctx, &s.state, record)
	if err == nil {
		s.version++
	}
	return err
}
func (s *MemoryStore) GetSchedule(ctx context.Context, id ScheduleID) (ScheduleRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return getSchedule(ctx, s.state, id)
}
func (s *MemoryStore) ListSchedules(ctx context.Context, filter ScheduleFilter, page PageRequest) (Page[ScheduleRecord], error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return listSchedules(ctx, s.state, filter, page)
}
func (s *MemoryStore) DeleteSchedule(ctx context.Context, id ScheduleID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := deleteSchedule(ctx, &s.state, id)
	if err == nil {
		s.version++
	}
	return err
}
func (s *MemoryStore) SaveScheduleExecution(ctx context.Context, record ScheduleExecutionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := saveScheduleExecution(ctx, &s.state, record)
	if err == nil {
		s.version++
	}
	return err
}
func (s *MemoryStore) ListScheduleExecutions(ctx context.Context, id ScheduleID, page PageRequest) (Page[ScheduleExecutionRecord], error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return listScheduleExecutions(ctx, s.state, id, page)
}

type memoryRepositories struct {
	state memoryState
	dirty bool
}

func (r *memoryRepositories) Threads() ThreadRepository                     { return r }
func (r *memoryRepositories) Messages() MessageRepository                   { return r }
func (r *memoryRepositories) WorkflowRuns() WorkflowRunRepository           { return r }
func (r *memoryRepositories) WorkflowSnapshots() WorkflowSnapshotRepository { return r }
func (r *memoryRepositories) Schedules() ScheduleRepository                 { return r }
func (r *memoryRepositories) ScheduleExecutions() ScheduleExecutionRepository {
	return r
}
func (r *memoryRepositories) WorkingMemory() WorkingMemoryRepository { return r }
func (r *memoryRepositories) CreateThread(ctx context.Context, v ThreadRecord) error {
	err := createThread(ctx, &r.state, v)
	if err == nil {
		r.dirty = true
	}
	return err
}
func (r *memoryRepositories) GetThread(ctx context.Context, id ThreadID) (ThreadRecord, error) {
	return getThread(ctx, r.state, id)
}
func (r *memoryRepositories) UpdateThread(ctx context.Context, v ThreadRecord) error {
	err := updateThread(ctx, &r.state, v)
	if err == nil {
		r.dirty = true
	}
	return err
}
func (r *memoryRepositories) AppendMessages(ctx context.Context, v []MessageRecord) error {
	err := appendMessages(ctx, &r.state, v)
	if err == nil && len(v) > 0 {
		r.dirty = true
	}
	return err
}
func (r *memoryRepositories) ListMessages(ctx context.Context, id ThreadID, p PageRequest) (Page[MessageRecord], error) {
	return listMessages(ctx, r.state, id, p)
}
func (r *memoryRepositories) SaveWorkflowRun(ctx context.Context, v WorkflowRunRecord) error {
	err := saveWorkflowRun(ctx, &r.state, v)
	if err == nil {
		r.dirty = true
	}
	return err
}
func (r *memoryRepositories) GetWorkflowRun(ctx context.Context, id RunID) (WorkflowRunRecord, error) {
	return getWorkflowRun(ctx, r.state, id)
}
func (r *memoryRepositories) ListWorkflowRuns(ctx context.Context, filter WorkflowRunFilter, page PageRequest) (Page[WorkflowRunRecord], error) {
	return listWorkflowRuns(ctx, r.state, filter, page)
}
func (r *memoryRepositories) SaveWorkflowSnapshot(ctx context.Context, v WorkflowSnapshotRecord) error {
	err := saveWorkflowSnapshot(ctx, &r.state, v)
	if err == nil {
		r.dirty = true
	}
	return err
}
func (r *memoryRepositories) ListWorkflowSnapshots(ctx context.Context, id RunID, p PageRequest) (Page[WorkflowSnapshotRecord], error) {
	return listWorkflowSnapshots(ctx, r.state, id, p)
}
func (r *memoryRepositories) SaveSchedule(ctx context.Context, v ScheduleRecord) error {
	err := saveSchedule(ctx, &r.state, v)
	if err == nil {
		r.dirty = true
	}
	return err
}
func (r *memoryRepositories) GetSchedule(ctx context.Context, id ScheduleID) (ScheduleRecord, error) {
	return getSchedule(ctx, r.state, id)
}
func (r *memoryRepositories) ListSchedules(ctx context.Context, filter ScheduleFilter, p PageRequest) (Page[ScheduleRecord], error) {
	return listSchedules(ctx, r.state, filter, p)
}
func (r *memoryRepositories) DeleteSchedule(ctx context.Context, id ScheduleID) error {
	err := deleteSchedule(ctx, &r.state, id)
	if err == nil {
		r.dirty = true
	}
	return err
}
func (r *memoryRepositories) SaveScheduleExecution(ctx context.Context, v ScheduleExecutionRecord) error {
	err := saveScheduleExecution(ctx, &r.state, v)
	if err == nil {
		r.dirty = true
	}
	return err
}
func (r *memoryRepositories) ListScheduleExecutions(ctx context.Context, id ScheduleID, p PageRequest) (Page[ScheduleExecutionRecord], error) {
	return listScheduleExecutions(ctx, r.state, id, p)
}

func createThread(ctx context.Context, s *memoryState, v ThreadRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if v.ID == "" {
		return fmt.Errorf("lebro: thread ID is required")
	}
	if err := validateJSON(v.Metadata); err != nil {
		return fmt.Errorf("lebro: thread metadata: %w", err)
	}
	if err := validateRecord(v); err != nil {
		return fmt.Errorf("lebro: thread: %w", err)
	}
	if _, ok := s.threads[v.ID]; ok {
		return fmt.Errorf("lebro: thread %q already exists", v.ID)
	}
	s.threads[v.ID] = cloneThreadRecord(v)
	return nil
}
func getThread(ctx context.Context, s memoryState, id ThreadID) (ThreadRecord, error) {
	if err := ctx.Err(); err != nil {
		return ThreadRecord{}, err
	}
	v, ok := s.threads[id]
	if !ok {
		return ThreadRecord{}, ErrNotFound
	}
	return cloneThreadRecord(v), nil
}
func updateThread(ctx context.Context, s *memoryState, v ThreadRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := s.threads[v.ID]; !ok {
		return ErrNotFound
	}
	if err := validateJSON(v.Metadata); err != nil {
		return fmt.Errorf("lebro: thread metadata: %w", err)
	}
	if err := validateRecord(v); err != nil {
		return fmt.Errorf("lebro: thread: %w", err)
	}
	s.threads[v.ID] = cloneThreadRecord(v)
	return nil
}
func appendMessages(ctx context.Context, s *memoryState, vs []MessageRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	seen := make(map[ThreadID]map[string]struct{}, len(vs))
	existing := make(map[ThreadID]map[string]struct{}, len(vs))
	for _, v := range vs {
		if v.ID == "" || v.ThreadID == "" {
			return fmt.Errorf("lebro: message and thread IDs are required")
		}
		if _, ok := s.threads[v.ThreadID]; !ok {
			return ErrNotFound
		}
		if err := v.Message.Validate(); err != nil {
			return err
		}
		if err := validateJSON(v.Metadata); err != nil {
			return fmt.Errorf("lebro: message metadata: %w", err)
		}
		if err := validateRecord(v); err != nil {
			return fmt.Errorf("lebro: message: %w", err)
		}
		if existing[v.ThreadID] == nil {
			existing[v.ThreadID] = make(map[string]struct{}, len(s.messages[v.ThreadID]))
			for _, message := range s.messages[v.ThreadID] {
				existing[v.ThreadID][message.ID] = struct{}{}
			}
		}
		if _, ok := existing[v.ThreadID][v.ID]; ok {
			return fmt.Errorf("lebro: message %q already exists", v.ID)
		}
		if seen[v.ThreadID] == nil {
			seen[v.ThreadID] = map[string]struct{}{}
		}
		if _, ok := seen[v.ThreadID][v.ID]; ok {
			return fmt.Errorf("lebro: message %q already exists", v.ID)
		}
		seen[v.ThreadID][v.ID] = struct{}{}
	}
	for _, v := range vs {
		s.messages[v.ThreadID] = append(s.messages[v.ThreadID], cloneMessageRecord(v))
	}
	return nil
}
func listMessages(ctx context.Context, s memoryState, id ThreadID, p PageRequest) (Page[MessageRecord], error) {
	if err := ctx.Err(); err != nil {
		return Page[MessageRecord]{}, err
	}
	if _, ok := s.threads[id]; !ok {
		return Page[MessageRecord]{}, ErrNotFound
	}
	return paginate(s.messages[id], p, cloneMessageRecord)
}
func saveWorkflowRun(ctx context.Context, s *memoryState, v WorkflowRunRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if v.ID == "" || v.WorkflowID == "" {
		return fmt.Errorf("lebro: workflow run and workflow IDs are required")
	}
	for name, value := range map[string]json.RawMessage{"input": v.Input, "output": v.Output, "metadata": v.Metadata} {
		if err := validateJSON(value); err != nil {
			return fmt.Errorf("lebro: workflow run %s: %w", name, err)
		}
	}
	for i, output := range v.StepOutputs {
		if err := validateJSON(output); err != nil {
			return fmt.Errorf("lebro: workflow run step output %d: %w", i, err)
		}
	}
	if v.Failure != nil {
		if err := validateRecord(v.Failure); err != nil {
			return fmt.Errorf("lebro: workflow run failure: %w", err)
		}
	}
	if err := validateRecord(v); err != nil {
		return fmt.Errorf("lebro: workflow run: %w", err)
	}
	s.runs[v.ID] = cloneWorkflowRunRecord(v)
	return nil
}
func getWorkflowRun(ctx context.Context, s memoryState, id RunID) (WorkflowRunRecord, error) {
	if err := ctx.Err(); err != nil {
		return WorkflowRunRecord{}, err
	}
	v, ok := s.runs[id]
	if !ok {
		return WorkflowRunRecord{}, ErrNotFound
	}
	return cloneWorkflowRunRecord(v), nil
}
func listWorkflowRuns(ctx context.Context, s memoryState, filter WorkflowRunFilter, p PageRequest) (Page[WorkflowRunRecord], error) {
	if err := ctx.Err(); err != nil {
		return Page[WorkflowRunRecord]{}, err
	}
	ids := make([]RunID, 0, len(s.runs))
	for id := range s.runs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		left, right := s.runs[ids[i]], s.runs[ids[j]]
		if left.StartedAt.Equal(right.StartedAt) {
			return ids[i] < ids[j]
		}
		return left.StartedAt.Before(right.StartedAt)
	})
	filtered := make([]WorkflowRunRecord, 0, len(ids))
	for _, id := range ids {
		run := s.runs[id]
		if filter.WorkflowID != "" && run.WorkflowID != filter.WorkflowID {
			continue
		}
		if filter.Status != "" && run.Status != filter.Status {
			continue
		}
		filtered = append(filtered, run)
	}
	return paginate(filtered, p, cloneWorkflowRunRecord)
}
func saveWorkflowSnapshot(ctx context.Context, s *memoryState, v WorkflowSnapshotRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if v.ID == "" || v.RunID == "" {
		return fmt.Errorf("lebro: workflow snapshot and run IDs are required")
	}
	if err := validateJSON(v.State); err != nil {
		return fmt.Errorf("lebro: workflow snapshot state: %w", err)
	}
	if err := validateRecord(v); err != nil {
		return fmt.Errorf("lebro: workflow snapshot: %w", err)
	}
	if _, ok := s.runs[v.RunID]; !ok {
		return ErrNotFound
	}
	if s.snapshotIDs[v.RunID] == nil {
		s.snapshotIDs[v.RunID] = map[string]struct{}{}
	}
	if _, ok := s.snapshotIDs[v.RunID][v.ID]; ok {
		return fmt.Errorf("lebro: workflow snapshot already exists")
	}
	snapshots := s.snapshots[v.RunID]
	index := sort.Search(len(snapshots), func(i int) bool { return snapshots[i].Sequence >= v.Sequence })
	if index < len(snapshots) && snapshots[index].Sequence == v.Sequence {
		return fmt.Errorf("lebro: workflow snapshot already exists")
	}
	snapshots = append(snapshots, WorkflowSnapshotRecord{})
	copy(snapshots[index+1:], snapshots[index:])
	snapshots[index] = cloneWorkflowSnapshotRecord(v)
	s.snapshots[v.RunID] = snapshots
	s.snapshotIDs[v.RunID][v.ID] = struct{}{}
	return nil
}
func listWorkflowSnapshots(ctx context.Context, s memoryState, id RunID, p PageRequest) (Page[WorkflowSnapshotRecord], error) {
	if err := ctx.Err(); err != nil {
		return Page[WorkflowSnapshotRecord]{}, err
	}
	if _, ok := s.runs[id]; !ok {
		return Page[WorkflowSnapshotRecord]{}, ErrNotFound
	}
	return paginate(s.snapshots[id], p, cloneWorkflowSnapshotRecord)
}

func saveSchedule(ctx context.Context, s *memoryState, v ScheduleRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if v.ID == "" || v.WorkflowID == "" {
		return fmt.Errorf("lebro: schedule and workflow IDs are required")
	}
	if v.Spec == "" {
		return fmt.Errorf("lebro: schedule spec is required")
	}
	for name, value := range map[string]json.RawMessage{"input": v.Input, "metadata": v.Metadata} {
		if err := validateJSON(value); err != nil {
			return fmt.Errorf("lebro: schedule %s: %w", name, err)
		}
	}
	if err := validateRecord(v); err != nil {
		return fmt.Errorf("lebro: schedule: %w", err)
	}
	s.schedules[v.ID] = cloneScheduleRecord(v)
	return nil
}
func getSchedule(ctx context.Context, s memoryState, id ScheduleID) (ScheduleRecord, error) {
	if err := ctx.Err(); err != nil {
		return ScheduleRecord{}, err
	}
	v, ok := s.schedules[id]
	if !ok {
		return ScheduleRecord{}, ErrNotFound
	}
	return cloneScheduleRecord(v), nil
}
func listSchedules(ctx context.Context, s memoryState, filter ScheduleFilter, p PageRequest) (Page[ScheduleRecord], error) {
	if err := ctx.Err(); err != nil {
		return Page[ScheduleRecord]{}, err
	}
	ids := make([]ScheduleID, 0, len(s.schedules))
	for id := range s.schedules {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		left, right := s.schedules[ids[i]], s.schedules[ids[j]]
		if left.CreatedAt.Equal(right.CreatedAt) {
			return ids[i] < ids[j]
		}
		return left.CreatedAt.Before(right.CreatedAt)
	})
	filtered := make([]ScheduleRecord, 0, len(ids))
	for _, id := range ids {
		schedule := s.schedules[id]
		if !scheduleMatchesFilter(schedule, filter) {
			continue
		}
		filtered = append(filtered, schedule)
	}
	return paginate(filtered, p, cloneScheduleRecord)
}

// scheduleMatchesFilter reports whether a schedule satisfies the filter. A
// DueBy filter selects only non-paused schedules with a NextFireAt at or before
// the instant, so a paused or unscheduled (nil NextFireAt) schedule is never
// returned as due work.
func scheduleMatchesFilter(schedule ScheduleRecord, filter ScheduleFilter) bool {
	if filter.WorkflowID != "" && schedule.WorkflowID != filter.WorkflowID {
		return false
	}
	if filter.DueBy != nil {
		if schedule.Paused || schedule.NextFireAt == nil || schedule.NextFireAt.After(*filter.DueBy) {
			return false
		}
	}
	return true
}
func deleteSchedule(ctx context.Context, s *memoryState, id ScheduleID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := s.schedules[id]; !ok {
		return ErrNotFound
	}
	// Remove the schedule's execution history with it so a schedule recreated
	// under the same ID does not inherit the old executions, matching the SQL
	// adapters' ON DELETE CASCADE.
	delete(s.schedules, id)
	delete(s.executions, id)
	delete(s.executionIDs, id)
	return nil
}
func saveScheduleExecution(ctx context.Context, s *memoryState, v ScheduleExecutionRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if v.ID == "" || v.ScheduleID == "" {
		return fmt.Errorf("lebro: schedule execution and schedule IDs are required")
	}
	if err := validateRecord(v); err != nil {
		return fmt.Errorf("lebro: schedule execution: %w", err)
	}
	if _, ok := s.schedules[v.ScheduleID]; !ok {
		return ErrNotFound
	}
	if s.executionIDs[v.ScheduleID] == nil {
		s.executionIDs[v.ScheduleID] = map[string]struct{}{}
	}
	if _, ok := s.executionIDs[v.ScheduleID][string(v.ID)]; ok {
		return fmt.Errorf("lebro: schedule execution already exists")
	}
	s.executions[v.ScheduleID] = append(s.executions[v.ScheduleID], cloneScheduleExecutionRecord(v))
	s.executionIDs[v.ScheduleID][string(v.ID)] = struct{}{}
	return nil
}
func listScheduleExecutions(ctx context.Context, s memoryState, id ScheduleID, p PageRequest) (Page[ScheduleExecutionRecord], error) {
	if err := ctx.Err(); err != nil {
		return Page[ScheduleExecutionRecord]{}, err
	}
	if _, ok := s.schedules[id]; !ok {
		return Page[ScheduleExecutionRecord]{}, ErrNotFound
	}
	return paginate(s.executions[id], p, cloneScheduleExecutionRecord)
}

func paginate[T any](records []T, request PageRequest, cloneRecord func(T) T) (Page[T], error) {
	start := 0
	if request.Cursor != "" {
		var err error
		start, err = strconv.Atoi(request.Cursor)
		if err != nil || start < 0 {
			return Page[T]{}, ErrInvalidPage
		}
	}
	limit := request.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 0 {
		return Page[T]{}, ErrInvalidPage
	}
	if start >= len(records) {
		return Page[T]{Records: []T{}}, nil
	}
	end := len(records)
	if limit < len(records)-start {
		end = start + limit
	}
	page := Page[T]{Records: make([]T, 0, end-start)}
	for _, record := range records[start:end] {
		page.Records = append(page.Records, cloneRecord(record))
	}
	if end < len(records) {
		page.NextCursor = strconv.Itoa(end)
	}
	return page, nil
}
func cloneMemoryState(s memoryState) memoryState {
	out := newMemoryState()
	for k, v := range s.threads {
		out.threads[k] = cloneThreadRecord(v)
	}
	for k, v := range s.messages {
		out.messages[k] = append([]MessageRecord(nil), v...)
		for i := range out.messages[k] {
			out.messages[k][i] = cloneMessageRecord(out.messages[k][i])
		}
	}
	for k, v := range s.runs {
		out.runs[k] = cloneWorkflowRunRecord(v)
	}
	for k, v := range s.snapshots {
		out.snapshots[k] = append([]WorkflowSnapshotRecord(nil), v...)
		for i := range out.snapshots[k] {
			out.snapshots[k][i] = cloneWorkflowSnapshotRecord(out.snapshots[k][i])
		}
	}
	for runID, ids := range s.snapshotIDs {
		out.snapshotIDs[runID] = make(map[string]struct{}, len(ids))
		for id := range ids {
			out.snapshotIDs[runID][id] = struct{}{}
		}
	}
	for k, v := range s.schedules {
		out.schedules[k] = cloneScheduleRecord(v)
	}
	for k, v := range s.executions {
		out.executions[k] = append([]ScheduleExecutionRecord(nil), v...)
		for i := range out.executions[k] {
			out.executions[k][i] = cloneScheduleExecutionRecord(out.executions[k][i])
		}
	}
	for scheduleID, ids := range s.executionIDs {
		out.executionIDs[scheduleID] = make(map[string]struct{}, len(ids))
		for id := range ids {
			out.executionIDs[scheduleID][id] = struct{}{}
		}
	}
	for k, v := range s.workingMemory {
		out.workingMemory[k] = cloneWorkingMemoryFact(v)
	}
	return out
}

func cloneScheduleRecord(v ScheduleRecord) ScheduleRecord {
	v.Input = cloneJSON(v.Input)
	v.Metadata = cloneJSON(v.Metadata)
	if v.NextFireAt != nil {
		next := *v.NextFireAt
		v.NextFireAt = &next
	}
	if v.LastFireAt != nil {
		last := *v.LastFireAt
		v.LastFireAt = &last
	}
	return v
}
func cloneScheduleExecutionRecord(v ScheduleExecutionRecord) ScheduleExecutionRecord {
	if v.FinishedAt != nil {
		finished := *v.FinishedAt
		v.FinishedAt = &finished
	}
	return v
}

func cloneThreadRecord(v ThreadRecord) ThreadRecord { v.Metadata = cloneJSON(v.Metadata); return v }
func cloneMessageRecord(v MessageRecord) MessageRecord {
	v.Metadata = cloneJSON(v.Metadata)
	return v
}
func cloneWorkflowRunRecord(v WorkflowRunRecord) WorkflowRunRecord {
	v.Input = cloneJSON(v.Input)
	v.Output = cloneJSON(v.Output)
	v.Metadata = cloneJSON(v.Metadata)
	if len(v.StepOutputs) > 0 {
		outputs := make([]json.RawMessage, len(v.StepOutputs))
		for i, output := range v.StepOutputs {
			outputs[i] = cloneJSON(output)
		}
		v.StepOutputs = outputs
	}
	if len(v.Path) > 0 {
		path := make([]StepID, len(v.Path))
		copy(path, v.Path)
		v.Path = path
	}
	if v.Failure != nil {
		failure := *v.Failure
		v.Failure = &failure
	}
	if v.FinishedAt != nil {
		finished := *v.FinishedAt
		v.FinishedAt = &finished
	}
	if len(v.FanOut) > 0 {
		fanOut := make([]FanOutJoinResult, len(v.FanOut))
		for i, result := range v.FanOut {
			fanOut[i] = result
			if len(result.Branches) > 0 {
				branches := make([]FanOutBranchResult, len(result.Branches))
				for j, branch := range result.Branches {
					branches[j] = branch
					branches[j].Output = cloneJSON(branch.Output)
				}
				fanOut[i].Branches = branches
			}
		}
		v.FanOut = fanOut
	}
	return v
}
func cloneWorkflowSnapshotRecord(v WorkflowSnapshotRecord) WorkflowSnapshotRecord {
	v.State = cloneJSON(v.State)
	return v
}
func cloneJSON(v json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), v...) }

func validateJSON(value json.RawMessage) error {
	if len(value) == 0 || json.Valid(value) {
		return nil
	}
	return errors.New("must be valid JSON")
}

func validateRecord(record any) error {
	if _, err := json.Marshal(record); err != nil {
		return fmt.Errorf("must be JSON serializable: %w", err)
	}
	return nil
}
