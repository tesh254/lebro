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
	t.Run("workflow run durable fields round-trip", func(t *testing.T) { storageContractWorkflowRunDurableFields(t, newStore) })
	t.Run("workflow runs list and filter", func(t *testing.T) { storageContractWorkflowRunList(t, newStore) })
	t.Run("thread namespace and owner round-trip", func(t *testing.T) { storageContractThreadNamespaceOwner(t, newStore) })
	t.Run("observability records", func(t *testing.T) { storageContractObservability(t, newStore) })
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
		{ID: "message-2", ThreadID: "thread-1", Message: runtime.Message{Role: runtime.RoleAssistant, Content: "two", Reasoning: runtime.ModelReasoning{Text: "considered options", Details: runtime.NewModelReasoningDetails(json.RawMessage(`[{"type":"thinking","signature":"opaque"}]`))}}, CreatedAt: now},
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
	if got := first.Records[1].Message.Reasoning; got.Text != "considered options" || string(got.Details.Raw()) != `[{"type":"thinking","signature":"opaque"}]` {
		t.Fatalf("stored reasoning = %#v", got)
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
	if err := store.Messages().UpdateMessages(ctx, []runtime.MessageRecord{{
		ID: "message-1", ThreadID: "thread-1", Message: runtime.Message{Role: runtime.RoleUser, Content: "updated"}, Metadata: json.RawMessage(`{"version":2}`),
	}}); err != nil {
		t.Fatal(err)
	}
	updated, err := store.Messages().ListMessages(ctx, "thread-1", runtime.PageRequest{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Records) != 1 || updated.Records[0].Message.Content != "updated" || !updated.Records[0].CreatedAt.Equal(now) {
		t.Fatalf("updated message = %#v, want changed content and original timestamp", updated)
	}
	if err := store.Messages().DeleteMessages(ctx, "thread-1", []string{"message-2", "missing"}); err != nil {
		t.Fatal(err)
	}
	remaining, err := store.Messages().ListMessages(ctx, "thread-1", runtime.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining.Records) != 2 || remaining.Records[0].ID != "message-1" || remaining.Records[1].ID != "message-3" {
		t.Fatalf("remaining messages = %#v", remaining)
	}
	if err := store.Messages().UpdateMessages(ctx, []runtime.MessageRecord{
		{ID: "message-1", ThreadID: "thread-1", Message: runtime.Message{Role: runtime.RoleUser, Content: "should roll back"}},
		{ID: "missing", ThreadID: "thread-1", Message: runtime.Message{Role: runtime.RoleUser, Content: "missing"}},
	}); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("batch update error = %v, want ErrNotFound", err)
	}
	afterRollback, err := store.Messages().ListMessages(ctx, "thread-1", runtime.PageRequest{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if afterRollback.Records[0].Message.Content != "updated" {
		t.Fatalf("partial batch update persisted: %#v", afterRollback)
	}

	// Message IDs are scoped per thread: the same ID may be appended to a
	// different thread.
	if err := store.Threads().CreateThread(ctx, runtime.ThreadRecord{ID: "thread-2"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Messages().AppendMessages(ctx, []runtime.MessageRecord{{
		ID: "message-1", ThreadID: "thread-2", Message: runtime.Message{Role: runtime.RoleAssistant, Content: "other thread"}, CreatedAt: now,
	}}); err != nil {
		t.Fatalf("same message ID in another thread rejected: %v", err)
	}

	// An empty append is a no-op, so callers may conditionally append slices
	// without checking their length.
	if err := store.Messages().AppendMessages(ctx, nil); err != nil {
		t.Fatalf("empty AppendMessages error = %v, want nil", err)
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

	// Snapshot IDs are scoped per run: the same ID and sequence may be used
	// in a different run.
	if err := store.WorkflowRuns().SaveWorkflowRun(ctx, runtime.WorkflowRunRecord{ID: "run-2", WorkflowID: "workflow-1", Status: runtime.RunStatusRunning, StartedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, runtime.WorkflowSnapshotRecord{
		ID: "snapshot-1", RunID: "run-2", Sequence: 1, State: json.RawMessage(`{"index":1}`), CreatedAt: now,
	}); err != nil {
		t.Fatalf("same snapshot ID in another run rejected: %v", err)
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
		if _, err := repositories.WorkflowRuns().ListWorkflowRuns(ctx, runtime.WorkflowRunFilter{}, runtime.PageRequest{}); err != nil {
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
		{"list runs", func() error {
			_, err := store.WorkflowRuns().ListWorkflowRuns(canceled, runtime.WorkflowRunFilter{}, runtime.PageRequest{})
			return err
		}},
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
	run := runtime.WorkflowRunRecord{ID: "run-1", WorkflowID: "workflow-1", Input: json.RawMessage(`{"version":1}`), Metadata: json.RawMessage(`{"version":1}`), StepOutputs: []json.RawMessage{json.RawMessage(`{"version":1}`)}, Failure: &runtime.WorkflowFailureData{Kind: runtime.WorkflowErrorStepFailed, Message: "boom"}}
	if err := store.WorkflowRuns().SaveWorkflowRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := store.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, runtime.WorkflowSnapshotRecord{ID: "snapshot-1", RunID: run.ID, SchemaVersion: 1, State: json.RawMessage(`{"version":1}`)}); err != nil {
		t.Fatal(err)
	}

	loadedRun, err := store.WorkflowRuns().GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	loadedRun.Input[0] = '['
	loadedRun.Metadata[0] = '['
	loadedRun.StepOutputs[0][0] = '['
	loadedRun.Failure.Message = "mutated"
	againRun, err := store.WorkflowRuns().GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(againRun.Input) != `{"version":1}` || string(againRun.Metadata) != `{"version":1}` {
		t.Fatalf("stored run mutated: %#v", againRun)
	}
	if string(againRun.StepOutputs[0]) != `{"version":1}` {
		t.Fatalf("stored run step output mutated: %q", againRun.StepOutputs[0])
	}
	if againRun.Failure == nil || againRun.Failure.Message != "boom" {
		t.Fatalf("stored run failure mutated: %#v", againRun.Failure)
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

	txCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- store.Transaction(txCtx, func(ctx context.Context, repositories runtime.Repositories) error {
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
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("Transaction did not unwind after cancellation")
		}
		t.Fatal("Transaction deadlocked after outer-store read")
	}
}

func storageContractThreadNamespaceOwner(t *testing.T, newStore StoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	thread := runtime.ThreadRecord{
		ID:        "thread-ns-1",
		Namespace: "tenant-acme",
		OwnerID:   "user-42",
		Metadata:  json.RawMessage(`{"tag":"ns"}`),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.Threads().CreateThread(ctx, thread); err != nil {
		t.Fatal(err)
	}

	got, err := store.Threads().GetThread(ctx, "thread-ns-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Namespace != "tenant-acme" || got.OwnerID != "user-42" {
		t.Fatalf("namespace/owner = %q/%q, want tenant-acme/user-42", got.Namespace, got.OwnerID)
	}

	if err := store.Threads().UpdateThread(ctx, runtime.ThreadRecord{
		ID:        "thread-ns-1",
		Namespace: "tenant-beta",
		OwnerID:   "user-99",
		Metadata:  json.RawMessage(`{"tag":"updated"}`),
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	updated, err := store.Threads().GetThread(ctx, "thread-ns-1")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Namespace != "tenant-beta" || updated.OwnerID != "user-99" {
		t.Fatalf("updated namespace/owner = %q/%q, want tenant-beta/user-99", updated.Namespace, updated.OwnerID)
	}
	if string(updated.Metadata) != `{"tag":"updated"}` {
		t.Fatalf("updated metadata = %s, want {\"tag\":\"updated\"}", updated.Metadata)
	}

	emptyThread := runtime.ThreadRecord{ID: "thread-empty-ns", CreatedAt: now, UpdatedAt: now}
	if err := store.Threads().CreateThread(ctx, emptyThread); err != nil {
		t.Fatal(err)
	}
	emptyGot, err := store.Threads().GetThread(ctx, "thread-empty-ns")
	if err != nil {
		t.Fatal(err)
	}
	if emptyGot.Namespace != "" || emptyGot.OwnerID != "" {
		t.Fatalf("empty namespace/owner = %q/%q, want empty", emptyGot.Namespace, emptyGot.OwnerID)
	}
}

func storageContractWorkflowRunDurableFields(t *testing.T, newStore StoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	finished := now.Add(5 * time.Minute)

	run := runtime.WorkflowRunRecord{
		ID:              "run-durable",
		WorkflowID:      "workflow-durable",
		ThreadID:        "thread-1",
		Status:          runtime.RunStatusSucceeded,
		Input:           json.RawMessage(`{"input":"value"}`),
		Output:          json.RawMessage(`{"output":"done"}`),
		StepOutputs:     []json.RawMessage{json.RawMessage(`{"step":1}`), json.RawMessage(`{"step":2}`)},
		CurrentStep:     2,
		CurrentStepID:   "step-2",
		Failure:         nil,
		WorkflowVersion: "v1",
		Metadata:        json.RawMessage(`{"source":"test"}`),
		StartedAt:       now,
		FinishedAt:      &finished,
		UpdatedAt:       finished,
	}
	if err := store.WorkflowRuns().SaveWorkflowRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	got, err := store.WorkflowRuns().GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkflowID != run.WorkflowID || got.Status != run.Status || got.WorkflowVersion != run.WorkflowVersion {
		t.Fatalf("run durable header = %#v, want %#v", got, run)
	}
	if string(got.Input) != string(run.Input) || string(got.Output) != string(run.Output) || string(got.Metadata) != string(run.Metadata) {
		t.Fatalf("run durable JSON = in %q out %q meta %q, want %q %q %q", got.Input, got.Output, got.Metadata, run.Input, run.Output, run.Metadata)
	}
	if len(got.StepOutputs) != len(run.StepOutputs) {
		t.Fatalf("step outputs len = %d, want %d", len(got.StepOutputs), len(run.StepOutputs))
	}
	for i, output := range run.StepOutputs {
		if string(got.StepOutputs[i]) != string(output) {
			t.Fatalf("step output %d = %q, want %q", i, got.StepOutputs[i], output)
		}
	}
	if got.CurrentStep != run.CurrentStep || got.CurrentStepID != run.CurrentStepID {
		t.Fatalf("current step = %d/%q, want %d/%q", got.CurrentStep, got.CurrentStepID, run.CurrentStep, run.CurrentStepID)
	}
	if got.FinishedAt == nil || !got.FinishedAt.Equal(finished) {
		t.Fatalf("finished at = %v, want %v", got.FinishedAt, finished)
	}

	// A failed run persists structured failure data.
	failed := runtime.WorkflowRunRecord{
		ID:            "run-failed",
		WorkflowID:    "workflow-durable",
		Status:        runtime.RunStatusFailed,
		CurrentStep:   1,
		CurrentStepID: "step-1",
		Failure: &runtime.WorkflowFailureData{
			Kind:    runtime.WorkflowErrorStepFailed,
			Step:    1,
			StepID:  "step-1",
			Message: "handler blew up",
		},
		StartedAt: now,
		UpdatedAt: now,
	}
	if err := store.WorkflowRuns().SaveWorkflowRun(ctx, failed); err != nil {
		t.Fatal(err)
	}
	gotFailed, err := store.WorkflowRuns().GetWorkflowRun(ctx, failed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotFailed.Failure == nil || gotFailed.Failure.Kind != failed.Failure.Kind || gotFailed.Failure.Step != failed.Failure.Step || gotFailed.Failure.StepID != failed.Failure.StepID || gotFailed.Failure.Message != failed.Failure.Message {
		t.Fatalf("failure = %#v, want %#v", gotFailed.Failure, failed.Failure)
	}

	// A suspended run round-trips with no FinishedAt so Resume can distinguish
	// a resumable run from a completed one.
	suspended := runtime.WorkflowRunRecord{
		ID:            "run-suspended",
		WorkflowID:    "workflow-durable",
		Status:        runtime.RunStatusSuspended,
		CurrentStep:   2,
		CurrentStepID: "await-approval",
		StepOutputs:   []json.RawMessage{json.RawMessage(`{"step":1}`)},
		StartedAt:     now,
		UpdatedAt:     now,
	}
	if err := store.WorkflowRuns().SaveWorkflowRun(ctx, suspended); err != nil {
		t.Fatal(err)
	}
	gotSuspended, err := store.WorkflowRuns().GetWorkflowRun(ctx, suspended.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotSuspended.Status != runtime.RunStatusSuspended {
		t.Fatalf("suspended status = %q, want suspended", gotSuspended.Status)
	}
	if gotSuspended.FinishedAt != nil {
		t.Fatalf("suspended FinishedAt = %v, want nil", gotSuspended.FinishedAt)
	}
	if gotSuspended.CurrentStep != 2 || gotSuspended.CurrentStepID != "await-approval" {
		t.Fatalf("suspended current step = %d/%q, want 2/await-approval", gotSuspended.CurrentStep, gotSuspended.CurrentStepID)
	}

	// A suspend snapshot round-trips with a suspend envelope so Resume can
	// recover the resume contract and payload.
	suspendState := json.RawMessage(`{"step":2,"step_id":"await-approval","outputs":[{"step":1}],"suspend":{"contract":{"approved":true},"payload":{"pending":"human"}}}`)
	suspendSnapshot := runtime.WorkflowSnapshotRecord{
		ID:            "snapshot-suspend-2",
		RunID:         suspended.ID,
		Sequence:      2,
		SchemaVersion: 2,
		State:         suspendState,
		CreatedAt:     now,
	}
	if err := store.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, suspendSnapshot); err != nil {
		t.Fatal(err)
	}
	gotSnapshots, err := store.WorkflowSnapshots().ListWorkflowSnapshots(ctx, suspended.ID, runtime.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(gotSnapshots.Records) != 1 || gotSnapshots.Records[0].SchemaVersion != 2 || string(gotSnapshots.Records[0].State) != string(suspendState) {
		t.Fatalf("suspend snapshot = %+v, want single record with schema v2", gotSnapshots.Records)
	}

	// An upsert with empty durable fields resets them rather than preserving
	// stale values, mirroring the whole-record replace semantics of the
	// other fields.
	if err := store.WorkflowRuns().SaveWorkflowRun(ctx, runtime.WorkflowRunRecord{
		ID: "run-durable", WorkflowID: "workflow-durable", Status: runtime.RunStatusCancelled, StartedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	reset, err := store.WorkflowRuns().GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reset.Status != runtime.RunStatusCancelled || reset.CurrentStep != 0 || reset.CurrentStepID != "" || len(reset.StepOutputs) != 0 || reset.Failure != nil || reset.WorkflowVersion != "" {
		t.Fatalf("upsert did not reset durable fields: %#v", reset)
	}
}

func storageContractWorkflowRunList(t *testing.T, newStore StoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	runs := []runtime.WorkflowRunRecord{
		{ID: "run-a", WorkflowID: "wf-1", Status: runtime.RunStatusSucceeded, StartedAt: now, UpdatedAt: now},
		{ID: "run-b", WorkflowID: "wf-1", Status: runtime.RunStatusFailed, StartedAt: now.Add(time.Minute), UpdatedAt: now},
		{ID: "run-c", WorkflowID: "wf-2", Status: runtime.RunStatusSucceeded, StartedAt: now.Add(2 * time.Minute), UpdatedAt: now},
	}
	for _, run := range runs {
		if err := store.WorkflowRuns().SaveWorkflowRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}

	all, err := store.WorkflowRuns().ListWorkflowRuns(ctx, runtime.WorkflowRunFilter{}, runtime.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Records) != len(runs) {
		t.Fatalf("ListWorkflowRuns() = %d records, want %d", len(all.Records), len(runs))
	}

	byWorkflow, err := store.WorkflowRuns().ListWorkflowRuns(ctx, runtime.WorkflowRunFilter{WorkflowID: "wf-1"}, runtime.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(byWorkflow.Records) != 2 {
		t.Fatalf("ListWorkflowRuns(wf-1) = %d records, want 2", len(byWorkflow.Records))
	}
	for _, got := range byWorkflow.Records {
		if got.WorkflowID != "wf-1" {
			t.Fatalf("ListWorkflowRuns(wf-1) returned %q", got.WorkflowID)
		}
	}

	byStatus, err := store.WorkflowRuns().ListWorkflowRuns(ctx, runtime.WorkflowRunFilter{Status: runtime.RunStatusSucceeded}, runtime.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(byStatus.Records) != 2 {
		t.Fatalf("ListWorkflowRuns(succeeded) = %d records, want 2", len(byStatus.Records))
	}
	for _, got := range byStatus.Records {
		if got.Status != runtime.RunStatusSucceeded {
			t.Fatalf("ListWorkflowRuns(succeeded) returned %q", got.Status)
		}
	}

	combined, err := store.WorkflowRuns().ListWorkflowRuns(ctx, runtime.WorkflowRunFilter{WorkflowID: "wf-1", Status: runtime.RunStatusFailed}, runtime.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(combined.Records) != 1 || combined.Records[0].ID != "run-b" {
		t.Fatalf("ListWorkflowRuns(wf-1, failed) = %#v, want [run-b]", combined.Records)
	}

	// Pagination bounds apply like the other list operations.
	if page, err := store.WorkflowRuns().ListWorkflowRuns(ctx, runtime.WorkflowRunFilter{}, runtime.PageRequest{Cursor: "not-a-cursor"}); !errors.Is(err, runtime.ErrInvalidPage) {
		t.Fatalf("ListWorkflowRuns bad cursor error = %v, want ErrInvalidPage, page = %#v", err, page)
	}
	paged, err := store.WorkflowRuns().ListWorkflowRuns(ctx, runtime.WorkflowRunFilter{}, runtime.PageRequest{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(paged.Records) != 2 || paged.NextCursor == "" {
		t.Fatalf("paged run list = %#v, want 2 records and a next cursor", paged)
	}
}

// storageContractObservability verifies the durable run-event, model-attempt,
// and tool-execution repositories: round-trips, filter vocabulary, ordering,
// pagination, validation, idempotent appends, transaction participation, and
// independence from the threads table.
func storageContractObservability(t *testing.T, newStore StoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)
	obs, ok := store.(runtime.ObservabilityRepositories)
	if !ok {
		t.Fatalf("store does not implement runtime.ObservabilityRepositories; observability support is mandatory for built-in adapters")
	}

	now := time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)
	metadata := runtime.Metadata{"app.customer_id": json.RawMessage(`"acme"`)}

	events := []runtime.RunEventRecord{
		{ID: "evt-1", RunID: "run-1", ThreadID: "thread-1", Sequence: 1, Type: runtime.RunEventStarted, Timestamp: now},
		{ID: "evt-2", RunID: "run-1", ThreadID: "thread-1", Sequence: 2, Type: runtime.RunEventModelFinished, Timestamp: now.Add(time.Second),
			FinishReason: runtime.FinishReasonStop, Usage: runtime.ModelUsage{InputTokens: 4, OutputTokens: 7}, DurationNanos: int64(time.Second)},
		{ID: "evt-3", RunID: "run-1", ThreadID: "thread-1", Sequence: 3, Type: runtime.RunEventSucceeded, Timestamp: now.Add(2 * time.Second), Status: runtime.RunStatusSucceeded},
		{ID: "evt-4", RunID: "run-2", ThreadID: "thread-1", Sequence: 1, Type: runtime.RunEventFailed, Timestamp: now.Add(3 * time.Second),
			Status: runtime.RunStatusFailed, ErrorKind: string(runtime.AgentErrorToolFailure), ErrorMessage: "lebro: agent tool failure: boom"},
		{ID: "evt-5", RunID: "run-2", ThreadID: "thread-1", Sequence: 2, Type: runtime.RunEventPlugin, Timestamp: now.Add(4 * time.Second),
			Payload: json.RawMessage(`{"decision":"compact"}`),
			Plugin:  &runtime.PluginAttribution{ID: "compactor", Version: "1.0.0", Action: "compact", Outcome: "approved"}},
	}
	for i := range events {
		events[i].Metadata = metadata
		events[i].Namespace = "tenant-a"
		events[i].OwnerID = "owner-a"
	}
	if err := obs.RunEvents().AppendRunEvents(ctx, events); err != nil {
		t.Fatalf("AppendRunEvents: %v", err)
	}
	if err := obs.RunEvents().AppendRunEvents(ctx, nil); err != nil {
		t.Fatalf("AppendRunEvents(nil): %v", err)
	}

	all, err := obs.RunEvents().ListRunEvents(ctx, runtime.RunEventFilter{ThreadID: "thread-1"}, runtime.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Records) != len(events) {
		t.Fatalf("ListRunEvents = %d records, want %d", len(all.Records), len(events))
	}
	for i, got := range all.Records {
		if want := events[i]; got.ID != want.ID || got.Type != want.Type || !got.Timestamp.Equal(want.Timestamp) {
			t.Fatalf("ListRunEvents[%d] = %#v, want %#v (ordering must follow run then sequence)", i, got, want)
		}
		if string(got.Metadata["app.customer_id"]) != `"acme"` {
			t.Fatalf("ListRunEvents[%d].Metadata = %#v, want namespaced metadata preserved", i, got.Metadata)
		}
	}
	paged, err := obs.RunEvents().ListRunEvents(ctx, runtime.RunEventFilter{ThreadID: "thread-1"}, runtime.PageRequest{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(paged.Records) != 2 || paged.NextCursor == "" {
		t.Fatalf("paged ListRunEvents = %#v, want 2 records and a cursor", paged)
	}

	byType, err := obs.RunEvents().ListRunEvents(ctx, runtime.RunEventFilter{Type: runtime.RunEventModelFinished}, runtime.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(byType.Records) != 1 || byType.Records[0].Usage.OutputTokens != 7 {
		t.Fatalf("ListRunEvents(type=model_finished) = %#v, want the usage-bearing event", byType.Records)
	}
	byRange, err := obs.RunEvents().ListRunEvents(ctx, runtime.RunEventFilter{From: now.Add(2 * time.Second), To: now.Add(4 * time.Second)}, runtime.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(byRange.Records) != 2 {
		t.Fatalf("ListRunEvents(time range) = %d records, want 2 (from inclusive, to exclusive)", len(byRange.Records))
	}
	byProvider, err := obs.RunEvents().ListRunEvents(ctx, runtime.RunEventFilter{Provider: "openai"}, runtime.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(byProvider.Records) != 0 {
		t.Fatalf("ListRunEvents(provider=openai) = %d records, want 0", len(byProvider.Records))
	}
	byScope, err := obs.RunEvents().ListRunEvents(ctx, runtime.RunEventFilter{Namespace: "tenant-a", OwnerID: "owner-a"}, runtime.PageRequest{})
	if err != nil || len(byScope.Records) != len(events) {
		t.Fatalf("ListRunEvents(scope) = %#v, %v; want scoped records", byScope, err)
	}

	attempts := []runtime.ModelAttemptRecord{
		{ID: "att-1", RunID: "run-1", ThreadID: "thread-1", Namespace: "tenant-a", OwnerID: "owner-a", StepID: "step-001", Step: 1, Index: 1,
			Provider: "openai", Model: "gpt-x", Status: runtime.ModelAttemptFailed,
			StartedAt: now, FinishedAt: now.Add(100 * time.Millisecond),
			ErrorKind: "rate_limited", ErrorMessage: "rate limited"},
		{ID: "att-2", RunID: "run-1", ThreadID: "thread-1", Namespace: "tenant-a", OwnerID: "owner-a", StepID: "step-001", Step: 1, Index: 2,
			Provider: "anthropic", Model: "claude-y", RoutedModel: "claude-y-latest", Status: runtime.ModelAttemptFallback,
			FinishReason: runtime.FinishReasonStop, Usage: runtime.ModelUsage{InputTokens: 11, OutputTokens: 5, ReasoningTokens: 2, TotalTokens: 18},
			StartedAt: now.Add(150 * time.Millisecond), FinishedAt: now.Add(900 * time.Millisecond),
			ProducedMessageIDs: []string{"run-1-msg-2"}, Metadata: metadata},
	}
	if err := obs.ModelAttempts().SaveModelAttempts(ctx, attempts); err != nil {
		t.Fatalf("SaveModelAttempts: %v", err)
	}
	gotAttempts, err := obs.ModelAttempts().ListModelAttempts(ctx, runtime.ModelAttemptFilter{RunID: "run-1"}, runtime.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(gotAttempts.Records) != 2 {
		t.Fatalf("ListModelAttempts = %d records, want 2 (every attempt is retained, not only the winner)", len(gotAttempts.Records))
	}
	if gotAttempts.Records[0].Status != runtime.ModelAttemptFailed || gotAttempts.Records[1].Usage.TotalTokens != 18 {
		t.Fatalf("ListModelAttempts round-trip mismatch: %#v", gotAttempts.Records)
	}
	if len(gotAttempts.Records[1].ProducedMessageIDs) != 1 || gotAttempts.Records[1].ProducedMessageIDs[0] != "run-1-msg-2" {
		t.Fatalf("attempt produced message IDs = %#v", gotAttempts.Records[1].ProducedMessageIDs)
	}
	fallbacks, err := obs.ModelAttempts().ListModelAttempts(ctx, runtime.ModelAttemptFilter{Status: runtime.ModelAttemptFallback}, runtime.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fallbacks.Records) != 1 || fallbacks.Records[0].ID != "att-2" {
		t.Fatalf("ListModelAttempts(status=fallback) = %#v", fallbacks.Records)
	}
	openai, err := obs.ModelAttempts().ListModelAttempts(ctx, runtime.ModelAttemptFilter{Provider: "openai"}, runtime.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(openai.Records) != 1 || openai.Records[0].ID != "att-1" {
		t.Fatalf("ListModelAttempts(provider=openai) = %#v", openai.Records)
	}
	if scoped, err := obs.ModelAttempts().ListModelAttempts(ctx, runtime.ModelAttemptFilter{Namespace: "tenant-a", OwnerID: "owner-a"}, runtime.PageRequest{}); err != nil || len(scoped.Records) != 2 {
		t.Fatalf("ListModelAttempts(scope) = %#v, %v", scoped, err)
	}

	tools := []runtime.ToolExecutionRecord{
		{ID: "tool-1", RunID: "run-1", ThreadID: "thread-1", Namespace: "tenant-a", OwnerID: "owner-a", Step: 1, StepID: "step-001",
			ToolCallID: "call-1", ToolID: "weather", State: runtime.ToolExecutionHandlerError,
			StartedAt: now.Add(time.Second), FinishedAt: now.Add(1200 * time.Millisecond),
			ErrorKind: string(runtime.ToolExecutionHandlerError), ErrorMessage: `lebro: tool "weather" execution handler_error: lookup failed`, Metadata: metadata},
		{ID: "tool-2", RunID: "run-1", ThreadID: "thread-1", Namespace: "tenant-a", OwnerID: "owner-a", Step: 1, StepID: "step-001",
			ToolCallID: "call-2", ToolID: "clock", State: runtime.ToolExecutionSucceeded,
			StartedAt: now.Add(1300 * time.Millisecond), FinishedAt: now.Add(1350 * time.Millisecond)},
	}
	if err := obs.ToolExecutions().SaveToolExecutions(ctx, tools); err != nil {
		t.Fatalf("SaveToolExecutions: %v", err)
	}
	gotTools, err := obs.ToolExecutions().ListToolExecutions(ctx, runtime.ToolExecutionFilter{RunID: "run-1"}, runtime.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(gotTools.Records) != 2 || gotTools.Records[0].State != runtime.ToolExecutionHandlerError {
		t.Fatalf("ListToolExecutions = %#v", gotTools.Records)
	}
	failed, err := obs.ToolExecutions().ListToolExecutions(ctx, runtime.ToolExecutionFilter{State: runtime.ToolExecutionHandlerError}, runtime.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(failed.Records) != 1 || failed.Records[0].ToolID != "weather" {
		t.Fatalf("ListToolExecutions(state=handler_error) = %#v", failed.Records)
	}
	if failed.Records[0].ErrorMessage == "" || failed.Records[0].FinishedAt.IsZero() {
		t.Fatalf("tool failure record must carry state, duration, and safe error data: %#v", failed.Records[0])
	}
	if scoped, err := obs.ToolExecutions().ListToolExecutions(ctx, runtime.ToolExecutionFilter{Namespace: "tenant-a", OwnerID: "owner-a"}, runtime.PageRequest{}); err != nil || len(scoped.Records) != 2 {
		t.Fatalf("ListToolExecutions(scope) = %#v, %v", scoped, err)
	}

	// Records must not require the thread to exist: failed runs persist
	// diagnostics before any transcript.
	orphan := runtime.RunEventRecord{ID: "evt-orphan", RunID: "run-orphan", Sequence: 1, Type: runtime.RunEventCancelled, Timestamp: now}
	if err := obs.RunEvents().AppendRunEvents(ctx, []runtime.RunEventRecord{orphan}); err != nil {
		t.Fatalf("AppendRunEvents without thread: %v", err)
	}

	// Duplicate appends are skipped, not errors: two independently constructed
	// agents sharing one store can reuse default run IDs.
	if err := obs.RunEvents().AppendRunEvents(ctx, []runtime.RunEventRecord{{ID: "evt-1", RunID: "run-1", Sequence: 1, Type: runtime.RunEventStarted, Timestamp: now}}); err != nil {
		t.Fatalf("duplicate AppendRunEvents must be idempotent: %v", err)
	}
	duped, err := obs.RunEvents().ListRunEvents(ctx, runtime.RunEventFilter{RunID: "run-1"}, runtime.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(duped.Records) != 3 {
		t.Fatalf("after duplicate append ListRunEvents = %d records, want 3", len(duped.Records))
	}

	// Validation parity with MemoryStore.
	invalid := []runtime.RunEventRecord{{ID: "", RunID: "run-1", Sequence: 1, Type: runtime.RunEventStarted, Timestamp: now}}
	if err := obs.RunEvents().AppendRunEvents(ctx, invalid); err == nil {
		t.Fatal("AppendRunEvents accepted an empty event ID")
	}
	badMeta := []runtime.RunEventRecord{{ID: "evt-bad", RunID: "run-1", Sequence: 9, Type: runtime.RunEventStarted, Timestamp: now,
		Metadata: runtime.Metadata{"noplural": json.RawMessage(`"x"`)}}}
	if err := obs.RunEvents().AppendRunEvents(ctx, badMeta); err == nil {
		t.Fatal("AppendRunEvents accepted non-namespaced metadata")
	}
	badStatus := []runtime.ModelAttemptRecord{{ID: "att-bad", RunID: "run-1", Index: 1, Status: "weird",
		StartedAt: now, FinishedAt: now}}
	if err := obs.ModelAttempts().SaveModelAttempts(ctx, badStatus); err == nil {
		t.Fatal("SaveModelAttempts accepted an invalid status")
	}
	badState := []runtime.ToolExecutionRecord{{ID: "tool-bad", RunID: "run-1", ToolCallID: "c", ToolID: "t",
		State: "exploded", StartedAt: now}}
	if err := obs.ToolExecutions().SaveToolExecutions(ctx, badState); err == nil {
		t.Fatal("SaveToolExecutions accepted an invalid state")
	}

	// Transaction participation: observability writes join the caller's
	// transaction and roll back with it.
	err = store.Transaction(ctx, func(ctx context.Context, repos runtime.Repositories) error {
		txObs, ok := repos.(runtime.ObservabilityRepositories)
		if !ok {
			t.Fatal("transaction repositories do not implement ObservabilityRepositories")
		}
		if err := txObs.RunEvents().AppendRunEvents(ctx, []runtime.RunEventRecord{{ID: "evt-tx", RunID: "run-tx", Sequence: 1, Type: runtime.RunEventStarted, Timestamp: now}}); err != nil {
			return err
		}
		return errors.New("rollback please")
	})
	if err == nil {
		t.Fatal("expected transaction error")
	}
	page, err := obs.RunEvents().ListRunEvents(ctx, runtime.RunEventFilter{RunID: "run-tx"}, runtime.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 0 {
		t.Fatalf("rolled-back event survived: %#v", page.Records)
	}

	// Defensive copies: mutating returned records must not affect stored data.
	page, err = obs.RunEvents().ListRunEvents(ctx, runtime.RunEventFilter{RunID: "run-1"}, runtime.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	page.Records[0].Payload = json.RawMessage(`{"tampered":true}`)
	page.Records[0].Metadata["app.customer_id"] = json.RawMessage(`"evil"`)
	again, err := obs.RunEvents().ListRunEvents(ctx, runtime.RunEventFilter{RunID: "run-1"}, runtime.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if string(again.Records[0].Metadata["app.customer_id"]) != `"acme"` {
		t.Fatalf("returned records alias stored state: %#v", again.Records[0].Metadata)
	}
}
