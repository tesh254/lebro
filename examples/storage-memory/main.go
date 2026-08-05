// storage-memory demonstrates the storage contracts with the in-memory
// adapter. It is suitable for tests and local development; production adapters
// will use the same Store and repository interfaces.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/tesh254/lebro"
)

func main() {
	ctx := context.Background()
	store := lebro.NewMemoryStore()
	now := time.Now().UTC()

	must(store.Transaction(ctx, func(ctx context.Context, repositories lebro.Repositories) error {
		must(repositories.Threads().CreateThread(ctx, lebro.ThreadRecord{
			ID:        "support-42",
			Metadata:  json.RawMessage(`{"customer":"acme"}`),
			CreatedAt: now,
			UpdatedAt: now,
		}))

		must(repositories.Messages().AppendMessages(ctx, []lebro.MessageRecord{{
			ID:        "message-1",
			ThreadID:  "support-42",
			Message:   lebro.Message{Role: lebro.RoleUser, Content: "Where is my order?"},
			CreatedAt: now,
		}}))

		must(repositories.WorkflowRuns().SaveWorkflowRun(ctx, lebro.WorkflowRunRecord{
			ID:         "run-1",
			WorkflowID: "support-triage",
			ThreadID:   "support-42",
			Status:     lebro.RunStatusRunning,
			Input:      json.RawMessage(`{"priority":"normal"}`),
			StartedAt:  now,
			UpdatedAt:  now,
		}))

		must(repositories.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, lebro.WorkflowSnapshotRecord{
			ID:        "snapshot-1",
			RunID:     "run-1",
			Sequence:  1,
			State:     json.RawMessage(`{"step":"classify"}`),
			CreatedAt: now,
		}))
		return nil
	}))

	messages := mustValue(store.Messages().ListMessages(ctx, "support-42", lebro.PageRequest{}))
	fmt.Printf("stored %d message(s): %s\n", len(messages.Records), messages.Records[0].Message.Content)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func mustValue[T any](value T, err error) T {
	must(err)
	return value
}
