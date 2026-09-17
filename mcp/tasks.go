package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	mcpjsonrpc "github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const tasksExtension = "io.modelcontextprotocol/tasks"

var (
	// ErrTaskNotFound is returned when a task is absent or its retention period elapsed.
	ErrTaskNotFound = errors.New("lebro/mcp: task not found")
	// ErrTaskConflict is returned when a task update loses a concurrent state transition.
	ErrTaskConflict = errors.New("lebro/mcp: task conflict")
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
	Version        int64                  `json:"-"`
}

// TaskStore persists task identity and terminal results. Update must reject a
// stale Version with ErrTaskConflict, making cancellation and completion safe
// across server instances.
type TaskStore interface {
	CreateTask(context.Context, TaskRecord) error
	GetTask(context.Context, string) (TaskRecord, error)
	UpdateTask(context.Context, TaskRecord) error
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
	// OnConflict observes a completion that could not be persisted after
	// retries. Applications should alert and reconcile the durable run.
	OnConflict func(context.Context, TaskRecord)
}

func (c *TaskConfig) validate() error {
	if c == nil || c.Store == nil {
		return errors.New("lebro/mcp: Tasks.Store is required")
	}
	if c.Identity == nil || c.Context == nil || c.Authorize == nil {
		return errors.New("lebro/mcp: Tasks.Identity, Context, and Authorize are required")
	}
	if c.TTL < 0 || c.PollInterval < 0 || c.SyncTimeout < 0 {
		return errors.New("lebro/mcp: task TTL and poll interval must not be negative")
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
}

func newTaskService(config *TaskConfig) *taskService {
	return &taskService{config: config, entries: make(map[string]taskEntry), cancels: make(map[string]context.CancelFunc)}
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
	return &taskResult{ResultBase: mcpsdk.ResultBase{}, ResultType: "task", TaskRecord: record}, nil
}

func (s *taskService) execute(record TaskRecord, entry taskEntry) {
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancels[record.ID] = cancel
	s.mu.Unlock()
	defer func() { cancel(); s.mu.Lock(); delete(s.cancels, record.ID); s.mu.Unlock() }()
	current, err := s.config.Store.GetTask(ctx, record.ID)
	if err != nil || current.Status != TaskWorking {
		return
	}
	ctx, err = s.config.Context(ctx, record)
	if err != nil {
		s.finish(ctx, record, nil, err, "restore task execution context failed")
		return
	}
	result, err := entry.run(ctx, cloneRaw(record.Arguments))
	s.finish(ctx, record, result, err, "task execution failed")
}

func (s *taskService) finish(ctx context.Context, record TaskRecord, result *mcpsdk.CallToolResult, runErr error, failureMessage string) {
	ctx = context.WithoutCancel(ctx)
	for attempts := 0; attempts < 3; attempts++ {
		current, err := s.config.Store.GetTask(ctx, record.ID)
		if err != nil || current.Status.terminal() {
			return
		}
		current.LastUpdatedAt = s.now()
		if runErr != nil {
			if errors.Is(runErr, context.Canceled) {
				current.Status = TaskCancelled
				current.StatusMessage = "cancelled"
			} else {
				current.Status = TaskFailed
				current.StatusMessage = failureMessage
				current.Failure = json.RawMessage(`{"code":-32603,"message":"` + failureMessage + `"}`)
			}
		} else {
			current.Status = TaskCompleted
			current.Result = result
		}
		if err := s.config.Store.UpdateTask(ctx, current); !errors.Is(err, ErrTaskConflict) {
			return
		}
	}
	slog.Error("lebro/mcp: task completion conflict", "task_id", record.ID)
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
	return &taskResult{ResultType: "complete", TaskRecord: record}, nil
}

func (s *taskService) update(ctx context.Context, _ *mcpsdk.ServerSession, params *taskUpdateParams) (*taskAck, error) {
	if !tasksNegotiated(params.GetMeta()) {
		return nil, missingTasksCapability()
	}
	if _, err := s.loadAuthorized(ctx, params.TaskID); err != nil {
		return nil, err
	}
	return &taskAck{ResultType: "complete"}, nil
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
			return &taskAck{ResultType: "complete"}, nil
		}
		record.Status, record.StatusMessage, record.LastUpdatedAt = TaskCancelled, "cancelled", s.now()
		if err := s.config.Store.UpdateTask(ctx, record); errors.Is(err, ErrTaskConflict) {
			continue
		} else if err != nil {
			return nil, err
		}
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
		return &taskAck{ResultType: "complete"}, nil
	}
	return nil, ErrTaskConflict
}

func (s *taskService) loadAuthorized(ctx context.Context, id string) (TaskRecord, error) {
	record, err := s.config.Store.GetTask(ctx, id)
	if err != nil {
		if errors.Is(err, ErrTaskNotFound) {
			return TaskRecord{}, invalidTaskID("Task not found")
		}
		return TaskRecord{}, err
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
