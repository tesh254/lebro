package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrNotFound is returned when a repository has no record for an identifier.
var ErrNotFound = errors.New("lebro: record not found")

// ErrConflict is returned when an optimistic transaction observes a concurrent
// write before it can commit. Callers may retry the transaction.
var ErrConflict = errors.New("lebro: storage conflict")

// ErrInvalidPage is returned when a pagination request has an invalid cursor
// or limit.
var ErrInvalidPage = errors.New("lebro: invalid page request")

// PageRequest specifies a bounded, cursor-based repository query. A zero
// Limit lets an adapter choose its default page size.
type PageRequest struct {
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// Page is a stable result shape for repository list operations. An empty
// NextCursor indicates that there are no more records.
type Page[T any] struct {
	Records    []T    `json:"records"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// RuntimeScope identifies an application-controlled runtime boundary. Namespace
// conventionally names the tenant or organization and OwnerID names a user (or
// other principal) within it. Both fields are optional so existing
// single-tenant applications keep their zero-value behavior.
//
// Lebro deliberately does not attach application meaning to either value: an
// application owns organizations, users, credentials, and authorization.
type RuntimeScope struct {
	Namespace string `json:"namespace,omitempty"`
	OwnerID   string `json:"owner_id,omitempty"`
}

// ThreadRecord owns conversation metadata. Messages are stored separately so
// adapters can append to long-lived threads efficiently. Namespace and OwnerID
// scope threads for multi-tenant and embedding applications; both are optional
// and empty values are valid for single-namespace use.
type ThreadRecord struct {
	ID        ThreadID        `json:"id"`
	Namespace string          `json:"namespace,omitempty"`
	OwnerID   string          `json:"owner_id,omitempty"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// MessageRecord is the durable, ordered representation of a Message.
type MessageRecord struct {
	ID       string          `json:"id"`
	ThreadID ThreadID        `json:"thread_id"`
	Message  Message         `json:"message"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
	// Annotations holds validated, namespaced application metadata. It is
	// separate from the free-form Metadata JSON so existing callers are
	// unaffected.
	Annotations Metadata  `json:"annotations,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// WorkflowFailureData records the normalized cause of a failed or cancelled
// workflow run. It mirrors WorkflowError but is stored as portable JSON so
// adapters do not need to understand Go error wrappers. Kind is a
// WorkflowErrorKind; Step is 1-indexed (0 means the failure happened before
// the first step); Message is the human-readable cause.
type WorkflowFailureData struct {
	Kind    WorkflowErrorKind `json:"kind,omitempty"`
	Step    int               `json:"step,omitempty"`
	StepID  StepID            `json:"step_id,omitempty"`
	Message string            `json:"message,omitempty"`
}

// WorkflowRunRecord captures the durable state of a workflow execution.
// Input, Output, StepOutputs, and Metadata are raw JSON so applications can
// evolve their payload schemas without a storage-adapter change. Failure is a
// typed *WorkflowFailureData (not raw JSON) so the failure context is stable
// across adapters. CurrentStep and CurrentStepID identify the last step the
// run reached (0 and "" before the first step). WorkflowVersion is an opaque
// caller-supplied definition/version reference for forward-compatible
// migrations; the executor never interprets it. Path is the ordered list of
// StepIDs of the first steps of the branches selected at each branching step;
// it is empty when no branching step was reached.
type WorkflowRunRecord struct {
	ID              RunID                `json:"id"`
	WorkflowID      WorkflowID           `json:"workflow_id"`
	ThreadID        ThreadID             `json:"thread_id,omitempty"`
	Namespace       string               `json:"namespace,omitempty"`
	OwnerID         string               `json:"owner_id,omitempty"`
	Status          RunStatus            `json:"status"`
	Input           json.RawMessage      `json:"input,omitempty"`
	Output          json.RawMessage      `json:"output,omitempty"`
	StepOutputs     []json.RawMessage    `json:"step_outputs,omitempty"`
	CurrentStep     int                  `json:"current_step,omitempty"`
	CurrentStepID   StepID               `json:"current_step_id,omitempty"`
	Path            []StepID             `json:"path,omitempty"`
	Failure         *WorkflowFailureData `json:"failure,omitempty"`
	FanOut          []FanOutJoinResult   `json:"fan_out,omitempty"`
	WorkflowVersion string               `json:"workflow_version,omitempty"`
	Metadata        json.RawMessage      `json:"metadata,omitempty"`
	StartedAt       time.Time            `json:"started_at"`
	FinishedAt      *time.Time           `json:"finished_at,omitempty"`
	UpdatedAt       time.Time            `json:"updated_at"`
}

// WorkflowSnapshotRecord stores a resumable workflow state at a sequence
// number. State must be valid JSON to make snapshots portable across adapters.
// SchemaVersion is the snapshot envelope version; the initial release line
// writes 1 and readers tolerate 0 (legacy/unspecified). Adapters never
// interpret State.
type WorkflowSnapshotRecord struct {
	ID            string          `json:"id"`
	RunID         RunID           `json:"run_id"`
	Sequence      int64           `json:"sequence"`
	SchemaVersion int             `json:"schema_version,omitempty"`
	State         json.RawMessage `json:"state"`
	CreatedAt     time.Time       `json:"created_at"`
}

// WorkflowRunFilter narrows a ListWorkflowRuns query. A zero value returns
// every run. WorkflowID and Status match exactly when non-zero; an empty
// Status matches any status.
type WorkflowRunFilter struct {
	WorkflowID WorkflowID
	Status     RunStatus
	Namespace  string
	OwnerID    string
}

// ModelUsage records provider-reported token usage when available.
type ModelUsage struct {
	InputTokens     int64 `json:"input_tokens,omitempty"`
	OutputTokens    int64 `json:"output_tokens,omitempty"`
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
	TotalTokens     int64 `json:"total_tokens,omitempty"`
}

// Metadata is validated, namespaced application metadata attached to durable
// records. Keys must be namespaced ("app.customer_id", "plugin.compaction")
// so independently developed integrations cannot collide, and the "lebro"
// namespace is reserved for runtime-owned keys. Values are raw JSON. Nil and
// empty Metadata are valid so existing callers are unaffected.
//
// Validation enforces bounded size so a misbehaving caller cannot turn an
// observability record into an unbounded sink: see Validate for the limits.
type Metadata map[string]json.RawMessage

// PluginAttribution identifies the plugin responsible for a durable record or
// event. It is populated by runtime plugin hooks introduced after this
// contract; plugins that integrate today set it themselves when appending
// records through the repositories.
type PluginAttribution struct {
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
	Action  string `json:"action,omitempty"`
	Outcome string `json:"outcome,omitempty"`
}

// ModelAttemptRecord is the durable record of one actual provider invocation.
// A routed or fallback run persists every attempt, not only the winner. Token
// usage belongs here and never on tool-result messages. Request payloads,
// secrets, and opaque provider replay data are intentionally absent: records
// carry identity, outcome, and cost signals, not conversation content.
type ModelAttemptRecord struct {
	ID        string   `json:"id"`
	RunID     RunID    `json:"run_id"`
	ThreadID  ThreadID `json:"thread_id,omitempty"`
	Namespace string   `json:"namespace,omitempty"`
	OwnerID   string   `json:"owner_id,omitempty"`
	StepID    StepID   `json:"step_id,omitempty"`
	Step      int      `json:"step,omitempty"`
	// Index is the 1-indexed position of this attempt among the attempts for
	// one model call; the routed winner normally carries the highest index.
	Index    int        `json:"index"`
	Provider ProviderID `json:"provider,omitempty"`
	// Model is the provider-facing model name sent with the request.
	Model string `json:"model,omitempty"`
	// RoutedModel is the selected model identity when routing resolved a
	// different target than Model.
	RoutedModel  string             `json:"routed_model,omitempty"`
	Status       ModelAttemptStatus `json:"status"`
	FinishReason FinishReason       `json:"finish_reason,omitempty"`
	Usage        ModelUsage         `json:"usage"`
	StartedAt    time.Time          `json:"started_at"`
	FinishedAt   time.Time          `json:"finished_at"`
	// ProducedMessageIDs links the attempt to transcript message IDs it
	// produced (the terminal assistant message for the winning attempt).
	ProducedMessageIDs []string `json:"produced_message_ids,omitempty"`
	// ErrorKind is a normalized classification (for example a ModelErrorKind);
	// ErrorMessage is the safe error text. Raw provider payloads are excluded.
	ErrorKind    string `json:"error_kind,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	// ProviderRequestID, CostMicros, and Currency are recorded only when the
	// provider reports them; lebro never computes cost itself.
	ProviderRequestID string   `json:"provider_request_id,omitempty"`
	CostMicros        int64    `json:"cost_micros,omitempty"`
	Currency          string   `json:"currency,omitempty"`
	Metadata          Metadata `json:"metadata,omitempty"`
}

// ToolExecutionRecord is the durable record of one tool invocation lifecycle.
// Arguments and results are intentionally absent; failures carry a normalized
// state plus a safe, classified error message.
type ToolExecutionRecord struct {
	ID           string             `json:"id"`
	RunID        RunID              `json:"run_id"`
	ThreadID     ThreadID           `json:"thread_id,omitempty"`
	Namespace    string             `json:"namespace,omitempty"`
	OwnerID      string             `json:"owner_id,omitempty"`
	StepID       StepID             `json:"step_id,omitempty"`
	Step         int                `json:"step,omitempty"`
	ToolCallID   string             `json:"tool_call_id"`
	ToolID       ToolID             `json:"tool_id"`
	State        ToolExecutionState `json:"state"`
	StartedAt    time.Time          `json:"started_at"`
	FinishedAt   time.Time          `json:"finished_at"`
	ErrorKind    string             `json:"error_kind,omitempty"`
	ErrorMessage string             `json:"error_message,omitempty"`
	Metadata     Metadata           `json:"metadata,omitempty"`
}

// RunEventRecord is the durable, ordered representation of one RunEvent. The
// Sequence field is 1-indexed and monotonic within a run; stored sequences
// keep their original numbering even though delta events are omitted by
// default, so sequences may have gaps but never regress. Payload holds the
// per-type allowlisted context (retry attempts, loop iterations, route
// candidates); message content, tool arguments/results, and reasoning replay
// data are never written to it.
type RunEventRecord struct {
	ID              string                `json:"id"`
	RunID           RunID                 `json:"run_id"`
	ThreadID        ThreadID              `json:"thread_id,omitempty"`
	Namespace       string                `json:"namespace,omitempty"`
	OwnerID         string                `json:"owner_id,omitempty"`
	Sequence        int64                 `json:"sequence"`
	Type            RunEventType          `json:"type"`
	Timestamp       time.Time             `json:"timestamp"`
	StepID          StepID                `json:"step_id,omitempty"`
	Step            int                   `json:"step,omitempty"`
	ParentRunID     RunID                 `json:"parent_run_id,omitempty"`
	ParentStepID    StepID                `json:"parent_step_id,omitempty"`
	Branch          string                `json:"branch,omitempty"`
	ToolCallID      string                `json:"tool_call_id,omitempty"`
	ToolID          ToolID                `json:"tool_id,omitempty"`
	Provider        ProviderID            `json:"provider,omitempty"`
	ProviderModel   string                `json:"provider_model,omitempty"`
	AttemptStatus   ModelAttemptStatus    `json:"attempt_status,omitempty"`
	ProcessorPhase  ProcessorPhase        `json:"processor_phase,omitempty"`
	ProcessorAction ProcessorDecisionKind `json:"processor_action,omitempty"`
	Status          RunStatus             `json:"status,omitempty"`
	FinishReason    FinishReason          `json:"finish_reason,omitempty"`
	Usage           ModelUsage            `json:"usage,omitempty"`
	DurationNanos   int64                 `json:"duration_ns,omitempty"`
	// ErrorKind classifies a reported failure (agent, model, tool, or context
	// kind); ErrorMessage is its safe text. Raw provider payloads stay out.
	ErrorKind    string             `json:"error_kind,omitempty"`
	ErrorMessage string             `json:"error_message,omitempty"`
	Payload      json.RawMessage    `json:"payload,omitempty"`
	Plugin       *PluginAttribution `json:"plugin,omitempty"`
	Metadata     Metadata           `json:"metadata,omitempty"`
}

// RunEventFilter narrows a ListRunEvents query. Zero values match anything;
// From is inclusive and To is exclusive on the event timestamp.
type RunEventFilter struct {
	RunID     RunID
	ThreadID  ThreadID
	Namespace string
	OwnerID   string
	Type      RunEventType
	From      time.Time
	To        time.Time
	Provider  ProviderID
	ToolID    ToolID
}

// ModelAttemptFilter narrows a ListModelAttempts query. Zero values match
// anything.
type ModelAttemptFilter struct {
	RunID     RunID
	ThreadID  ThreadID
	Namespace string
	OwnerID   string
	Provider  ProviderID
	Status    ModelAttemptStatus
}

// ToolExecutionFilter narrows a ListToolExecutions query. Zero values match
// anything.
type ToolExecutionFilter struct {
	RunID     RunID
	ThreadID  ThreadID
	Namespace string
	OwnerID   string
	ToolID    ToolID
	State     ToolExecutionState
}

// RunEventRepository owns ordered, durable run events.
type RunEventRepository interface {
	AppendRunEvents(context.Context, []RunEventRecord) error
	ListRunEvents(context.Context, RunEventFilter, PageRequest) (Page[RunEventRecord], error)
}

// ModelAttemptRepository owns durable model-attempt records.
type ModelAttemptRepository interface {
	SaveModelAttempts(context.Context, []ModelAttemptRecord) error
	ListModelAttempts(context.Context, ModelAttemptFilter, PageRequest) (Page[ModelAttemptRecord], error)
}

// ToolExecutionRepository owns durable tool-execution records.
type ToolExecutionRepository interface {
	SaveToolExecutions(context.Context, []ToolExecutionRecord) error
	ListToolExecutions(context.Context, ToolExecutionFilter, PageRequest) (Page[ToolExecutionRecord], error)
}

// ObservabilityRepositories groups the durable observability record
// repositories. Store support is opt-in: a Store participates simply by
// having both the Store itself and the Repositories values handed to
// Transaction implement this interface. Adapters that do not implement it
// leave the new records unpersisted and behave exactly as before, so custom
// Store implementations are never forced to add tables or methods.
type ObservabilityRepositories interface {
	RunEvents() RunEventRepository
	ModelAttempts() ModelAttemptRepository
	ToolExecutions() ToolExecutionRepository
}

// WorkingMemoryFact is one durable, user-scoped fact. Namespace identifies a
// tenant and OwnerID identifies the user within it. Key is unique within that
// scope. Version starts at one and changes only after a successful write.
type WorkingMemoryFact struct {
	ID        string          `json:"id"`
	Namespace string          `json:"namespace"`
	OwnerID   string          `json:"owner_id"`
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	Version   int64           `json:"version"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// WorkingMemoryScope selects facts owned by one user in one tenant.
// WorkingMemoryScope retains its original JSON field names for wire
// compatibility. RuntimeScope is the reusable scope for new record types.
type WorkingMemoryScope struct {
	Namespace string
	OwnerID   string
}

// WorkingMemoryRepository owns scoped fact CRUD. expectedVersion is zero for
// create and otherwise must equal the stored version; conflicts return
// ErrConflict. Clear deletes every fact in scope.
type WorkingMemoryRepository interface {
	UpsertWorkingMemoryFact(context.Context, WorkingMemoryFact, int64) (WorkingMemoryFact, error)
	GetWorkingMemoryFact(context.Context, WorkingMemoryScope, string) (WorkingMemoryFact, error)
	ListWorkingMemoryFacts(context.Context, WorkingMemoryScope, PageRequest) (Page[WorkingMemoryFact], error)
	DeleteWorkingMemoryFact(context.Context, WorkingMemoryScope, string, int64) error
	ClearWorkingMemory(context.Context, WorkingMemoryScope) error
}

// ThreadRepository owns thread records and their lifecycle.
type ThreadRepository interface {
	CreateThread(context.Context, ThreadRecord) error
	GetThread(context.Context, ThreadID) (ThreadRecord, error)
	UpdateThread(context.Context, ThreadRecord) error
}

// MessageRepository owns ordered messages within a thread.
type MessageRepository interface {
	AppendMessages(context.Context, []MessageRecord) error
	// UpdateMessages replaces existing messages without changing their thread,
	// sequence position, or creation timestamp.
	UpdateMessages(context.Context, []MessageRecord) error
	// DeleteMessages removes messages from one thread. Missing IDs are ignored.
	DeleteMessages(context.Context, ThreadID, []string) error
	ListMessages(context.Context, ThreadID, PageRequest) (Page[MessageRecord], error)
}

// WorkflowRunRepository owns workflow executions.
type WorkflowRunRepository interface {
	SaveWorkflowRun(context.Context, WorkflowRunRecord) error
	GetWorkflowRun(context.Context, RunID) (WorkflowRunRecord, error)
	ListWorkflowRuns(context.Context, WorkflowRunFilter, PageRequest) (Page[WorkflowRunRecord], error)
}

// WorkflowSnapshotRepository owns ordered resumable workflow snapshots.
type WorkflowSnapshotRepository interface {
	SaveWorkflowSnapshot(context.Context, WorkflowSnapshotRecord) error
	ListWorkflowSnapshots(context.Context, RunID, PageRequest) (Page[WorkflowSnapshotRecord], error)
}

// Repositories groups a consistent view of every storage repository. Database
// adapters must return transaction-scoped repositories from Store.Transaction.
type Repositories interface {
	Threads() ThreadRepository
	Messages() MessageRepository
	WorkflowRuns() WorkflowRunRepository
	WorkflowSnapshots() WorkflowSnapshotRepository
	Schedules() ScheduleRepository
	ScheduleExecutions() ScheduleExecutionRepository
	WorkingMemory() WorkingMemoryRepository
}

// Store owns transaction boundaries and migration execution. Adapters own the
// schema migrations needed for their backing technology; callers own the
// atomic unit of work passed to Transaction.
type Store interface {
	Repositories
	Transaction(context.Context, func(context.Context, Repositories) error) error
	Migrate(context.Context) error
}

// ThreadStore is retained as a small compatibility contract for code that only
// needs a conversation's message history.
type ThreadStore interface {
	AppendMessages(context.Context, ThreadID, []Message) error
	Messages(context.Context, ThreadID) ([]Message, error)
}

// WorkflowRunStore is retained as a small compatibility contract for code that
// only needs executable workflow results.
type WorkflowRunStore interface {
	SaveRun(context.Context, RunResult) error
	LoadRun(context.Context, RunID) (RunResult, error)
}
