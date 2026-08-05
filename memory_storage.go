package lebro

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
	mu    sync.RWMutex
	state memoryState
}

type memoryState struct {
	threads   map[ThreadID]ThreadRecord
	messages  map[ThreadID][]MessageRecord
	runs      map[RunID]WorkflowRunRecord
	snapshots map[RunID][]WorkflowSnapshotRecord
}

// NewMemoryStore creates an empty in-memory storage implementation.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{state: newMemoryState()}
}

func newMemoryState() memoryState {
	return memoryState{map[ThreadID]ThreadRecord{}, map[ThreadID][]MessageRecord{}, map[RunID]WorkflowRunRecord{}, map[RunID][]WorkflowSnapshotRecord{}}
}

func (s *MemoryStore) Threads() ThreadRepository                     { return s }
func (s *MemoryStore) Messages() MessageRepository                   { return s }
func (s *MemoryStore) WorkflowRuns() WorkflowRunRepository           { return s }
func (s *MemoryStore) WorkflowSnapshots() WorkflowSnapshotRepository { return s }

// Migrate is a no-op: MemoryStore has no external schema.
func (s *MemoryStore) Migrate(context.Context) error { return nil }

// Transaction runs fn against an isolated copy and commits it only if fn
// succeeds. The callback must not retain its repositories after it returns.
func (s *MemoryStore) Transaction(ctx context.Context, fn func(context.Context, Repositories) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := &memoryRepositories{state: cloneMemoryState(s.state)}
	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.state = tx.state
	return nil
}

func (s *MemoryStore) CreateThread(ctx context.Context, record ThreadRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return createThread(ctx, &s.state, record)
}
func (s *MemoryStore) GetThread(ctx context.Context, id ThreadID) (ThreadRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return getThread(ctx, s.state, id)
}
func (s *MemoryStore) UpdateThread(ctx context.Context, record ThreadRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return updateThread(ctx, &s.state, record)
}
func (s *MemoryStore) AppendMessages(ctx context.Context, records []MessageRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return appendMessages(ctx, &s.state, records)
}
func (s *MemoryStore) ListMessages(ctx context.Context, id ThreadID, page PageRequest) (Page[MessageRecord], error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return listMessages(ctx, s.state, id, page)
}
func (s *MemoryStore) SaveWorkflowRun(ctx context.Context, record WorkflowRunRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return saveWorkflowRun(ctx, &s.state, record)
}
func (s *MemoryStore) GetWorkflowRun(ctx context.Context, id RunID) (WorkflowRunRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return getWorkflowRun(ctx, s.state, id)
}
func (s *MemoryStore) SaveWorkflowSnapshot(ctx context.Context, record WorkflowSnapshotRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return saveWorkflowSnapshot(ctx, &s.state, record)
}
func (s *MemoryStore) ListWorkflowSnapshots(ctx context.Context, id RunID, page PageRequest) (Page[WorkflowSnapshotRecord], error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return listWorkflowSnapshots(ctx, s.state, id, page)
}

type memoryRepositories struct{ state memoryState }

func (r *memoryRepositories) Threads() ThreadRepository                     { return r }
func (r *memoryRepositories) Messages() MessageRepository                   { return r }
func (r *memoryRepositories) WorkflowRuns() WorkflowRunRepository           { return r }
func (r *memoryRepositories) WorkflowSnapshots() WorkflowSnapshotRepository { return r }
func (r *memoryRepositories) CreateThread(ctx context.Context, v ThreadRecord) error {
	return createThread(ctx, &r.state, v)
}
func (r *memoryRepositories) GetThread(ctx context.Context, id ThreadID) (ThreadRecord, error) {
	return getThread(ctx, r.state, id)
}
func (r *memoryRepositories) UpdateThread(ctx context.Context, v ThreadRecord) error {
	return updateThread(ctx, &r.state, v)
}
func (r *memoryRepositories) AppendMessages(ctx context.Context, v []MessageRecord) error {
	return appendMessages(ctx, &r.state, v)
}
func (r *memoryRepositories) ListMessages(ctx context.Context, id ThreadID, p PageRequest) (Page[MessageRecord], error) {
	return listMessages(ctx, r.state, id, p)
}
func (r *memoryRepositories) SaveWorkflowRun(ctx context.Context, v WorkflowRunRecord) error {
	return saveWorkflowRun(ctx, &r.state, v)
}
func (r *memoryRepositories) GetWorkflowRun(ctx context.Context, id RunID) (WorkflowRunRecord, error) {
	return getWorkflowRun(ctx, r.state, id)
}
func (r *memoryRepositories) SaveWorkflowSnapshot(ctx context.Context, v WorkflowSnapshotRecord) error {
	return saveWorkflowSnapshot(ctx, &r.state, v)
}
func (r *memoryRepositories) ListWorkflowSnapshots(ctx context.Context, id RunID, p PageRequest) (Page[WorkflowSnapshotRecord], error) {
	return listWorkflowSnapshots(ctx, r.state, id, p)
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
	if _, ok := s.threads[v.ID]; ok {
		return fmt.Errorf("lebro: thread %q already exists", v.ID)
	}
	s.threads[v.ID] = clone(v)
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
	return clone(v), nil
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
	s.threads[v.ID] = clone(v)
	return nil
}
func appendMessages(ctx context.Context, s *memoryState, vs []MessageRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	seen := make(map[ThreadID]map[string]struct{}, len(vs))
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
		for _, existing := range s.messages[v.ThreadID] {
			if existing.ID == v.ID {
				return fmt.Errorf("lebro: message %q already exists", v.ID)
			}
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
		s.messages[v.ThreadID] = append(s.messages[v.ThreadID], clone(v))
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
	return paginate(s.messages[id], p), nil
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
	s.runs[v.ID] = clone(v)
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
	return clone(v), nil
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
	if _, ok := s.runs[v.RunID]; !ok {
		return ErrNotFound
	}
	for _, e := range s.snapshots[v.RunID] {
		if e.ID == v.ID || e.Sequence == v.Sequence {
			return fmt.Errorf("lebro: workflow snapshot already exists")
		}
	}
	s.snapshots[v.RunID] = append(s.snapshots[v.RunID], clone(v))
	sort.SliceStable(s.snapshots[v.RunID], func(i, j int) bool { return s.snapshots[v.RunID][i].Sequence < s.snapshots[v.RunID][j].Sequence })
	return nil
}
func listWorkflowSnapshots(ctx context.Context, s memoryState, id RunID, p PageRequest) (Page[WorkflowSnapshotRecord], error) {
	if err := ctx.Err(); err != nil {
		return Page[WorkflowSnapshotRecord]{}, err
	}
	if _, ok := s.runs[id]; !ok {
		return Page[WorkflowSnapshotRecord]{}, ErrNotFound
	}
	return paginate(s.snapshots[id], p), nil
}

func paginate[T any](records []T, request PageRequest) Page[T] {
	start, _ := strconv.Atoi(request.Cursor)
	if start < 0 {
		start = 0
	}
	limit := request.Limit
	if limit <= 0 {
		limit = 100
	}
	if start >= len(records) {
		return Page[T]{Records: []T{}}
	}
	end := start + limit
	if end > len(records) {
		end = len(records)
	}
	page := Page[T]{Records: make([]T, 0, end-start)}
	for _, record := range records[start:end] {
		page.Records = append(page.Records, clone(record))
	}
	if end < len(records) {
		page.NextCursor = strconv.Itoa(end)
	}
	return page
}
func cloneMemoryState(s memoryState) memoryState {
	out := newMemoryState()
	for k, v := range s.threads {
		out.threads[k] = clone(v)
	}
	for k, v := range s.messages {
		out.messages[k] = append([]MessageRecord(nil), v...)
		for i := range out.messages[k] {
			out.messages[k][i] = clone(out.messages[k][i])
		}
	}
	for k, v := range s.runs {
		out.runs[k] = clone(v)
	}
	for k, v := range s.snapshots {
		out.snapshots[k] = append([]WorkflowSnapshotRecord(nil), v...)
		for i := range out.snapshots[k] {
			out.snapshots[k][i] = clone(out.snapshots[k][i])
		}
	}
	return out
}
func clone[T any](v T) T {
	bytes, _ := json.Marshal(v)
	var copy T
	_ = json.Unmarshal(bytes, &copy)
	return copy
}

func validateJSON(value json.RawMessage) error {
	if len(value) == 0 || json.Valid(value) {
		return nil
	}
	return errors.New("must be valid JSON")
}
