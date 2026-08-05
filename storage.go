package lebro

import "context"

// ThreadStore persists a conversation's canonical message history. Concrete
// in-memory, SQLite, and Postgres implementations are added separately.
type ThreadStore interface {
	AppendMessages(context.Context, ThreadID, []Message) error
	Messages(context.Context, ThreadID) ([]Message, error)
}

// WorkflowRunStore persists workflow run records and supports durable resume.
type WorkflowRunStore interface {
	SaveRun(context.Context, RunResult) error
	LoadRun(context.Context, RunID) (RunResult, error)
}
