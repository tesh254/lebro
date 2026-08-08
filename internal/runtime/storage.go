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
	ID        string          `json:"id"`
	ThreadID  ThreadID        `json:"thread_id"`
	Message   Message         `json:"message"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// WorkflowRunRecord captures the durable state of a workflow execution.
// Input, Output, and Metadata are raw JSON so applications can evolve their
// payload schemas without a storage-adapter change.
type WorkflowRunRecord struct {
	ID         RunID           `json:"id"`
	WorkflowID WorkflowID      `json:"workflow_id"`
	ThreadID   ThreadID        `json:"thread_id,omitempty"`
	Status     RunStatus       `json:"status"`
	Input      json.RawMessage `json:"input,omitempty"`
	Output     json.RawMessage `json:"output,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// WorkflowSnapshotRecord stores a resumable workflow state at a sequence
// number. State must be valid JSON to make snapshots portable across adapters.
type WorkflowSnapshotRecord struct {
	ID        string          `json:"id"`
	RunID     RunID           `json:"run_id"`
	Sequence  int64           `json:"sequence"`
	State     json.RawMessage `json:"state"`
	CreatedAt time.Time       `json:"created_at"`
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
	ListMessages(context.Context, ThreadID, PageRequest) (Page[MessageRecord], error)
}

// WorkflowRunRepository owns workflow executions.
type WorkflowRunRepository interface {
	SaveWorkflowRun(context.Context, WorkflowRunRecord) error
	GetWorkflowRun(context.Context, RunID) (WorkflowRunRecord, error)
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
