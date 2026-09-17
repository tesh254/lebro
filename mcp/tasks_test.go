package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	mcpjsonrpc "github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type memoryTaskStore struct {
	mu      sync.Mutex
	records map[string]TaskRecord
}

func newMemoryTaskStore() *memoryTaskStore {
	return &memoryTaskStore{records: make(map[string]TaskRecord)}
}
func (s *memoryTaskStore) CreateTask(_ context.Context, record TaskRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[record.ID]; ok {
		return ErrTaskConflict
	}
	s.records[record.ID] = cloneTask(record)
	return nil
}
func (s *memoryTaskStore) GetTask(_ context.Context, id string) (TaskRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[id]
	if !ok {
		return TaskRecord{}, ErrTaskNotFound
	}
	return cloneTask(record), nil
}
func (s *memoryTaskStore) UpdateTask(_ context.Context, record TaskRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.records[record.ID]
	if !ok {
		return ErrTaskNotFound
	}
	if current.Version != record.Version {
		return ErrTaskConflict
	}
	record.Version++
	s.records[record.ID] = cloneTask(record)
	return nil
}

func (s *memoryTaskStore) ListWorkingTasks(_ context.Context) ([]TaskRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var working []TaskRecord
	for _, record := range s.records {
		if record.Status == TaskWorking {
			working = append(working, cloneTask(record))
		}
	}
	return working, nil
}

func cloneTask(record TaskRecord) TaskRecord {
	record.Arguments = cloneRaw(record.Arguments)
	record.Identity = cloneRaw(record.Identity)
	record.Failure = cloneRaw(record.Failure)
	if record.Result != nil {
		copied := *record.Result
		record.Result = &copied
	}
	if record.TTLMs != nil {
		ttl := *record.TTLMs
		record.TTLMs = &ttl
	}
	return record
}

func newTasks(store TaskStore, now func() time.Time, authorize func(context.Context, TaskRecord) error) *taskService {
	return newTaskService(&TaskConfig{Store: store, Identity: func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{"subject":"ava"}`), nil }, Context: func(ctx context.Context, record TaskRecord) (context.Context, error) {
		return context.WithValue(ctx, taskIdentityKey{}, string(record.Identity)), nil
	}, Authorize: authorize, NewID: func() (string, error) { return "task-1", nil }, Now: now, PollInterval: time.Millisecond})
}

type taskIdentityKey struct{}

func taskMeta() mcpsdk.ParamsBase {
	return mcpsdk.ParamsBase{Meta: mcpsdk.Meta{"io.modelcontextprotocol/clientCapabilities": map[string]any{"extensions": map[string]any{tasksExtension: map[string]any{}}}}}
}

func TestTaskCompletesAndKeepsResult(t *testing.T) {
	store := newMemoryTaskStore()
	now := time.Now
	tasks := newTasks(store, now, func(ctx context.Context, record TaskRecord) error {
		if got := ctx.Value(taskIdentityKey{}); got != nil && got != string(record.Identity) {
			return errors.New("wrong caller")
		}
		return nil
	})
	done := make(chan struct{})
	entry := taskEntry{run: func(ctx context.Context, arguments json.RawMessage) (*mcpsdk.CallToolResult, error) {
		defer close(done)
		if got := ctx.Value(taskIdentityKey{}); got != `{"subject":"ava"}` {
			t.Errorf("identity = %v", got)
		}
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: string(arguments)}}}, nil
	}}
	result, err := tasks.create(context.Background(), "agent.a", json.RawMessage(`{"messages":[]}`), entry)
	if err != nil {
		t.Fatal(err)
	}
	created := result.(*taskResult)
	if created.ResultType != "task" || created.Status != TaskWorking {
		t.Fatalf("created = %#v", created)
	}
	<-done
	deadline := time.After(time.Second)
	for {
		record, err := tasks.loadAuthorized(context.WithValue(context.Background(), taskIdentityKey{}, `{"subject":"ava"}`), "task-1")
		if err != nil {
			t.Fatal(err)
		}
		if record.Status == TaskCompleted {
			if record.Result == nil || len(record.Result.Content) != 1 {
				t.Fatalf("result = %#v", record.Result)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("task did not finish")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestTaskCancelAndRevokedAccess(t *testing.T) {
	store := newMemoryTaskStore()
	allow := true
	tasks := newTasks(store, time.Now, func(context.Context, TaskRecord) error {
		if !allow {
			return errors.New("revoked")
		}
		return nil
	})
	started := make(chan struct{})
	cancelled := make(chan struct{})
	_, err := tasks.create(context.Background(), "workflow.a", json.RawMessage(`{}`), taskEntry{run: func(ctx context.Context, _ json.RawMessage) (*mcpsdk.CallToolResult, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := tasks.cancel(context.Background(), nil, &taskParams{ParamsBase: taskMeta(), TaskID: "task-1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("run did not observe cancellation")
	}
	deadline := time.After(time.Second)
	for {
		record, err := store.GetTask(context.Background(), "task-1")
		if err != nil {
			t.Fatal(err)
		}
		if record.Status == TaskCancelled {
			break
		}
		select {
		case <-deadline:
			t.Fatal("task not cancelled")
		case <-time.After(time.Millisecond):
		}
	}
	allow = false
	if _, err := tasks.get(context.Background(), nil, &taskParams{ParamsBase: taskMeta(), TaskID: "task-1"}); err == nil {
		t.Fatal("revoked get succeeded")
	}
}

func TestTaskMethodsRequireNegotiatedCapability(t *testing.T) {
	store := newMemoryTaskStore()
	tasks := newTasks(store, time.Now, func(context.Context, TaskRecord) error { return nil })
	_, err := tasks.get(context.Background(), nil, &taskParams{TaskID: "missing"})
	var protocol *mcpjsonrpc.Error
	if !errors.As(err, &protocol) || protocol.Code != -32021 {
		t.Fatalf("error = %#v", err)
	}
}

func TestTaskPanicRecordsStableFailure(t *testing.T) {
	store := newMemoryTaskStore()
	tasks := newTasks(store, time.Now, func(context.Context, TaskRecord) error { return nil })
	if _, err := tasks.create(context.Background(), "agent.panic", json.RawMessage(`{}`), taskEntry{run: func(context.Context, json.RawMessage) (*mcpsdk.CallToolResult, error) { panic("model panic") }}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	for {
		record, err := store.GetTask(context.Background(), "task-1")
		if err != nil {
			t.Fatal(err)
		}
		if record.Status == TaskFailed {
			if string(record.Failure) != `{"code":-32603,"message":"task execution failed"}` {
				t.Fatalf("failure = %s", record.Failure)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("task did not fail")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestTaskExpiryAndRestartRecovery(t *testing.T) {
	store := newMemoryTaskStore()
	now := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	tasks := newTasks(store, clock, func(context.Context, TaskRecord) error { return nil })
	tasks.config.TTL = time.Minute
	if err := store.CreateTask(context.Background(), TaskRecord{ID: "recovered", EntryID: "workflow.a", Status: TaskCompleted, CreatedAt: now, LastUpdatedAt: now, Version: 1}); err != nil {
		t.Fatal(err)
	}
	restarted := newTasks(store, clock, func(context.Context, TaskRecord) error { return nil })
	if _, err := restarted.get(context.Background(), nil, &taskParams{ParamsBase: taskMeta(), TaskID: "recovered"}); err != nil {
		t.Fatalf("restart get: %v", err)
	}
	if err := store.CreateTask(context.Background(), TaskRecord{ID: "task-1", EntryID: "workflow.a", Status: TaskWorking, CreatedAt: now, LastUpdatedAt: now, TTLMs: func() *int64 { ttl := int64(time.Minute / time.Millisecond); return &ttl }(), Version: 1}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := tasks.get(context.Background(), nil, &taskParams{ParamsBase: taskMeta(), TaskID: "task-1"}); err == nil {
		t.Fatalf("expiry error = %v", err)
	}
}

func TestTaskRunFailureRecordsFailedStatus(t *testing.T) {
	store := newMemoryTaskStore()
	tasks := newTasks(store, time.Now, func(context.Context, TaskRecord) error { return nil })
	if _, err := tasks.create(context.Background(), "agent.a", json.RawMessage(`{}`), taskEntry{run: func(context.Context, json.RawMessage) (*mcpsdk.CallToolResult, error) {
		return nil, errors.New("model exploded")
	}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	for {
		record, err := store.GetTask(context.Background(), "task-1")
		if err != nil {
			t.Fatal(err)
		}
		if record.Status == TaskFailed {
			if record.StatusMessage != "task execution failed" {
				t.Fatalf("status message = %q", record.StatusMessage)
			}
			if string(record.Failure) != `{"code":-32603,"message":"task execution failed"}` {
				t.Fatalf("failure = %s", record.Failure)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("task status = %q, want failed", record.Status)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestTaskUpdateAcknowledgement(t *testing.T) {
	store := newMemoryTaskStore()
	tasks := newTasks(store, time.Now, func(context.Context, TaskRecord) error { return nil })
	done := make(chan struct{})
	if _, err := tasks.create(context.Background(), "agent.a", json.RawMessage(`{}`), taskEntry{run: func(context.Context, json.RawMessage) (*mcpsdk.CallToolResult, error) {
		defer close(done)
		return &mcpsdk.CallToolResult{}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	<-done
	ack, err := tasks.update(context.Background(), nil, &taskUpdateParams{ParamsBase: taskMeta(), TaskID: "task-1", InputResponses: mcpsdk.InputResponseMap{"req-1": &mcpsdk.ElicitResult{Action: "accept"}}})
	if err != nil {
		t.Fatal(err)
	}
	if ack.ResultType != "complete" {
		t.Fatalf("ack = %#v", ack)
	}
	if _, err := tasks.update(context.Background(), nil, &taskUpdateParams{ParamsBase: taskMeta(), TaskID: "missing"}); err == nil {
		t.Fatal("update for missing task succeeded")
	}
}

func TestTaskRestartRecoveryResumesWorkingRun(t *testing.T) {
	store := newMemoryTaskStore()
	now := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	if err := store.CreateTask(context.Background(), TaskRecord{ID: "resumable", EntryID: "agent.a", Status: TaskWorking, CreatedAt: now, LastUpdatedAt: now, Version: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTask(context.Background(), TaskRecord{ID: "orphaned", EntryID: "agent.gone", Status: TaskWorking, CreatedAt: now, LastUpdatedAt: now, Version: 1}); err != nil {
		t.Fatal(err)
	}
	restarted := newTasks(store, clock, func(context.Context, TaskRecord) error { return nil })
	done := make(chan struct{})
	if err := restarted.register("agent.a", taskEntry{run: func(ctx context.Context, arguments json.RawMessage) (*mcpsdk.CallToolResult, error) {
		defer close(done)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: string(arguments)}}}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := restarted.recoverWorking(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("recovered run did not start")
	}
	deadline := time.After(time.Second)
	for {
		record, err := store.GetTask(context.Background(), "resumable")
		if err != nil {
			t.Fatal(err)
		}
		if record.Status == TaskCompleted {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("recovered status = %q", record.Status)
		case <-time.After(time.Millisecond):
		}
	}
	orphaned, err := store.GetTask(context.Background(), "orphaned")
	if err != nil {
		t.Fatal(err)
	}
	if orphaned.Status != TaskWorking {
		t.Fatalf("orphaned status = %q, want working", orphaned.Status)
	}
}
