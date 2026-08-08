package testkit

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tesh254/lebro/internal/runtime"
)

// StoreFactory builds the storage adapter under contract scrutiny. It must
// return a store whose schema is ready for records (adapters with migrations
// should run Migrate themselves or arrange for the suite to do it).
type StoreFactory func(*testing.T) runtime.Store

// StorageContractSuite runs the adapter-neutral storage behaviors that every
// Store implementation — in-memory or database-backed — must satisfy: record
// round-trips and ordering, pagination, validation, error vocabulary,
// transaction atomicity, and context cancellation. Adapters keep their own
// close/reopen, migration, and lock-contention tests.
func StorageContractSuite(t *testing.T, newStore StoreFactory) {
	t.Helper()
	t.Run("repository contract", func(t *testing.T) { storageContractRepository(t, newStore) })
	t.Run("transaction rolls back", func(t *testing.T) { storageContractTransactionRollback(t, newStore) })
	t.Run("rejects invalid JSON", func(t *testing.T) { storageContractInvalidJSON(t, newStore) })
	t.Run("repository errors", func(t *testing.T) { storageContractRepositoryErrors(t, newStore) })
	t.Run("transaction repositories", func(t *testing.T) { storageContractTransactionRepositories(t, newStore) })
	t.Run("migration is idempotent", func(t *testing.T) { storageContractMigrateIdempotent(t, newStore) })
	t.Run("honors canceled context", func(t *testing.T) { storageContractCanceledContext(t, newStore) })
	t.Run("pagination bounds", func(t *testing.T) { storageContractPaginationBounds(t, newStore) })
	t.Run("defensive copies and transaction cancellation", func(t *testing.T) { storageContractDefensiveCopies(t, newStore) })
	t.Run("outer store reads in transaction", func(t *testing.T) { storageContractOuterReads(t, newStore) })
}

func storageContractRepository(t *testing.T, newStore StoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	thread := runtime.ThreadRecord{ID: "thread-1", Metadata: json.RawMessage(`{"tag":"original"}`), CreatedAt: now, UpdatedAt: now}
	if err := store.Threads().CreateThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	if err := store.Threads().UpdateThread(ctx, runtime.ThreadRecord{ID: "thread-1", Metadata: json.RawMessage(`{"tag":"updated"}`), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	gotThread, err := store.Threads().GetThread(ctx, "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(gotThread.Metadata) != `{"tag":"updated"}` {
		t.Fatalf("thread metadata = %s, want updated value", gotThread.Metadata)
	}

	messages := []runtime.MessageRecord{
		{ID: "message-1", ThreadID: "thread-1", Message: runtime.Message{Role: runtime.RoleUser, Content: "one"}, CreatedAt: now},
		{ID: "message-2", ThreadID: "thread-1", Message: runtime.Message{Role: runtime.RoleAssistant, Content: "two"}, CreatedAt: now},
		{ID: "message-3", ThreadID: "thread-1", Message: runtime.Message{Role: runtime.RoleTool, Content: "three", ToolCallID: "call-1"}, CreatedAt: now},
	}
	if err := store.Messages().AppendMessages(ctx, messages); err != nil {
		t.Fatal(err)
	}
	first, err := store.Messages().ListMessages(ctx, "thread-1", runtime.PageRequest{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Records) != 2 || first.NextCursor == "" {
		t.Fatalf("first page = %#v, want two records and a cursor", first)
	}
	first.Records[0].Message.Content = "mutated"
	again, err := store.Messages().ListMessages(ctx, "thread-1", runtime.PageRequest{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Records[0].Message.Content; got != "one" {
		t.Fatalf("stored message = %q, want one", got)
	}
	second, err := store.Messages().ListMessages(ctx, "thread-1", runtime.PageRequest{Cursor: first.NextCursor, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Records) != 1 || second.Records[0].ID != "message-3" || second.NextCursor != "" {
		t.Fatalf("second page = %#v, want final message only", second)
	}

	run := runtime.WorkflowRunRecord{ID: "run-1", WorkflowID: "workflow-1", Status: runtime.RunStatusRunning, StartedAt: now, UpdatedAt: now}
	if err := store.WorkflowRuns().SaveWorkflowRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := store.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, runtime.WorkflowSnapshotRecord{ID: "snapshot-2", RunID: "run-1", Sequence: 2, State: json.RawMessage(`{"index":2}`), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, runtime.WorkflowSnapshotRecord{ID: "snapshot-1", RunID: "run-1", Sequence: 1, State: json.RawMessage(`{"index":1}`), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	snapshots, err := store.WorkflowSnapshots().ListWorkflowSnapshots(ctx, "run-1", runtime.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots.Records) != 2 || snapshots.Records[0].Sequence != 1 {
		t.Fatalf("snapshots = %#v, want ordered records", snapshots)
	}
}

func storageContractTransactionRollback(t *testing.T, newStore StoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)
	errRollback := errors.New("rollback")

	err := store.Transaction(ctx, func(ctx context.Context, repositories runtime.Repositories) error {
		if err := repositories.Threads().CreateThread(ctx, runtime.ThreadRecord{ID: "thread-1"}); err != nil {
			return err
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatalf("Transaction() error = %v, want rollback error", err)
	}
	if _, err := store.Threads().GetThread(ctx, "thread-1"); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("GetThread after rollback error = %v, want ErrNotFound", err)
	}

	if err := store.Transaction(ctx, func(ctx context.Context, repositories runtime.Repositories) error {
		return repositories.Threads().CreateThread(ctx, runtime.ThreadRecord{ID: "thread-1"})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Threads().GetThread(ctx, "thread-1"); err != nil {
		t.Fatalf("GetThread after commit: %v", err)
	}
}

func storageContractInvalidJSON(t *testing.T, newStore StoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)
	if err := store.Threads().CreateThread(ctx, runtime.ThreadRecord{ID: "thread-1", Metadata: json.RawMessage(`{`)}); err == nil {
		t.Fatal("CreateThread() error = nil, want invalid JSON error")
	}
}

func storageContractRepositoryErrors(t *testing.T, newStore StoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)

	if _, err := store.Threads().GetThread(ctx, "missing"); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("GetThread missing error = %v, want ErrNotFound", err)
	}
	if err := store.Threads().CreateThread(ctx, runtime.ThreadRecord{}); err == nil {
		t.Fatal("CreateThread missing ID error = nil")
	}
	thread := runtime.ThreadRecord{ID: "thread-1"}
	if err := store.Threads().CreateThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	if err := store.Threads().CreateThread(ctx, thread); err == nil {
		t.Fatal("CreateThread duplicate error = nil")
	}
	if err := store.Threads().UpdateThread(ctx, runtime.ThreadRecord{ID: "missing"}); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("UpdateThread missing error = %v, want ErrNotFound", err)
	}
	thread.Metadata = json.RawMessage(`{"updated":true}`)
	if err := store.Threads().UpdateThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	if err := store.Threads().UpdateThread(ctx, runtime.ThreadRecord{ID: thread.ID, Metadata: json.RawMessage(`{`)}); err == nil {
		t.Fatal("UpdateThread invalid metadata error = nil")
	}

	for _, records := range [][]runtime.MessageRecord{
		{{ThreadID: "thread-1"}},
		{{ID: "message-1", ThreadID: "missing", Message: runtime.Message{Role: runtime.RoleUser}}},
		{{ID: "message-1", ThreadID: "thread-1", Message: runtime.Message{Role: "invalid"}}},
		{{ID: "message-1", ThreadID: "thread-1", Message: runtime.Message{Role: runtime.RoleUser}, Metadata: json.RawMessage(`{`)}},
		{{ID: "same", ThreadID: "thread-1", Message: runtime.Message{Role: runtime.RoleUser}}, {ID: "same", ThreadID: "thread-1", Message: runtime.Message{Role: runtime.RoleAssistant}}},
	} {
		if err := store.Messages().AppendMessages(ctx, records); err == nil {
			t.Fatalf("AppendMessages(%#v) error = nil", records)
		}
	}
	message := runtime.MessageRecord{ID: "message-1", ThreadID: "thread-1", Message: runtime.Message{Role: runtime.RoleUser}}
	if err := store.Messages().AppendMessages(ctx, []runtime.MessageRecord{message}); err != nil {
		t.Fatal(err)
	}
	if err := store.Messages().AppendMessages(ctx, []runtime.MessageRecord{message}); err == nil {
		t.Fatal("AppendMessages duplicate error = nil")
	}
	if _, err := store.Messages().ListMessages(ctx, "missing", runtime.PageRequest{}); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("ListMessages missing error = %v, want ErrNotFound", err)
	}

	for _, run := range []runtime.WorkflowRunRecord{
		{},
		{ID: "run-1", WorkflowID: "workflow-1", Input: json.RawMessage(`{`)},
		{ID: "run-1", WorkflowID: "workflow-1", Metadata: json.RawMessage(`{`)},
	} {
		if err := store.WorkflowRuns().SaveWorkflowRun(ctx, run); err == nil {
			t.Fatalf("SaveWorkflowRun(%#v) error = nil", run)
		}
	}
	run := runtime.WorkflowRunRecord{ID: "run-1", WorkflowID: "workflow-1", Output: json.RawMessage(`{"ok":true}`)}
	if err := store.WorkflowRuns().SaveWorkflowRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, err := store.WorkflowRuns().GetWorkflowRun(ctx, "missing"); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("GetWorkflowRun missing error = %v, want ErrNotFound", err)
	}
	if _, err := store.WorkflowRuns().GetWorkflowRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.WorkflowRuns().SaveWorkflowRun(ctx, runtime.WorkflowRunRecord{ID: "run-1", WorkflowID: "workflow-2"}); err != nil {
		t.Fatalf("SaveWorkflowRun upsert: %v", err)
	}
	upserted, err := store.WorkflowRuns().GetWorkflowRun(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if upserted.WorkflowID != "workflow-2" {
		t.Fatalf("upserted run workflow ID = %q, want workflow-2", upserted.WorkflowID)
	}

	for _, snapshot := range []runtime.WorkflowSnapshotRecord{
		{},
		{ID: "snapshot-1", RunID: "missing"},
		{ID: "snapshot-1", RunID: run.ID, State: json.RawMessage(`{`)},
	} {
		if err := store.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, snapshot); err == nil {
			t.Fatalf("SaveWorkflowSnapshot(%#v) error = nil", snapshot)
		}
	}
	snapshot := runtime.WorkflowSnapshotRecord{ID: "snapshot-1", RunID: run.ID, Sequence: 1}
	if err := store.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := store.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, snapshot); err == nil {
		t.Fatal("SaveWorkflowSnapshot duplicate error = nil")
	}
	if err := store.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, runtime.WorkflowSnapshotRecord{ID: "snapshot-2", RunID: run.ID, Sequence: 1}); err == nil {
		t.Fatal("SaveWorkflowSnapshot duplicate sequence error = nil")
	}
	if _, err := store.WorkflowSnapshots().ListWorkflowSnapshots(ctx, "missing", runtime.PageRequest{}); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("ListWorkflowSnapshots missing error = %v, want ErrNotFound", err)
	}
}

func storageContractTransactionRepositories(t *testing.T, newStore StoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)

	if err := store.Transaction(ctx, func(ctx context.Context, repositories runtime.Repositories) error {
		if err := repositories.Threads().CreateThread(ctx, runtime.ThreadRecord{ID: "thread-1"}); err != nil {
			return err
		}
		if err := repositories.Threads().UpdateThread(ctx, runtime.ThreadRecord{ID: "thread-1", Metadata: json.RawMessage(`{"tx":true}`)}); err != nil {
			return err
		}
		if _, err := repositories.Threads().GetThread(ctx, "thread-1"); err != nil {
			return err
		}
		if err := repositories.Messages().AppendMessages(ctx, []runtime.MessageRecord{{ID: "message-1", ThreadID: "thread-1", Message: runtime.Message{Role: runtime.RoleUser}}}); err != nil {
			return err
		}
		if _, err := repositories.Messages().ListMessages(ctx, "thread-1", runtime.PageRequest{}); err != nil {
			return err
		}
		if err := repositories.WorkflowRuns().SaveWorkflowRun(ctx, runtime.WorkflowRunRecord{ID: "run-1", WorkflowID: "workflow-1"}); err != nil {
			return err
		}
		if _, err := repositories.WorkflowRuns().GetWorkflowRun(ctx, "run-1"); err != nil {
			return err
		}
		if err := repositories.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, runtime.WorkflowSnapshotRecord{ID: "snapshot-1", RunID: "run-1"}); err != nil {
			return err
		}
		_, err := repositories.WorkflowSnapshots().ListWorkflowSnapshots(ctx, "run-1", runtime.PageRequest{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func storageContractMigrateIdempotent(t *testing.T, newStore StoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate(): %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate(): %v", err)
	}
}

func storageContractCanceledContext(t *testing.T, newStore StoreFactory) {
	t.Helper()
	ready := context.Background()
	store := newStore(t)
	if err := store.Migrate(ready); err != nil {
		t.Fatal(err)
	}
	if err := store.Threads().CreateThread(ready, runtime.ThreadRecord{ID: "thread-1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.WorkflowRuns().SaveWorkflowRun(ready, runtime.WorkflowRunRecord{ID: "run-1", WorkflowID: "workflow-1"}); err != nil {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(ready)
	cancel()

	tests := []struct {
		name string
		call func() error
	}{
		{"create thread", func() error { return store.Threads().CreateThread(canceled, runtime.ThreadRecord{ID: "thread-2"}) }},
		{"get thread", func() error { _, err := store.Threads().GetThread(canceled, "thread-1"); return err }},
		{"update thread", func() error { return store.Threads().UpdateThread(canceled, runtime.ThreadRecord{ID: "thread-1"}) }},
		{"append messages", func() error {
			return store.Messages().AppendMessages(canceled, []runtime.MessageRecord{{ID: "message-1", ThreadID: "thread-1", Message: runtime.Message{Role: runtime.RoleUser}}})
		}},
		{"list messages", func() error {
			_, err := store.Messages().ListMessages(canceled, "thread-1", runtime.PageRequest{})
			return err
		}},
		{"save run", func() error {
			return store.WorkflowRuns().SaveWorkflowRun(canceled, runtime.WorkflowRunRecord{ID: "run-2", WorkflowID: "workflow-1"})
		}},
		{"get run", func() error { _, err := store.WorkflowRuns().GetWorkflowRun(canceled, "run-1"); return err }},
		{"save snapshot", func() error {
			return store.WorkflowSnapshots().SaveWorkflowSnapshot(canceled, runtime.WorkflowSnapshotRecord{ID: "snapshot-1", RunID: "run-1"})
		}},
		{"list snapshots", func() error {
			_, err := store.WorkflowSnapshots().ListWorkflowSnapshots(canceled, "run-1", runtime.PageRequest{})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
		})
	}
	if err := store.Transaction(canceled, func(context.Context, runtime.Repositories) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("Transaction canceled error = %v, want context.Canceled", err)
	}
}

func storageContractPaginationBounds(t *testing.T, newStore StoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)
	if err := store.Threads().CreateThread(ctx, runtime.ThreadRecord{ID: "thread-1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Messages().AppendMessages(ctx, []runtime.MessageRecord{{ID: "message-1", ThreadID: "thread-1", Message: runtime.Message{Role: runtime.RoleUser}}}); err != nil {
		t.Fatal(err)
	}
	page, err := store.Messages().ListMessages(ctx, "thread-1", runtime.PageRequest{Cursor: "100", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 0 || page.NextCursor != "" {
		t.Fatalf("out-of-range page = %#v", page)
	}
	page, err = store.Messages().ListMessages(ctx, "thread-1", runtime.PageRequest{Cursor: "-1", Limit: -1})
	if !errors.Is(err, runtime.ErrInvalidPage) {
		t.Fatalf("negative page error = %v, want ErrInvalidPage", err)
	}
	if _, err := store.Messages().ListMessages(ctx, "thread-1", runtime.PageRequest{Cursor: "not-a-cursor"}); !errors.Is(err, runtime.ErrInvalidPage) {
		t.Fatalf("malformed cursor error = %v, want ErrInvalidPage", err)
	}
	if err := store.Messages().AppendMessages(ctx, []runtime.MessageRecord{{ID: "message-2", ThreadID: "thread-1", Message: runtime.Message{Role: runtime.RoleAssistant}}}); err != nil {
		t.Fatal(err)
	}
	page, err = store.Messages().ListMessages(ctx, "thread-1", runtime.PageRequest{Cursor: "1", Limit: int(^uint(0) >> 1)})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 || page.Records[0].ID != "message-2" {
		t.Fatalf("large-limit page = %#v", page)
	}
}

func storageContractDefensiveCopies(t *testing.T, newStore StoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)
	if err := store.Threads().CreateThread(ctx, runtime.ThreadRecord{ID: "thread-1", Metadata: json.RawMessage(`{"version":1}`)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Messages().AppendMessages(ctx, []runtime.MessageRecord{{ID: "message-1", ThreadID: "thread-1", Message: runtime.Message{Role: runtime.RoleUser}, Metadata: json.RawMessage(`{"version":1}`)}}); err != nil {
		t.Fatal(err)
	}
	run := runtime.WorkflowRunRecord{ID: "run-1", WorkflowID: "workflow-1", Input: json.RawMessage(`{"version":1}`), Metadata: json.RawMessage(`{"version":1}`)}
	if err := store.WorkflowRuns().SaveWorkflowRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := store.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, runtime.WorkflowSnapshotRecord{ID: "snapshot-1", RunID: run.ID, State: json.RawMessage(`{"version":1}`)}); err != nil {
		t.Fatal(err)
	}

	loadedRun, err := store.WorkflowRuns().GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	loadedRun.Input[0] = '['
	loadedRun.Metadata[0] = '['
	againRun, err := store.WorkflowRuns().GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(againRun.Input) != `{"version":1}` || string(againRun.Metadata) != `{"version":1}` {
		t.Fatalf("stored run mutated: %#v", againRun)
	}

	snapshots, err := store.WorkflowSnapshots().ListWorkflowSnapshots(ctx, run.ID, runtime.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	snapshots.Records[0].State[0] = '['
	againSnapshots, err := store.WorkflowSnapshots().ListWorkflowSnapshots(ctx, run.ID, runtime.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if string(againSnapshots.Records[0].State) != `{"version":1}` {
		t.Fatalf("stored snapshot mutated: %#v", againSnapshots.Records[0])
	}

	ctxCancel, cancel := context.WithCancel(ctx)
	err = store.Transaction(ctxCancel, func(ctx context.Context, repositories runtime.Repositories) error {
		if err := repositories.Threads().UpdateThread(ctx, runtime.ThreadRecord{ID: "thread-1", Metadata: json.RawMessage(`{"version":2}`)}); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Transaction cancellation error = %v, want context.Canceled", err)
	}
	thread, err := store.Threads().GetThread(ctx, "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(thread.Metadata) != `{"version":1}` {
		t.Fatalf("transaction committed after cancellation: %#v", thread)
	}
}

func storageContractOuterReads(t *testing.T, newStore StoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)
	if err := store.Threads().CreateThread(ctx, runtime.ThreadRecord{ID: "thread-1"}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- store.Transaction(ctx, func(ctx context.Context, repositories runtime.Repositories) error {
			if _, err := store.Threads().GetThread(ctx, "thread-1"); err != nil {
				return err
			}
			return repositories.Messages().AppendMessages(ctx, []runtime.MessageRecord{{ID: "message-1", ThreadID: "thread-1", Message: runtime.Message{Role: runtime.RoleUser}}})
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Transaction deadlocked after outer-store read")
	}
}
