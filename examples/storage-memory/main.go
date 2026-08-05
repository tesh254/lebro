// storage-memory demonstrates the storage contracts with the in-memory
// adapter. It is suitable for tests and local development; production adapters
// will use the same Store and repository interfaces.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/tesh254/lebro"
)

func main() {
	ctx := context.Background()
	store := lebro.NewMemoryStore()
	now := time.Now().UTC()

	err := store.Transaction(ctx, func(ctx context.Context, repositories lebro.Repositories) error {
		if err := repositories.Threads().CreateThread(ctx, lebro.ThreadRecord{
			ID:        "support-42",
			Metadata:  json.RawMessage(`{"customer":"acme"}`),
			CreatedAt: now,
			UpdatedAt: now,
		}); err != nil {
			return err
		}

		if err := repositories.Messages().AppendMessages(ctx, []lebro.MessageRecord{{
			ID:        "message-1",
			ThreadID:  "support-42",
			Message:   lebro.Message{Role: lebro.RoleUser, Content: "Where is my order?"},
			CreatedAt: now,
		}}); err != nil {
			return err
		}

		if err := repositories.WorkflowRuns().SaveWorkflowRun(ctx, lebro.WorkflowRunRecord{
			ID:         "run-1",
			WorkflowID: "support-triage",
			ThreadID:   "support-42",
			Status:     lebro.RunStatusRunning,
			Input:      json.RawMessage(`{"priority":"normal"}`),
			StartedAt:  now,
			UpdatedAt:  now,
		}); err != nil {
			return err
		}

		return repositories.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, lebro.WorkflowSnapshotRecord{
			ID:        "snapshot-1",
			RunID:     "run-1",
			Sequence:  1,
			State:     json.RawMessage(`{"step":"classify"}`),
			CreatedAt: now,
		})
	})
	if err != nil {
		log.Fatal(err)
	}

	messages, err := store.Messages().ListMessages(ctx, "support-42", lebro.PageRequest{})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("stored %d message(s): %s\n", len(messages.Records), messages.Records[0].Message.Content)
}
