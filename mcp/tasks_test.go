package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
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

type failingReadStore struct{ *memoryTaskStore }

func (s *failingReadStore) GetTask(context.Context, string) (TaskRecord, error) {
	return TaskRecord{}, errors.New("store down")
}

// flakyUpdateStore fails record writes once armed, simulating a store that
// accepts the claim but then refuses lease renewals.
type flakyUpdateStore struct {
	*memoryTaskStore
	failUpdates atomic.Bool
}

func (s *flakyUpdateStore) UpdateTask(ctx context.Context, record TaskRecord) error {
	if s.failUpdates.Load() {
		return errors.New("store write down")
	}
	return s.memoryTaskStore.UpdateTask(ctx, record)
}

// postClaimReadFailureStore fails reads once a claim is recorded, simulating a
// store that goes down between the claim write and the post-claim state read.
type postClaimReadFailureStore struct {
	*memoryTaskStore
	claimed atomic.Bool
}

func (s *postClaimReadFailureStore) UpdateTask(ctx context.Context, record TaskRecord) error {
	err := s.memoryTaskStore.UpdateTask(ctx, record)
	if err == nil && record.Status == TaskWorking && !record.LeaseUntil.IsZero() {
		s.claimed.Store(true)
	}
	return err
}

func (s *postClaimReadFailureStore) GetTask(ctx context.Context, id string) (TaskRecord, error) {
	if s.claimed.Load() {
		return TaskRecord{}, errors.New("post-claim read down")
	}
	return s.memoryTaskStore.GetTask(ctx, id)
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

func TestTaskClaimLeasePreventsDoubleExecution(t *testing.T) {
	store := newMemoryTaskStore()
	now := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	var clock time.Time
	clock = now
	tasks := newTasks(store, func() time.Time { return clock }, func(context.Context, TaskRecord) error { return nil })
	record := TaskRecord{ID: "task-1", EntryID: "agent.a", Status: TaskWorking, CreatedAt: now, LastUpdatedAt: now, Version: 1}
	if err := store.CreateTask(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	won, err := tasks.claim(context.Background(), record)
	if err != nil || !won {
		t.Fatalf("first claim: won=%v err=%v", won, err)
	}
	held, err := store.GetTask(context.Background(), "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if !tasks.leaseHeld(held) {
		t.Fatal("lease not recorded")
	}
	if won, err := tasks.claim(context.Background(), held); won || err != nil {
		t.Fatalf("second claim: won=%v err=%v", won, err)
	}
	clock = now.Add(31 * time.Second)
	held, err = store.GetTask(context.Background(), "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if won, err := tasks.claim(context.Background(), held); !won || err != nil {
		t.Fatalf("claim after lease expiry: won=%v err=%v", won, err)
	}
}

func TestTaskExecutionClaimFailureNotifiesConflict(t *testing.T) {
	tasks := newTasks(&failingReadStore{newMemoryTaskStore()}, time.Now, func(context.Context, TaskRecord) error { return nil })
	observed := make(chan string, 1)
	tasks.config.OnConflict = func(_ context.Context, record TaskRecord) { observed <- record.ID }
	tasks.execute(TaskRecord{ID: "task-1", EntryID: "agent.a", Status: TaskWorking}, taskEntry{run: func(context.Context, json.RawMessage) (*mcpsdk.CallToolResult, error) { return nil, nil }})
	select {
	case id := <-observed:
		if id != "task-1" {
			t.Fatalf("observed = %q", id)
		}
	case <-time.After(time.Second):
		t.Fatal("OnConflict not called")
	}
}

func TestTaskLeaseRenewsWhileRunInFlight(t *testing.T) {
	store := newMemoryTaskStore()
	tasks := newTasks(store, time.Now, func(context.Context, TaskRecord) error { return nil })
	tasks.config.Lease = 30 * time.Millisecond
	release := make(chan struct{})
	started := make(chan struct{})
	if _, err := tasks.create(context.Background(), "agent.a", json.RawMessage(`{}`), taskEntry{run: func(context.Context, json.RawMessage) (*mcpsdk.CallToolResult, error) {
		close(started)
		<-release
		return &mcpsdk.CallToolResult{}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	<-started
	pollDeadline := time.Now().Add(time.Second)
	for {
		record, err := store.GetTask(context.Background(), "task-1")
		if err != nil {
			t.Fatal(err)
		}
		if record.LeaseUntil.After(time.Now()) {
			break
		}
		if time.Now().After(pollDeadline) {
			t.Fatalf("lease was not renewed: %v", record.LeaseUntil)
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	deadline := time.After(time.Second)
	for {
		record, err := store.GetTask(context.Background(), "task-1")
		if err != nil {
			t.Fatal(err)
		}
		if record.Status.terminal() {
			if !record.LeaseUntil.IsZero() {
				t.Fatal("terminal record still leased")
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("task did not finish")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestServerRecoverTasksRelaunchesWorkingRecord(t *testing.T) {
	store := newMemoryTaskStore()
	now := time.Now()
	done := make(chan struct{})
	server := NewServer(ServerConfig{Implementation: &mcpsdk.Implementation{Name: "t", Version: "1"}, Tasks: &TaskConfig{Store: store, Identity: func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }, Context: func(ctx context.Context, _ TaskRecord) (context.Context, error) { return ctx, nil }, Authorize: func(context.Context, TaskRecord) error { return nil }, PollInterval: time.Millisecond}})
	if err := server.tasks.register("agent.a", taskEntry{run: func(context.Context, json.RawMessage) (*mcpsdk.CallToolResult, error) {
		defer close(done)
		return &mcpsdk.CallToolResult{}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTask(context.Background(), TaskRecord{ID: "task-1", EntryID: "agent.a", Status: TaskWorking, CreatedAt: now, LastUpdatedAt: now, Version: 1}); err != nil {
		t.Fatal(err)
	}
	if err := server.RecoverTasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("recovered run did not start")
	}
	if err := NewServer(ServerConfig{Implementation: &mcpsdk.Implementation{Name: "t", Version: "1"}}).RecoverTasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	bare := NewServer(ServerConfig{Implementation: &mcpsdk.Implementation{Name: "t", Version: "1"}, Tasks: &TaskConfig{Store: noListStore{}, Identity: func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }, Context: func(ctx context.Context, _ TaskRecord) (context.Context, error) { return ctx, nil }, Authorize: func(context.Context, TaskRecord) error { return nil }}})
	if err := bare.RecoverTasks(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type noListStore struct{}

func (noListStore) CreateTask(context.Context, TaskRecord) error { return nil }
func (noListStore) GetTask(context.Context, string) (TaskRecord, error) {
	return TaskRecord{}, ErrTaskNotFound
}
func (noListStore) UpdateTask(context.Context, TaskRecord) error { return nil }

// TestTaskLeaseLossCancelsRun proves a run whose lease renewals persistently
// fail is cancelled instead of racing a peer that re-claims the record: two
// consecutive unreachable ticks span a full lease.
func TestTaskLeaseLossCancelsRun(t *testing.T) {
	store := &flakyUpdateStore{memoryTaskStore: newMemoryTaskStore()}
	tasks := newTasks(store, time.Now, func(context.Context, TaskRecord) error { return nil })
	tasks.config.Lease = 20 * time.Millisecond
	observed := make(chan string, 1)
	tasks.config.OnConflict = func(_ context.Context, record TaskRecord) { observed <- record.ID }
	started := make(chan struct{})
	cancelled := make(chan struct{})
	if _, err := tasks.create(context.Background(), "agent.a", json.RawMessage(`{}`), taskEntry{run: func(ctx context.Context, _ json.RawMessage) (*mcpsdk.CallToolResult, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	}}); err != nil {
		t.Fatal(err)
	}
	<-started
	store.failUpdates.Store(true)
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("lease loss did not cancel the run")
	}
	select {
	case id := <-observed:
		if id != "task-1" {
			t.Fatalf("observed = %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnConflict not called after lease loss")
	}
}

// TestTaskPeerCancellationStopsLocalRun proves a cancellation persisted by
// another instance reaches this process's executing run within one renewal
// tick, without any local cancel signal.
func TestTaskPeerCancellationStopsLocalRun(t *testing.T) {
	store := newMemoryTaskStore()
	tasks := newTasks(store, time.Now, func(context.Context, TaskRecord) error { return nil })
	tasks.config.Lease = 20 * time.Millisecond
	started := make(chan struct{})
	cancelled := make(chan struct{})
	if _, err := tasks.create(context.Background(), "agent.a", json.RawMessage(`{}`), taskEntry{run: func(ctx context.Context, _ json.RawMessage) (*mcpsdk.CallToolResult, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	}}); err != nil {
		t.Fatal(err)
	}
	<-started
	// Act as the peer: persist the cancellation from outside the executing
	// process, retrying past the local renewer's concurrent lease writes.
	go func() {
		for {
			record, err := store.GetTask(context.Background(), "task-1")
			if err != nil {
				continue
			}
			record.Status, record.StatusMessage, record.LastUpdatedAt = TaskCancelled, "cancelled", time.Now().UTC()
			if err := store.UpdateTask(context.Background(), record); err == nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("peer cancellation did not stop the local run")
	}
}

// TestRecoverTasksBoundsConcurrentRuns proves recovery never launches more
// than maxConcurrentRecoveredTasks runs at once, while still running every
// recovered record eventually.
func TestRecoverTasksBoundsConcurrentRuns(t *testing.T) {
	store := newMemoryTaskStore()
	now := time.Now()
	const total = maxConcurrentRecoveredTasks * 2
	for i := 0; i < total; i++ {
		id := "task-" + strconv.Itoa(i)
		if err := store.CreateTask(context.Background(), TaskRecord{ID: id, EntryID: "agent.a", Status: TaskWorking, CreatedAt: now, LastUpdatedAt: now, Version: 1}); err != nil {
			t.Fatal(err)
		}
	}
	server := NewServer(ServerConfig{Implementation: &mcpsdk.Implementation{Name: "t", Version: "1"}, Tasks: &TaskConfig{Store: store, Identity: func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }, Context: func(ctx context.Context, _ TaskRecord) (context.Context, error) { return ctx, nil }, Authorize: func(context.Context, TaskRecord) error { return nil }, PollInterval: time.Millisecond}})
	var started, completed atomic.Int64
	gate := make(chan struct{})
	if err := server.tasks.register("agent.a", taskEntry{run: func(context.Context, json.RawMessage) (*mcpsdk.CallToolResult, error) {
		started.Add(1)
		<-gate
		completed.Add(1)
		return &mcpsdk.CallToolResult{}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := server.RecoverTasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for started.Load() < maxConcurrentRecoveredTasks {
		select {
		case <-deadline:
			t.Fatalf("started = %d, want %d", started.Load(), maxConcurrentRecoveredTasks)
		case <-time.After(time.Millisecond):
		}
	}
	time.Sleep(50 * time.Millisecond)
	if got := started.Load(); got != maxConcurrentRecoveredTasks {
		t.Fatalf("recovery launched %d concurrent runs, want bound %d", got, maxConcurrentRecoveredTasks)
	}
	close(gate)
	deadline = time.After(5 * time.Second)
	for completed.Load() < total {
		select {
		case <-deadline:
			t.Fatalf("completed = %d, want %d", completed.Load(), total)
		case <-time.After(time.Millisecond):
		}
	}
}

// TestRecoverTasksConcurrentIsRaceFree pins that concurrent RecoverTasks
// calls share the pre-initialized recovery semaphore without a data race, and
// that the recovered entry actually runs exactly once. Both callers are
// released from a barrier so their recoverWorking calls overlap the sensitive
// window instead of running sequentially.
func TestRecoverTasksConcurrentIsRaceFree(t *testing.T) {
	store := newMemoryTaskStore()
	now := time.Now()
	if err := store.CreateTask(context.Background(), TaskRecord{ID: "task-1", EntryID: "agent.a", Status: TaskWorking, CreatedAt: now, LastUpdatedAt: now, Version: 1}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(ServerConfig{Implementation: &mcpsdk.Implementation{Name: "t", Version: "1"}, Tasks: &TaskConfig{Store: store, Identity: func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }, Context: func(ctx context.Context, _ TaskRecord) (context.Context, error) { return ctx, nil }, Authorize: func(context.Context, TaskRecord) error { return nil }, PollInterval: time.Millisecond}})
	var runs atomic.Int64
	if err := server.tasks.register("agent.a", taskEntry{run: func(context.Context, json.RawMessage) (*mcpsdk.CallToolResult, error) {
		runs.Add(1)
		return &mcpsdk.CallToolResult{}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	barrier := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-barrier
			if err := server.RecoverTasks(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	close(barrier)
	wg.Wait()
	deadline := time.After(2 * time.Second)
	for runs.Load() < 1 {
		select {
		case <-deadline:
			t.Fatal("recovered entry never ran")
		case <-time.After(time.Millisecond):
		}
	}
	time.Sleep(20 * time.Millisecond)
	if got := runs.Load(); got != 1 {
		t.Fatalf("recovered entry ran %d times, want exactly 1", got)
	}
}

// TestTaskPostClaimReadFailureNotifiesConflict proves a store read that fails
// after the claim write is not silently dropped: OnConflict fires so the
// application can reconcile the leased, unexecuted record, with a context
// that is still usable for store and network work.
func TestTaskPostClaimReadFailureNotifiesConflict(t *testing.T) {
	store := &postClaimReadFailureStore{memoryTaskStore: newMemoryTaskStore()}
	tasks := newTasks(store, time.Now, func(context.Context, TaskRecord) error { return nil })
	type observation struct {
		id      string
		ctxLive bool
	}
	observed := make(chan observation, 1)
	tasks.config.OnConflict = func(ctx context.Context, record TaskRecord) {
		observed <- observation{id: record.ID, ctxLive: ctx.Err() == nil}
	}
	// The record must exist so the claim succeeds and only the post-claim
	// read fails; otherwise OnConflict fires from the claim path instead.
	now := time.Now()
	if err := store.CreateTask(context.Background(), TaskRecord{ID: "task-1", EntryID: "agent.a", Status: TaskWorking, CreatedAt: now, LastUpdatedAt: now, Version: 1}); err != nil {
		t.Fatal(err)
	}
	tasks.execute(TaskRecord{ID: "task-1", EntryID: "agent.a", Status: TaskWorking}, taskEntry{run: func(context.Context, json.RawMessage) (*mcpsdk.CallToolResult, error) { return nil, nil }})
	select {
	case o := <-observed:
		if o.id != "task-1" {
			t.Fatalf("observed = %q", o.id)
		}
		if !o.ctxLive {
			t.Fatal("OnConflict must receive a context that is not cancelled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnConflict not called after post-claim read failure")
	}
}

// TestTaskStoreDownReturnsGenericInternalError proves a store outage during
// tasks/get, tasks/update, or tasks/cancel is reported as a generic JSON-RPC
// internal error, not as a leaked store error.
func TestTaskStoreDownReturnsGenericInternalError(t *testing.T) {
	tasks := newTasks(&failingReadStore{newMemoryTaskStore()}, time.Now, func(context.Context, TaskRecord) error { return nil })
	calls := map[string]func() error{
		"get": func() error {
			_, err := tasks.get(context.Background(), nil, &taskParams{ParamsBase: taskMeta(), TaskID: "task-1"})
			return err
		},
		"update": func() error {
			_, err := tasks.update(context.Background(), nil, &taskUpdateParams{ParamsBase: taskMeta(), TaskID: "task-1"})
			return err
		},
		"cancel": func() error {
			_, err := tasks.cancel(context.Background(), nil, &taskParams{ParamsBase: taskMeta(), TaskID: "task-1"})
			return err
		},
	}
	for name, call := range calls {
		err := call()
		var protocol *mcpjsonrpc.Error
		if !errors.As(err, &protocol) || protocol.Code != -32603 {
			t.Fatalf("%s: want generic -32603, got %#v", name, err)
		}
	}
}
