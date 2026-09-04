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
	events        map[RunID][]RunEventRecord
	eventIDs      map[RunID]map[string]struct{}
	attempts      map[RunID][]ModelAttemptRecord
	attemptIDs    map[RunID]map[string]struct{}
	toolExecs     map[RunID][]ToolExecutionRecord
	toolExecIDs   map[RunID]map[string]struct{}
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
		events:        map[RunID][]RunEventRecord{},
		eventIDs:      map[RunID]map[string]struct{}{},
		attempts:      map[RunID][]ModelAttemptRecord{},
		attemptIDs:    map[RunID]map[string]struct{}{},
		toolExecs:     map[RunID][]ToolExecutionRecord{},
		toolExecIDs:   map[RunID]map[string]struct{}{},
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
func (s *MemoryStore) RunEvents() RunEventRepository          { return s }
func (s *MemoryStore) ModelAttempts() ModelAttemptRepository  { return s }
func (s *MemoryStore) ToolExecutions() ToolExecutionRepository {
	return s
}

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
func (s *MemoryStore) UpdateMessages(ctx context.Context, records []MessageRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := updateMessages(ctx, &s.state, records)
	if err == nil && len(records) > 0 {
		s.version++
	}
	return err
}
func (s *MemoryStore) DeleteMessages(ctx context.Context, id ThreadID, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := deleteMessages(ctx, &s.state, id, ids)
	if err == nil && len(ids) > 0 {
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
func (s *MemoryStore) AppendRunEvents(ctx context.Context, records []RunEventRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := appendRunEvents(ctx, &s.state, records)
	if err == nil && len(records) > 0 {
		s.version++
	}
	return err
}
func (s *MemoryStore) ListRunEvents(ctx context.Context, filter RunEventFilter, page PageRequest) (Page[RunEventRecord], error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return listRunEvents(ctx, s.state, filter, page)
}
func (s *MemoryStore) SaveModelAttempts(ctx context.Context, records []ModelAttemptRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := saveModelAttempts(ctx, &s.state, records)
	if err == nil && len(records) > 0 {
		s.version++
	}
	return err
}
func (s *MemoryStore) ListModelAttempts(ctx context.Context, filter ModelAttemptFilter, page PageRequest) (Page[ModelAttemptRecord], error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return listModelAttempts(ctx, s.state, filter, page)
}
func (s *MemoryStore) SaveToolExecutions(ctx context.Context, records []ToolExecutionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := saveToolExecutions(ctx, &s.state, records)
	if err == nil && len(records) > 0 {
		s.version++
	}
	return err
}
func (s *MemoryStore) ListToolExecutions(ctx context.Context, filter ToolExecutionFilter, page PageRequest) (Page[ToolExecutionRecord], error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return listToolExecutions(ctx, s.state, filter, page)
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
func (r *memoryRepositories) RunEvents() RunEventRepository          { return r }
func (r *memoryRepositories) ModelAttempts() ModelAttemptRepository  { return r }
func (r *memoryRepositories) ToolExecutions() ToolExecutionRepository {
	return r
}
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
func (r *memoryRepositories) UpdateMessages(ctx context.Context, v []MessageRecord) error {
	err := updateMessages(ctx, &r.state, v)
	if err == nil && len(v) > 0 {
		r.dirty = true
	}
	return err
}
func (r *memoryRepositories) DeleteMessages(ctx context.Context, id ThreadID, ids []string) error {
	err := deleteMessages(ctx, &r.state, id, ids)
	if err == nil && len(ids) > 0 {
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
func (r *memoryRepositories) AppendRunEvents(ctx context.Context, records []RunEventRecord) error {
	err := appendRunEvents(ctx, &r.state, records)
	if err == nil && len(records) > 0 {
		r.dirty = true
	}
	return err
}
func (r *memoryRepositories) ListRunEvents(ctx context.Context, filter RunEventFilter, p PageRequest) (Page[RunEventRecord], error) {
	return listRunEvents(ctx, r.state, filter, p)
}
func (r *memoryRepositories) SaveModelAttempts(ctx context.Context, records []ModelAttemptRecord) error {
	err := saveModelAttempts(ctx, &r.state, records)
	if err == nil && len(records) > 0 {
		r.dirty = true
	}
	return err
}
func (r *memoryRepositories) ListModelAttempts(ctx context.Context, filter ModelAttemptFilter, p PageRequest) (Page[ModelAttemptRecord], error) {
	return listModelAttempts(ctx, r.state, filter, p)
}
func (r *memoryRepositories) SaveToolExecutions(ctx context.Context, records []ToolExecutionRecord) error {
	err := saveToolExecutions(ctx, &r.state, records)
	if err == nil && len(records) > 0 {
		r.dirty = true
	}
	return err
}
func (r *memoryRepositories) ListToolExecutions(ctx context.Context, filter ToolExecutionFilter, p PageRequest) (Page[ToolExecutionRecord], error) {
	return listToolExecutions(ctx, r.state, filter, p)
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
func updateMessages(ctx context.Context, s *memoryState, vs []MessageRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	seen := make(map[ThreadID]map[string]struct{}, len(vs))
	for _, v := range vs {
		if v.ID == "" || v.ThreadID == "" {
			return errors.New("lebro: message and thread IDs are required")
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
		if seen[v.ThreadID] == nil {
			seen[v.ThreadID] = map[string]struct{}{}
		}
		if _, ok := seen[v.ThreadID][v.ID]; ok {
			return fmt.Errorf("lebro: duplicate message %q", v.ID)
		}
		seen[v.ThreadID][v.ID] = struct{}{}
		found := false
		for _, existing := range s.messages[v.ThreadID] {
			if existing.ID == v.ID {
				found = true
				break
			}
		}
		if !found {
			return ErrNotFound
		}
	}
	for _, v := range vs {
		for i, existing := range s.messages[v.ThreadID] {
			if existing.ID == v.ID {
				v.CreatedAt = existing.CreatedAt
				s.messages[v.ThreadID][i] = cloneMessageRecord(v)
				break
			}
		}
	}
	return nil
}
func deleteMessages(ctx context.Context, s *memoryState, id ThreadID, ids []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := s.threads[id]; !ok {
		return ErrNotFound
	}
	if len(ids) == 0 {
		return nil
	}
	remove := make(map[string]struct{}, len(ids))
	for _, messageID := range ids {
		if messageID == "" {
			return errors.New("lebro: message ID is required")
		}
		remove[messageID] = struct{}{}
	}
	kept := s.messages[id][:0]
	for _, message := range s.messages[id] {
		if _, ok := remove[message.ID]; !ok {
			kept = append(kept, message)
		}
	}
	s.messages[id] = kept
	return nil
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

func appendRunEvents(ctx context.Context, s *memoryState, vs []RunEventRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.eventIDs == nil {
		s.eventIDs = map[RunID]map[string]struct{}{}
	}
	// Appends are idempotent: a record whose (run, ID) pair already exists is
	// skipped, because observability writes must never fail a run when two
	// independently constructed agents sharing a store reuse default run IDs.
	for _, v := range vs {
		if err := validateRunEventRecord(v); err != nil {
			return err
		}
	}
	for _, v := range vs {
		if s.eventIDs[v.RunID] == nil {
			s.eventIDs[v.RunID] = map[string]struct{}{}
			s.events[v.RunID] = nil
		}
		if _, ok := s.eventIDs[v.RunID][v.ID]; ok {
			continue
		}
		s.events[v.RunID] = append(s.events[v.RunID], cloneRunEventRecord(v))
		s.eventIDs[v.RunID][v.ID] = struct{}{}
	}
	return nil
}

func listRunEvents(ctx context.Context, s memoryState, filter RunEventFilter, p PageRequest) (Page[RunEventRecord], error) {
	if err := ctx.Err(); err != nil {
		return Page[RunEventRecord]{}, err
	}
	matched := make([]RunEventRecord, 0)
	for _, runID := range orderedRunIDs(s.events) {
		if filter.RunID != "" && runID != filter.RunID {
			continue
		}
		for _, event := range s.events[runID] {
			if !runEventMatchesFilter(event, filter) {
				continue
			}
			matched = append(matched, cloneRunEventRecord(event))
		}
	}
	return paginate(matched, p, func(v RunEventRecord) RunEventRecord { return v })
}

// orderedRunIDs returns map keys in insertion-independent but deterministic
// order. Records keep their insertion order inside each run; runs are walked
// in ID order so unfiltered listings are stable.
func orderedRunIDs[V any](byRun map[RunID][]V) []RunID {
	ids := make([]RunID, 0, len(byRun))
	for id := range byRun {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func runEventMatchesFilter(event RunEventRecord, filter RunEventFilter) bool {
	if filter.ThreadID != "" && event.ThreadID != filter.ThreadID {
		return false
	}
	if filter.Namespace != "" && event.Namespace != filter.Namespace {
		return false
	}
	if filter.OwnerID != "" && event.OwnerID != filter.OwnerID {
		return false
	}
	if filter.Type != "" && event.Type != filter.Type {
		return false
	}
	if filter.Provider != "" && event.Provider != filter.Provider {
		return false
	}
	if filter.ToolID != "" && event.ToolID != filter.ToolID {
		return false
	}
	return withinEventRange(event.Timestamp, filter.From, filter.To)
}

func saveModelAttempts(ctx context.Context, s *memoryState, vs []ModelAttemptRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.attemptIDs == nil {
		s.attemptIDs = map[RunID]map[string]struct{}{}
	}
	// Idempotent save: see appendRunEvents.
	for _, v := range vs {
		if err := validateModelAttemptRecord(v); err != nil {
			return err
		}
	}
	for _, v := range vs {
		if s.attemptIDs[v.RunID] == nil {
			s.attemptIDs[v.RunID] = map[string]struct{}{}
			s.attempts[v.RunID] = nil
		}
		if _, ok := s.attemptIDs[v.RunID][v.ID]; ok {
			continue
		}
		s.attempts[v.RunID] = append(s.attempts[v.RunID], cloneModelAttemptRecord(v))
		s.attemptIDs[v.RunID][v.ID] = struct{}{}
	}
	return nil
}

func listModelAttempts(ctx context.Context, s memoryState, filter ModelAttemptFilter, p PageRequest) (Page[ModelAttemptRecord], error) {
	if err := ctx.Err(); err != nil {
		return Page[ModelAttemptRecord]{}, err
	}
	matched := make([]ModelAttemptRecord, 0)
	for _, runID := range orderedRunIDs(s.attempts) {
		if filter.RunID != "" && runID != filter.RunID {
			continue
		}
		for _, attempt := range s.attempts[runID] {
			if filter.ThreadID != "" && attempt.ThreadID != filter.ThreadID {
				continue
			}
			if filter.Namespace != "" && attempt.Namespace != filter.Namespace {
				continue
			}
			if filter.OwnerID != "" && attempt.OwnerID != filter.OwnerID {
				continue
			}
			if filter.Provider != "" && attempt.Provider != filter.Provider {
				continue
			}
			if filter.Status != "" && attempt.Status != filter.Status {
				continue
			}
			matched = append(matched, cloneModelAttemptRecord(attempt))
		}
	}
	return paginate(matched, p, func(v ModelAttemptRecord) ModelAttemptRecord { return v })
}

func saveToolExecutions(ctx context.Context, s *memoryState, vs []ToolExecutionRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.toolExecIDs == nil {
		s.toolExecIDs = map[RunID]map[string]struct{}{}
	}
	// Idempotent save: see appendRunEvents.
	for _, v := range vs {
		if err := validateToolExecutionRecord(v); err != nil {
			return err
		}
	}
	for _, v := range vs {
		if s.toolExecIDs[v.RunID] == nil {
			s.toolExecIDs[v.RunID] = map[string]struct{}{}
			s.toolExecs[v.RunID] = nil
		}
		if _, ok := s.toolExecIDs[v.RunID][v.ID]; ok {
			continue
		}
		s.toolExecs[v.RunID] = append(s.toolExecs[v.RunID], cloneToolExecutionRecord(v))
		s.toolExecIDs[v.RunID][v.ID] = struct{}{}
	}
	return nil
}

func listToolExecutions(ctx context.Context, s memoryState, filter ToolExecutionFilter, p PageRequest) (Page[ToolExecutionRecord], error) {
	if err := ctx.Err(); err != nil {
		return Page[ToolExecutionRecord]{}, err
	}
	matched := make([]ToolExecutionRecord, 0)
	for _, runID := range orderedRunIDs(s.toolExecs) {
		if filter.RunID != "" && runID != filter.RunID {
			continue
		}
		for _, execution := range s.toolExecs[runID] {
			if filter.ThreadID != "" && execution.ThreadID != filter.ThreadID {
				continue
			}
			if filter.Namespace != "" && execution.Namespace != filter.Namespace {
				continue
			}
			if filter.OwnerID != "" && execution.OwnerID != filter.OwnerID {
				continue
			}
			if filter.ToolID != "" && execution.ToolID != filter.ToolID {
				continue
			}
			if filter.State != "" && execution.State != filter.State {
				continue
			}
			matched = append(matched, cloneToolExecutionRecord(execution))
		}
	}
	return paginate(matched, p, func(v ToolExecutionRecord) ToolExecutionRecord { return v })
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
	for runID, events := range s.events {
		out.events[runID] = make([]RunEventRecord, len(events))
		for i, event := range events {
			out.events[runID][i] = cloneRunEventRecord(event)
		}
	}
	for runID, ids := range s.eventIDs {
		out.eventIDs[runID] = make(map[string]struct{}, len(ids))
		for id := range ids {
			out.eventIDs[runID][id] = struct{}{}
		}
	}
	for runID, attempts := range s.attempts {
		out.attempts[runID] = make([]ModelAttemptRecord, len(attempts))
		for i, attempt := range attempts {
			out.attempts[runID][i] = cloneModelAttemptRecord(attempt)
		}
	}
	for runID, ids := range s.attemptIDs {
		out.attemptIDs[runID] = make(map[string]struct{}, len(ids))
		for id := range ids {
			out.attemptIDs[runID][id] = struct{}{}
		}
	}
	for runID, executions := range s.toolExecs {
		out.toolExecs[runID] = make([]ToolExecutionRecord, len(executions))
		for i, execution := range executions {
			out.toolExecs[runID][i] = cloneToolExecutionRecord(execution)
		}
	}
	for runID, ids := range s.toolExecIDs {
		out.toolExecIDs[runID] = make(map[string]struct{}, len(ids))
		for id := range ids {
			out.toolExecIDs[runID][id] = struct{}{}
		}
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
	v.Annotations = v.Annotations.Clone()
	return v
}
func cloneRunEventRecord(v RunEventRecord) RunEventRecord {
	v.Payload = cloneJSON(v.Payload)
	v.Metadata = v.Metadata.Clone()
	if v.Plugin != nil {
		plugin := *v.Plugin
		v.Plugin = &plugin
	}
	return v
}
func cloneModelAttemptRecord(v ModelAttemptRecord) ModelAttemptRecord {
	if len(v.ProducedMessageIDs) > 0 {
		v.ProducedMessageIDs = append([]string(nil), v.ProducedMessageIDs...)
	}
	v.Metadata = v.Metadata.Clone()
	return v
}
func cloneToolExecutionRecord(v ToolExecutionRecord) ToolExecutionRecord {
	v.Metadata = v.Metadata.Clone()
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
