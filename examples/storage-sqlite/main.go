// storage-sqlite demonstrates the durable file-backed storage adapter. The
// store is opened from a SQLite database file, migrated once, written through
// a transaction, and read back after close/reopen to show records survive
// process restarts. Running the example again on the same file reads the
// records left behind by the previous run instead of seeding duplicates.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/tesh254/lebro"
)

func main() {
	ctx := context.Background()
	path := "storage-sqlite.db"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}

	store := mustValue(lebro.NewSQLiteStore(path))
	must(store.Migrate(ctx))
	now := time.Now().UTC()

	if _, err := store.Threads().GetThread(ctx, "support-42"); errors.Is(err, lebro.ErrNotFound) {
		seed(ctx, store, now)
	} else if err != nil {
		panic(err)
	}

	// Close and reopen the same database file; records must survive.
	must(store.Close())
	store = mustValue(lebro.NewSQLiteStore(path))
	defer store.Close()
	must(store.Migrate(ctx))

	messages := mustValue(store.Messages().ListMessages(ctx, "support-42", lebro.PageRequest{}))
	fmt.Printf("after reopen: %d message(s)\n", len(messages.Records))
	run := mustValue(store.WorkflowRuns().GetWorkflowRun(ctx, "run-1"))
	fmt.Printf("run %s status: %s\n", run.ID, run.Status)
}

// seed writes the example records once, atomically.
func seed(ctx context.Context, store *lebro.SQLiteStore, now time.Time) {
	if err := store.Transaction(ctx, func(ctx context.Context, repositories lebro.Repositories) error {
		return runSteps(
			func() error {
				return repositories.Threads().CreateThread(ctx, lebro.ThreadRecord{
					ID:        "support-42",
					Metadata:  json.RawMessage(`{"customer":"acme"}`),
					CreatedAt: now,
					UpdatedAt: now,
				})
			},
			func() error {
				return repositories.Messages().AppendMessages(ctx, []lebro.MessageRecord{{
					ID:        "message-1",
					ThreadID:  "support-42",
					Message:   lebro.Message{Role: lebro.RoleUser, Content: "Where is my order?"},
					CreatedAt: now,
				}})
			},
			func() error {
				return repositories.WorkflowRuns().SaveWorkflowRun(ctx, lebro.WorkflowRunRecord{
					ID:         "run-1",
					WorkflowID: "support-triage",
					ThreadID:   "support-42",
					Status:     lebro.RunStatusRunning,
					Input:      json.RawMessage(`{"priority":"normal"}`),
					StartedAt:  now,
					UpdatedAt:  now,
				})
			},
			func() error {
				return repositories.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, lebro.WorkflowSnapshotRecord{
					ID:        "snapshot-1",
					RunID:     "run-1",
					Sequence:  1,
					State:     json.RawMessage(`{"step":"classify"}`),
					CreatedAt: now,
				})
			},
		)
	}); err != nil {
		panic(err)
	}
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

func runSteps(steps ...func() error) error {
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}
