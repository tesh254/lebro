package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	mcpjsonrpc "github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const tasksExtension = "io.modelcontextprotocol/tasks"

const (
	// taskResultTypeTask is the resultType of task envelopes that carry the
	// task record (create and get).
	taskResultTypeTask = "task"
	// taskResultTypeComplete is the resultType of acknowledgements (update
	// and cancel) and terminal get responses.
	taskResultTypeComplete = "complete"
)

// taskRetryBackoff spaces out store retries inside claim and extendLease so a
// persistently failing store is not hammered in a tight loop.
const taskRetryBackoff = 50 * time.Millisecond

// maxConcurrentRecoveredTasks bounds how many recovered runs execute
// concurrently after a restart. A store holding thousands of stale working
// records otherwise launches every one of them against the application's Run
// adapters at boot. Records beyond the bound wait on a slot; nothing is
// dropped.
const maxConcurrentRecoveredTasks = 32

var (
	// ErrTaskNotFound is returned when a task is absent or its retention period elapsed.
	ErrTaskNotFound = errors.New("lebro/mcp: task not found")
	// ErrTaskConflict is returned when a task update loses a concurrent state transition.
	ErrTaskConflict          = errors.New("lebro/mcp: task conflict")
	errTaskExecutionPanicked = errors.New("lebro/mcp: task execution panicked")
)

// TaskStatus is an MCP Tasks lifecycle state.
type TaskStatus string

const (
	TaskWorking   TaskStatus = "working"
	TaskCompleted TaskStatus = "completed"
	TaskCancelled TaskStatus = "cancelled"
	TaskFailed    TaskStatus = "failed"
)

func (s TaskStatus) terminal() bool {
	return s == TaskCompleted || s == TaskCancelled || s == TaskFailed
}

// TaskRecord is durable state owned by a TaskStore. Identity is opaque
// application data: store enough to reconstruct authorization context after a
// process restart, never raw credentials.
type TaskRecord struct {
	ID             string                 `json:"taskId"`
	EntryID        string                 `json:"-"`
	Arguments      json.RawMessage        `json:"-"`
	Identity       json.RawMessage        `json:"-"`
	Status         TaskStatus             `json:"status"`
	StatusMessage  string                 `json:"statusMessage,omitempty"`
	Result         *mcpsdk.CallToolResult `json:"result,omitempty"`
	Failure        json.RawMessage        `json:"error,omitempty"`
	CreatedAt      time.Time              `json:"createdAt"`
	LastUpdatedAt  time.Time              `json:"lastUpdatedAt"`
	TTLMs          *int64                 `json:"ttlMs"`
	PollIntervalMs int64                  `json:"pollIntervalMs"`
	// LeaseUntil bounds the execution claim held by the process running the
	// task; the zero time means unclaimed. Stores should persist it so
	// concurrent server instances honor the claim. Not exposed to MCP clients.
	LeaseUntil time.Time `json:"-"`
	Version    int64     `json:"-"`
}

// TaskStore persists task identity and terminal results. Update must reject a
// stale Version with ErrTaskConflict, making cancellation and completion safe
// across server instances.
type TaskStore interface {
	CreateTask(context.Context, TaskRecord) error
	GetTask(context.Context, string) (TaskRecord, error)
	UpdateTask(context.Context, TaskRecord) error
}

// WorkingTaskLister is an optional TaskStore capability for restart recovery.
// When the store implements it, Server.RecoverTasks re-launches execution for
// records that were still working when the previous process stopped.
type WorkingTaskLister interface {
	ListWorkingTasks(context.Context) ([]TaskRecord, error)
}

// TaskConfig connects MCP task lifecycle to application-owned durability and
// authorization. Identity and Context restore caller identity for background
// execution; Authorize runs on every get, update, and cancel request.
type TaskConfig struct {
	Store     TaskStore
	Identity  func(context.Context) (json.RawMessage, error)
	Context   func(context.Context, TaskRecord) (context.Context, error)
	Authorize func(context.Context, TaskRecord) error
	// Cancel lets an application cancel a mapped durable run after a restart.
	// It is called after cancellation is durably recorded.
	Cancel       func(context.Context, TaskRecord) error
	NewID        func() (string, error)
	Now          func() time.Time
	TTL          time.Duration
	PollInterval time.Duration
	// SyncTimeout bounds fallback calls for optional entries when a client did
	// not negotiate Tasks. Zero leaves fallback calls unbounded.
	SyncTimeout time.Duration
	// Lease bounds the execution claim a process holds on a working task so
	// concurrent server instances never run the same recovered task twice.
	// The executing process renews the claim while its run is in flight. Zero
	// uses 30 seconds.
	Lease time.Duration
	// OnConflict observes a completion or failure that could not be persisted
	// after retries, including non-conflict store errors. Applications should
	// alert and reconcile the durable run.
	OnConflict func(context.Context, TaskRecord)
}

func (c *TaskConfig) validate() error {
	if c == nil || c.Store == nil {
		return errors.New("lebro/mcp: Tasks.Store is required")
	}
	if c.Identity == nil || c.Context == nil || c.Authorize == nil {
		return errors.New("lebro/mcp: Tasks.Identity, Context, and Authorize are required")
	}
	if c.TTL < 0 || c.PollInterval < 0 || c.SyncTimeout < 0 || c.Lease < 0 {
		return errors.New("lebro/mcp: task TTL, poll interval, sync timeout, and lease must not be negative")
	}
	return nil
}

// AsyncEntryOptions marks one agent or workflow as a durable MCP Task entry.
// RequireTasks returns MCP error -32021 for clients that did not negotiate the
// Tasks extension; otherwise the configured entry remains synchronously callable.
type AsyncEntryOptions struct{ RequireTasks bool }

type taskEntry struct {
	require bool
	run     func(context.Context, json.RawMessage) (*mcpsdk.CallToolResult, error)
}

type taskService struct {
	config  *TaskConfig
	mu      sync.RWMutex
	entries map[string]taskEntry
	cancels map[string]context.CancelFunc
	// recoverySlots bounds concurrent recovered runs. It is created once in
	// newTaskService: a lazily initialized channel field would race under
	// concurrent RecoverTasks calls.
	recoverySlots chan struct{}
}

func newTaskService(config *TaskConfig) *taskService {
	return &taskService{
		config:        config,
		entries:       make(map[string]taskEntry),
		cancels:       make(map[string]context.CancelFunc),
		recoverySlots: make(chan struct{}, maxConcurrentRecoveredTasks),
	}
}

func (s *taskService) register(name string, entry taskEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entries[name]; exists {
		return fmt.Errorf("lebro/mcp: task entry %q is already exposed", name)
	}
	s.entries[name] = entry
	return nil
}

func (s *taskService) unregister(name string) { s.mu.Lock(); delete(s.entries, name); s.mu.Unlock() }

func (s *taskService) install(server *mcpsdk.Server) {
	server.AddReceivingMiddleware(s.middleware)
	mustTaskMethod(mcpsdk.AddReceivingCustomMethod(server, "tasks/get", s.get))
	mustTaskMethod(mcpsdk.AddReceivingCustomMethod(server, "tasks/update", s.update))
	mustTaskMethod(mcpsdk.AddReceivingCustomMethod(server, "tasks/cancel", s.cancel))
}

func mustTaskMethod(err error) {
	if err != nil {
		panic(fmt.Errorf("lebro/mcp: register tasks method: %w", err))
	}
}

func (s *taskService) middleware(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
	return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
		if method != "tools/call" {
			return next(ctx, method, req)
		}
		call, ok := req.(*mcpsdk.ServerRequest[*mcpsdk.CallToolParamsRaw])
		if !ok || call.Params == nil {
			return next(ctx, method, req)
		}
		s.mu.RLock()
		entry, async := s.entries[call.Params.Name]
		s.mu.RUnlock()
		if !async {
			return next(ctx, method, req)
		}
		if !tasksNegotiated(call.Params.Meta) {
			if entry.require {
				return nil, missingTasksCapability()
			}
			if s.config.SyncTimeout > 0 {
				bounded, cancel := context.WithTimeout(ctx, s.config.SyncTimeout)
				defer cancel()
				return next(bounded, method, req)
			}
			return next(ctx, method, req)
		}
		return s.create(ctx, call.Params.Name, call.Params.Arguments, entry)
	}
}

func tasksNegotiated(meta mcpsdk.Meta) bool {
	capabilities, ok := meta["io.modelcontextprotocol/clientCapabilities"].(map[string]any)
	if !ok {
		return false
	}
	extensions, ok := capabilities["extensions"].(map[string]any)
	if !ok {
		return false
	}
	_, ok = extensions[tasksExtension]
	return ok
}

func missingTasksCapability() error {
	data, _ := json.Marshal(map[string]any{"requiredCapabilities": map[string]any{"extensions": map[string]any{tasksExtension: map[string]any{}}}})
	return &mcpjsonrpc.Error{Code: -32021, Message: "Missing required client capability", Data: data}
}

func (s *taskService) create(ctx context.Context, entryID string, arguments json.RawMessage, entry taskEntry) (mcpsdk.Result, error) {
	identity, err := s.config.Identity(ctx)
	if err != nil {
		return nil, err
	}
	id, err := s.newID()
	if err != nil {
		return nil, err
	}
	now := s.now()
	record := TaskRecord{ID: id, EntryID: entryID, Arguments: cloneRaw(arguments), Identity: cloneRaw(identity), Status: TaskWorking, CreatedAt: now, LastUpdatedAt: now, PollIntervalMs: s.pollMs(), Version: 1}
	if s.config.TTL > 0 {
		ttl := s.config.TTL.Milliseconds()
		record.TTLMs = &ttl
	}
	if err := s.config.Authorize(ctx, record); err != nil {
		return nil, err
	}
	if err := s.config.Store.CreateTask(ctx, record); err != nil {
		return nil, fmt.Errorf("lebro/mcp: create task: %w", err)
	}
	// Creation is durable before this response. Do not inherit request cancellation.
	go s.execute(record, entry)
	return &taskResult{ResultBase: mcpsdk.ResultBase{}, ResultType: taskResultTypeTask, TaskRecord: record}, nil
}

// recoverWorking re-launches execution for tasks that were still working when
// the previous process stopped. Only records whose entry is exposed in this
// process are re-launched; a record left behind by an unexposed entry stays
// working until TTL expiry. execute claims each record before running, so
// concurrent server instances recovering the same record elect a single
// claimant and never run it concurrently.
func (s *taskService) recoverWorking(ctx context.Context) error {
	lister, ok := s.config.Store.(WorkingTaskLister)
	if !ok {
		return nil
	}
	records, err := lister.ListWorkingTasks(ctx)
	if err != nil {
		return fmt.Errorf("lebro/mcp: list working tasks: %w", err)
	}
	for _, record := range records {
		if s.expired(record) {
			continue
		}
		s.mu.RLock()
		entry, ok := s.entries[record.EntryID]
		s.mu.RUnlock()
		if !ok {
			continue
		}
		go func() {
			// The bounded slot wait happens on this goroutine, so
			// RecoverTasks itself is never delayed by recovery volume.
			s.recoverySlots <- struct{}{}
			defer func() { <-s.recoverySlots }()
			s.execute(record, entry)
		}()
	}
	return nil
}

func (s *taskService) execute(record TaskRecord, entry taskEntry) {
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancels[record.ID] = cancel
	s.mu.Unlock()
	defer func() { cancel(); s.mu.Lock(); delete(s.cancels, record.ID); s.mu.Unlock() }()
	won, err := s.claim(ctx, record)
	if err != nil {
		slog.Error("lebro/mcp: task execution claim failed", "task_id", record.ID)
		if s.config.OnConflict != nil {
			s.config.OnConflict(ctx, record)
		}
		return
	}
	if !won {
		return
	}
	renewDone := s.renewLease(ctx, record.ID, cancel)
	// stopRenewal joins the lease renewer before any terminal write: a
	// renewal tick landing between finish's read and write bumps the record
	// version and burns finish's conflict retries.
	stopRenewal := func() { cancel(); <-renewDone }
	current, err := s.config.Store.GetTask(ctx, record.ID)
	if err != nil {
		// The claim already wrote a lease; a failed re-read leaves the
		// record working with a live claim and nothing executing it. Say so.
		stopRenewal()
		slog.Error("lebro/mcp: task state read failed after claim", "task_id", record.ID, "error", err)
		if s.config.OnConflict != nil {
			// stopRenewal cancelled the run context; reconciliation needs a
			// context it can still use for store and network work.
			s.config.OnConflict(context.WithoutCancel(ctx), record)
		}
		return
	}
	if current.Status != TaskWorking {
		stopRenewal()
		return
	}
	ctx, err = s.config.Context(ctx, current)
	if err != nil {
		stopRenewal()
		// An application hook may return a nil context with its error; the
		// terminal write still needs a usable one.
		if ctx == nil {
			ctx = context.Background()
		}
		s.finish(ctx, current, nil, err, "restore task execution context failed")
		return
	}
	var result *mcpsdk.CallToolResult
	func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("lebro/mcp: task run panicked", "task_id", current.ID, "panic", r, "stack", string(debug.Stack()))
				err = errTaskExecutionPanicked
			}
		}()
		result, err = entry.run(ctx, cloneRaw(current.Arguments))
	}()
	stopRenewal()
	s.finish(ctx, current, result, err, "task execution failed")
}

// claim takes the execution lease for a working record. It returns true when
// this process may launch the task, false when another instance holds an
// unexpired claim or the record left the working state, and an error when the
// store keeps failing. The version conflict retry elects one claimant, so two
// instances recovering the same record never run it concurrently.
func (s *taskService) claim(ctx context.Context, record TaskRecord) (bool, error) {
	for attempts := 0; attempts < 3; attempts++ {
		current, err := s.config.Store.GetTask(ctx, record.ID)
		if err != nil {
			if attempts == 2 {
				return false, fmt.Errorf("lebro/mcp: claim task %s: %w", record.ID, err)
			}
			// Back off between attempts: retrying immediately against a
			// failing store just spins.
			select {
			case <-ctx.Done():
				return false, nil
			case <-time.After(taskRetryBackoff):
			}
			continue
		}
		if current.Status != TaskWorking || s.leaseHeld(current) {
			return false, nil
		}
		current.LeaseUntil = s.now().Add(s.lease())
		current.LastUpdatedAt = s.now()
		err = s.config.Store.UpdateTask(ctx, current)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, ErrTaskConflict) {
			return false, fmt.Errorf("lebro/mcp: claim task %s: %w", record.ID, err)
		}
	}
	return false, nil
}

// taskLeaseOutcome distinguishes the ways one renewal attempt can end. The
// renewer reacts differently to each: keep going, stop cleanly, or stop and
// cancel the run.
type taskLeaseOutcome int

const (
	leaseRenewed taskLeaseOutcome = iota
	// leaseTerminal means the record left the working state — completed,
	// failed, or cancelled, possibly by another instance. Renewal stops and
	// the local run is cancelled so it observes the durable decision.
	leaseTerminal
	// leaseUnreachable means the store could not confirm the extension this
	// tick. A single unreachable tick may be transient; the renewer counts
	// consecutive ones.
	leaseUnreachable
)

// renewLease keeps the execution claim alive while the run is in flight,
// extending it at half-lease intervals until the run's context ends. Without
// renewal, a long-running task would outlive its claim and become recoverable
// by another instance mid-run.
//
// Two consecutive unreachable ticks span a full lease with no confirmed
// extension, so the claim is dead and another instance may re-claim the
// record; the renewer cancels the run instead of racing it. A terminal record
// also cancels the run: this is how a cancellation recorded by any instance
// (local or peer) reaches the executing process within one renewal tick.
func (s *taskService) renewLease(ctx context.Context, id string, cancel context.CancelFunc) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		interval := s.lease() / 2
		if interval <= 0 {
			interval = time.Millisecond
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		unreachable := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				switch s.extendLease(id) {
				case leaseRenewed:
					unreachable = 0
				case leaseTerminal:
					slog.Info("lebro/mcp: task no longer working, cancelling run", "task_id", id)
					cancel()
					return
				case leaseUnreachable:
					unreachable++
					if unreachable >= 2 {
						slog.Error("lebro/mcp: task lease renewal lost, cancelling run", "task_id", id)
						cancel()
						return
					}
				}
			}
		}
	}()
	return done
}

// extendLease pushes the claim forward once. See taskLeaseOutcome for the
// outcomes. A non-conflict store failure is unreachable, not renewed: claiming
// success while the store refused the write would let the lease lapse under a
// running task.
func (s *taskService) extendLease(id string) taskLeaseOutcome {
	// A hung store must not consume the whole lease before the next tick
	// fires; bound the attempt window to a quarter of the lease.
	timeout := s.lease() / 4
	if timeout <= 0 {
		timeout = time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for attempts := 0; attempts < 3; attempts++ {
		current, err := s.config.Store.GetTask(ctx, id)
		if err != nil {
			select {
			case <-ctx.Done():
				return leaseUnreachable
			case <-time.After(taskRetryBackoff):
			}
			continue
		}
		if current.Status != TaskWorking {
			return leaseTerminal
		}
		current.LeaseUntil = s.now().Add(s.lease())
		current.LastUpdatedAt = s.now()
		if err := s.config.Store.UpdateTask(ctx, current); err != nil {
			if !errors.Is(err, ErrTaskConflict) {
				return leaseUnreachable
			}
			continue
		}
		return leaseRenewed
	}
	return leaseUnreachable
}

func (s *taskService) leaseHeld(record TaskRecord) bool {
	return !record.LeaseUntil.IsZero() && s.now().Before(record.LeaseUntil)
}

func (s *taskService) lease() time.Duration {
	if s.config.Lease > 0 {
		return s.config.Lease
	}
	return 30 * time.Second
}

// RecoverTasks re-launches execution for tasks that were still working when
// the previous process stopped. Call it after the async entries are exposed
// and before serving traffic. The TaskStore must implement WorkingTaskLister;
// otherwise RecoverTasks returns nil and recovery is a no-op.
func (s *Server) RecoverTasks(ctx context.Context) error {
	if s.tasks == nil {
		return nil
	}
	return s.tasks.recoverWorking(ctx)
}

func (s *taskService) finish(ctx context.Context, record TaskRecord, result *mcpsdk.CallToolResult, runErr error, failureMessage string) {
	ctx = context.WithoutCancel(ctx)
	// failureMessage lands inside a JSON string literal, so marshal it
	// instead of interpolating: a message with a quote or backslash must not
	// emit invalid JSON.
	var failurePayload json.RawMessage
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		raw, err := json.Marshal(struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}{Code: -32603, Message: failureMessage})
		if err != nil {
			raw = json.RawMessage(`{"code":-32603,"message":"task execution failed"}`)
		}
		failurePayload = raw
	}
	for attempts := 0; attempts < 3; attempts++ {
		current, err := s.config.Store.GetTask(ctx, record.ID)
		if err != nil || current.Status.terminal() {
			return
		}
		current.LastUpdatedAt = s.now()
		current.LeaseUntil = time.Time{}
		if runErr != nil {
			if errors.Is(runErr, context.Canceled) {
				current.Status = TaskCancelled
				current.StatusMessage = "cancelled"
			} else {
				current.Status = TaskFailed
				current.StatusMessage = failureMessage
				current.Failure = failurePayload
			}
		} else {
			current.Status = TaskCompleted
			current.Result = result
		}
		err = s.config.Store.UpdateTask(ctx, current)
		if err == nil {
			return
		}
		if !errors.Is(err, ErrTaskConflict) {
			break
		}
	}
	slog.Error("lebro/mcp: task completion not persisted", "task_id", record.ID)
	if s.config.OnConflict != nil {
		s.config.OnConflict(ctx, record)
	}
}

type taskParams struct {
	mcpsdk.ParamsBase
	TaskID string `json:"taskId"`
}
type taskUpdateParams struct {
	mcpsdk.ParamsBase
	TaskID         string                  `json:"taskId"`
	InputResponses mcpsdk.InputResponseMap `json:"inputResponses,omitempty"`
}
type taskResult struct {
	mcpsdk.ResultBase
	ResultType string `json:"resultType"`
	TaskRecord
}
type taskAck struct {
	mcpsdk.ResultBase
	ResultType string `json:"resultType"`
}

func (s *taskService) get(ctx context.Context, _ *mcpsdk.ServerSession, params *taskParams) (*taskResult, error) {
	if !tasksNegotiated(params.GetMeta()) {
		return nil, missingTasksCapability()
	}
	record, err := s.loadAuthorized(ctx, params.TaskID)
	if err != nil {
		return nil, err
	}
	return &taskResult{ResultType: taskResultTypeComplete, TaskRecord: record}, nil
}

func (s *taskService) update(ctx context.Context, _ *mcpsdk.ServerSession, params *taskUpdateParams) (*taskAck, error) {
	if !tasksNegotiated(params.GetMeta()) {
		return nil, missingTasksCapability()
	}
	if _, err := s.loadAuthorized(ctx, params.TaskID); err != nil {
		return nil, err
	}
	return &taskAck{ResultType: taskResultTypeComplete}, nil
}

func (s *taskService) cancel(ctx context.Context, _ *mcpsdk.ServerSession, params *taskParams) (*taskAck, error) {
	if !tasksNegotiated(params.GetMeta()) {
		return nil, missingTasksCapability()
	}
	for attempts := 0; attempts < 3; attempts++ {
		record, err := s.loadAuthorized(ctx, params.TaskID)
		if err != nil {
			return nil, err
		}
		if record.Status.terminal() {
			return &taskAck{ResultType: taskResultTypeComplete}, nil
		}
		record.Status, record.StatusMessage, record.LastUpdatedAt = TaskCancelled, "cancelled", s.now()
		record.LeaseUntil = time.Time{}
		if err := s.config.Store.UpdateTask(ctx, record); errors.Is(err, ErrTaskConflict) {
			continue
		} else if err != nil {
			// Store internals stay out of the JSON-RPC response; the client
			// sees a generic internal error and can retry.
			return nil, &mcpjsonrpc.Error{Code: -32603, Message: "Failed to cancel task: the task store is unavailable"}
		}
		// Cancel the local run when this process owns it. On a multi-instance
		// deployment the executing instance is a peer: it stops within one
		// lease-renewal tick, when its extendLease observes the terminal
		// status and cancels the run.
		s.mu.RLock()
		cancel := s.cancels[record.ID]
		s.mu.RUnlock()
		if cancel != nil {
			cancel()
		}
		if s.config.Cancel != nil {
			if err := s.config.Cancel(ctx, record); err != nil {
				return nil, err
			}
		}
		return &taskAck{ResultType: taskResultTypeComplete}, nil
	}
	return nil, ErrTaskConflict
}

func (s *taskService) loadAuthorized(ctx context.Context, id string) (TaskRecord, error) {
	record, err := s.config.Store.GetTask(ctx, id)
	if err != nil {
		if errors.Is(err, ErrTaskNotFound) {
			return TaskRecord{}, invalidTaskID("Task not found")
		}
		// Keep the client payload generic, but record the underlying cause
		// server-side so a store outage is diagnosable from the logs.
		slog.Error("lebro/mcp: task store read failed", "task_id", id, "error", err)
		return TaskRecord{}, &mcpjsonrpc.Error{Code: -32603, Message: "Failed to retrieve task: the task store is unavailable"}
	}
	if s.expired(record) {
		return TaskRecord{}, invalidTaskID("Task has expired")
	}
	if err := s.config.Authorize(ctx, record); err != nil {
		return TaskRecord{}, err
	}
	return record, nil
}

func invalidTaskID(message string) error {
	return &mcpjsonrpc.Error{Code: -32602, Message: "Failed to retrieve task: " + message}
}

func (s *taskService) expired(record TaskRecord) bool {
	return record.TTLMs != nil && s.now().After(record.CreatedAt.Add(time.Duration(*record.TTLMs)*time.Millisecond))
}
func (s *taskService) now() time.Time {
	if s.config.Now != nil {
		return s.config.Now().UTC()
	}
	return time.Now().UTC()
}
func (s *taskService) pollMs() int64 {
	if s.config.PollInterval > 0 {
		return s.config.PollInterval.Milliseconds()
	}
	return 1000
}
func (s *taskService) newID() (string, error) {
	if s.config.NewID != nil {
		return s.config.NewID()
	}
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}
func cloneRaw(value json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), value...) }
