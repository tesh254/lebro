package lebro

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

var _ Store = (*MemoryStore)(nil)

func TestStorageRecordsRoundTripThroughJSON(t *testing.T) {
	t.Parallel()

	finished := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	records := []any{
		ThreadRecord{ID: "thread-1", Metadata: json.RawMessage(`{"tenant":"acme"}`), CreatedAt: finished, UpdatedAt: finished},
		MessageRecord{ID: "message-1", ThreadID: "thread-1", Message: Message{Role: RoleTool, Content: "{}", ToolCallID: "call-1"}, Metadata: json.RawMessage(`{"attempt":1}`), CreatedAt: finished},
		WorkflowRunRecord{ID: "run-1", WorkflowID: "workflow-1", ThreadID: "thread-1", Status: RunStatusSucceeded, Input: json.RawMessage(`{"input":"value"}`), Output: json.RawMessage(`{"output":true}`), Metadata: json.RawMessage(`{"source":"test"}`), StartedAt: finished, FinishedAt: &finished, UpdatedAt: finished},
		WorkflowSnapshotRecord{ID: "snapshot-1", RunID: "run-1", Sequence: 1, State: json.RawMessage(`{"step":"awaiting_tool"}`), CreatedAt: finished},
	}

	for _, record := range records {
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("Marshal(%T): %v", record, err)
		}
		var decoded any
		switch record.(type) {
		case ThreadRecord:
			decoded = &ThreadRecord{}
		case MessageRecord:
			decoded = &MessageRecord{}
		case WorkflowRunRecord:
			decoded = &WorkflowRunRecord{}
		case WorkflowSnapshotRecord:
			decoded = &WorkflowSnapshotRecord{}
		}
		if err := json.Unmarshal(encoded, decoded); err != nil {
			t.Fatalf("Unmarshal(%T): %v", record, err)
		}
		reencoded, err := json.Marshal(decoded)
		if err != nil {
			t.Fatalf("Marshal decoded %T: %v", record, err)
		}
		if string(reencoded) != string(encoded) {
			t.Fatalf("%T JSON changed after round trip:\n got %s\nwant %s", record, reencoded, encoded)
		}
	}
}

func TestMemoryStoreRepositoryContract(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	thread := ThreadRecord{ID: "thread-1", Metadata: json.RawMessage(`{"tag":"original"}`), CreatedAt: now, UpdatedAt: now}
	if err := store.Threads().CreateThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	thread.Metadata[0] = '[' // Stored records must not alias caller-owned JSON.
	gotThread, err := store.Threads().GetThread(ctx, "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(gotThread.Metadata) != `{"tag":"original"}` {
		t.Fatalf("thread metadata = %s, want original value", gotThread.Metadata)
	}

	messages := []MessageRecord{
		{ID: "message-1", ThreadID: "thread-1", Message: Message{Role: RoleUser, Content: "one"}, CreatedAt: now},
		{ID: "message-2", ThreadID: "thread-1", Message: Message{Role: RoleAssistant, Content: "two"}, CreatedAt: now},
		{ID: "message-3", ThreadID: "thread-1", Message: Message{Role: RoleTool, Content: "three", ToolCallID: "call-1"}, CreatedAt: now},
	}
	if err := store.Messages().AppendMessages(ctx, messages); err != nil {
		t.Fatal(err)
	}
	first, err := store.Messages().ListMessages(ctx, "thread-1", PageRequest{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Records) != 2 || first.NextCursor == "" {
		t.Fatalf("first page = %#v, want two records and a cursor", first)
	}
	first.Records[0].Message.Content = "mutated"
	again, err := store.Messages().ListMessages(ctx, "thread-1", PageRequest{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Records[0].Message.Content; got != "one" {
		t.Fatalf("stored message = %q, want one", got)
	}
	second, err := store.Messages().ListMessages(ctx, "thread-1", PageRequest{Cursor: first.NextCursor, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Records) != 1 || second.Records[0].ID != "message-3" || second.NextCursor != "" {
		t.Fatalf("second page = %#v, want final message only", second)
	}

	run := WorkflowRunRecord{ID: "run-1", WorkflowID: "workflow-1", Status: RunStatusRunning, StartedAt: now, UpdatedAt: now}
	if err := store.WorkflowRuns().SaveWorkflowRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := store.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, WorkflowSnapshotRecord{ID: "snapshot-2", RunID: "run-1", Sequence: 2, State: json.RawMessage(`{"index":2}`), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, WorkflowSnapshotRecord{ID: "snapshot-1", RunID: "run-1", Sequence: 1, State: json.RawMessage(`{"index":1}`), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	snapshots, err := store.WorkflowSnapshots().ListWorkflowSnapshots(ctx, "run-1", PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots.Records) != 2 || snapshots.Records[0].Sequence != 1 {
		t.Fatalf("snapshots = %#v, want ordered records", snapshots)
	}
}

func TestMemoryStoreTransactionRollsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	errRollback := errors.New("rollback")

	err := store.Transaction(ctx, func(ctx context.Context, repositories Repositories) error {
		if err := repositories.Threads().CreateThread(ctx, ThreadRecord{ID: "thread-1"}); err != nil {
			return err
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatalf("Transaction() error = %v, want rollback error", err)
	}
	if _, err := store.Threads().GetThread(ctx, "thread-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetThread after rollback error = %v, want ErrNotFound", err)
	}

	if err := store.Transaction(ctx, func(ctx context.Context, repositories Repositories) error {
		return repositories.Threads().CreateThread(ctx, ThreadRecord{ID: "thread-1"})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Threads().GetThread(ctx, "thread-1"); err != nil {
		t.Fatalf("GetThread after commit: %v", err)
	}
}

func TestMemoryStoreRejectsInvalidJSON(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Threads().CreateThread(ctx, ThreadRecord{ID: "thread-1", Metadata: json.RawMessage(`{`)}); err == nil {
		t.Fatal("CreateThread() error = nil, want invalid JSON error")
	}
}

func TestMemoryStoreRepositoryErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate(): %v", err)
	}
	if _, err := store.Threads().GetThread(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetThread missing error = %v, want ErrNotFound", err)
	}
	if err := store.Threads().CreateThread(ctx, ThreadRecord{}); err == nil {
		t.Fatal("CreateThread missing ID error = nil")
	}
	thread := ThreadRecord{ID: "thread-1"}
	if err := store.Threads().CreateThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	if err := store.Threads().CreateThread(ctx, thread); err == nil {
		t.Fatal("CreateThread duplicate error = nil")
	}
	if err := store.Threads().UpdateThread(ctx, ThreadRecord{ID: "missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateThread missing error = %v, want ErrNotFound", err)
	}
	thread.Metadata = json.RawMessage(`{"updated":true}`)
	if err := store.Threads().UpdateThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	if err := store.Threads().UpdateThread(ctx, ThreadRecord{ID: thread.ID, Metadata: json.RawMessage(`{`)}); err == nil {
		t.Fatal("UpdateThread invalid metadata error = nil")
	}

	for _, records := range [][]MessageRecord{
		{{ThreadID: "thread-1"}},
		{{ID: "message-1", ThreadID: "missing", Message: Message{Role: RoleUser}}},
		{{ID: "message-1", ThreadID: "thread-1", Message: Message{Role: "invalid"}}},
		{{ID: "message-1", ThreadID: "thread-1", Message: Message{Role: RoleUser}, Metadata: json.RawMessage(`{`)}},
		{{ID: "same", ThreadID: "thread-1", Message: Message{Role: RoleUser}}, {ID: "same", ThreadID: "thread-1", Message: Message{Role: RoleAssistant}}},
	} {
		if err := store.Messages().AppendMessages(ctx, records); err == nil {
			t.Fatalf("AppendMessages(%#v) error = nil", records)
		}
	}
	message := MessageRecord{ID: "message-1", ThreadID: "thread-1", Message: Message{Role: RoleUser}}
	if err := store.Messages().AppendMessages(ctx, []MessageRecord{message}); err != nil {
		t.Fatal(err)
	}
	if err := store.Messages().AppendMessages(ctx, []MessageRecord{message}); err == nil {
		t.Fatal("AppendMessages duplicate error = nil")
	}
	if _, err := store.Messages().ListMessages(ctx, "missing", PageRequest{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ListMessages missing error = %v, want ErrNotFound", err)
	}

	for _, run := range []WorkflowRunRecord{
		{},
		{ID: "run-1", WorkflowID: "workflow-1", Input: json.RawMessage(`{`)},
		{ID: "run-1", WorkflowID: "workflow-1", Metadata: json.RawMessage(`{`)},
	} {
		if err := store.WorkflowRuns().SaveWorkflowRun(ctx, run); err == nil {
			t.Fatalf("SaveWorkflowRun(%#v) error = nil", run)
		}
	}
	run := WorkflowRunRecord{ID: "run-1", WorkflowID: "workflow-1", Output: json.RawMessage(`{"ok":true}`)}
	if err := store.WorkflowRuns().SaveWorkflowRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, err := store.WorkflowRuns().GetWorkflowRun(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetWorkflowRun missing error = %v, want ErrNotFound", err)
	}
	if _, err := store.WorkflowRuns().GetWorkflowRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}

	for _, snapshot := range []WorkflowSnapshotRecord{
		{},
		{ID: "snapshot-1", RunID: "missing"},
		{ID: "snapshot-1", RunID: run.ID, State: json.RawMessage(`{`)},
	} {
		if err := store.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, snapshot); err == nil {
			t.Fatalf("SaveWorkflowSnapshot(%#v) error = nil", snapshot)
		}
	}
	snapshot := WorkflowSnapshotRecord{ID: "snapshot-1", RunID: run.ID, Sequence: 1}
	if err := store.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := store.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, snapshot); err == nil {
		t.Fatal("SaveWorkflowSnapshot duplicate error = nil")
	}
	if err := store.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, WorkflowSnapshotRecord{ID: "snapshot-2", RunID: run.ID, Sequence: 1}); err == nil {
		t.Fatal("SaveWorkflowSnapshot duplicate sequence error = nil")
	}
	if _, err := store.WorkflowSnapshots().ListWorkflowSnapshots(ctx, "missing", PageRequest{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ListWorkflowSnapshots missing error = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreTransactionRepositories(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()

	if err := store.Transaction(ctx, func(ctx context.Context, repositories Repositories) error {
		if err := repositories.Threads().CreateThread(ctx, ThreadRecord{ID: "thread-1"}); err != nil {
			return err
		}
		if err := repositories.Threads().UpdateThread(ctx, ThreadRecord{ID: "thread-1", Metadata: json.RawMessage(`{"tx":true}`)}); err != nil {
			return err
		}
		if _, err := repositories.Threads().GetThread(ctx, "thread-1"); err != nil {
			return err
		}
		if err := repositories.Messages().AppendMessages(ctx, []MessageRecord{{ID: "message-1", ThreadID: "thread-1", Message: Message{Role: RoleUser}}}); err != nil {
			return err
		}
		if _, err := repositories.Messages().ListMessages(ctx, "thread-1", PageRequest{}); err != nil {
			return err
		}
		if err := repositories.WorkflowRuns().SaveWorkflowRun(ctx, WorkflowRunRecord{ID: "run-1", WorkflowID: "workflow-1"}); err != nil {
			return err
		}
		if _, err := repositories.WorkflowRuns().GetWorkflowRun(ctx, "run-1"); err != nil {
			return err
		}
		if err := repositories.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, WorkflowSnapshotRecord{ID: "snapshot-1", RunID: "run-1"}); err != nil {
			return err
		}
		_, err := repositories.WorkflowSnapshots().ListWorkflowSnapshots(ctx, "run-1", PageRequest{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryStoreHonorsCanceledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := NewMemoryStore()
	if err := store.Threads().CreateThread(ctx, ThreadRecord{ID: "thread-1"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("CreateThread canceled error = %v, want context.Canceled", err)
	}
	if err := store.Transaction(ctx, func(context.Context, Repositories) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("Transaction canceled error = %v, want context.Canceled", err)
	}
}

func TestMemoryStoreDefensiveCopiesAndTransactionCancellation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Threads().CreateThread(ctx, ThreadRecord{ID: "thread-1", Metadata: json.RawMessage(`{"version":1}`)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Messages().AppendMessages(ctx, []MessageRecord{{ID: "message-1", ThreadID: "thread-1", Message: Message{Role: RoleUser}, Metadata: json.RawMessage(`{"version":1}`)}}); err != nil {
		t.Fatal(err)
	}
	run := WorkflowRunRecord{ID: "run-1", WorkflowID: "workflow-1", Input: json.RawMessage(`{"version":1}`), Metadata: json.RawMessage(`{"version":1}`)}
	if err := store.WorkflowRuns().SaveWorkflowRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := store.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, WorkflowSnapshotRecord{ID: "snapshot-1", RunID: run.ID, State: json.RawMessage(`{"version":1}`)}); err != nil {
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

	snapshots, err := store.WorkflowSnapshots().ListWorkflowSnapshots(ctx, run.ID, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	snapshots.Records[0].State[0] = '['
	againSnapshots, err := store.WorkflowSnapshots().ListWorkflowSnapshots(ctx, run.ID, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if string(againSnapshots.Records[0].State) != `{"version":1}` {
		t.Fatalf("stored snapshot mutated: %#v", againSnapshots.Records[0])
	}

	ctxCancel, cancel := context.WithCancel(ctx)
	err = store.Transaction(ctxCancel, func(ctx context.Context, repositories Repositories) error {
		if err := repositories.Threads().UpdateThread(ctx, ThreadRecord{ID: "thread-1", Metadata: json.RawMessage(`{"version":2}`)}); err != nil {
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

func TestMemoryStorePaginationBounds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Threads().CreateThread(ctx, ThreadRecord{ID: "thread-1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Messages().AppendMessages(ctx, []MessageRecord{{ID: "message-1", ThreadID: "thread-1", Message: Message{Role: RoleUser}}}); err != nil {
		t.Fatal(err)
	}
	page, err := store.Messages().ListMessages(ctx, "thread-1", PageRequest{Cursor: "100", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 0 || page.NextCursor != "" {
		t.Fatalf("out-of-range page = %#v", page)
	}
	page, err = store.Messages().ListMessages(ctx, "thread-1", PageRequest{Cursor: "-1", Limit: -1})
	if !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("negative page error = %v, want ErrInvalidPage", err)
	}
	if _, err := store.Messages().ListMessages(ctx, "thread-1", PageRequest{Cursor: "not-a-cursor"}); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("malformed cursor error = %v, want ErrInvalidPage", err)
	}
	if err := store.Messages().AppendMessages(ctx, []MessageRecord{{ID: "message-2", ThreadID: "thread-1", Message: Message{Role: RoleAssistant}}}); err != nil {
		t.Fatal(err)
	}
	page, err = store.Messages().ListMessages(ctx, "thread-1", PageRequest{Cursor: "1", Limit: int(^uint(0) >> 1)})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 || page.Records[0].ID != "message-2" {
		t.Fatalf("large-limit page = %#v", page)
	}
}

func TestMemoryStoreRepositoriesHonorCanceledContext(t *testing.T) {
	t.Parallel()
	ready := context.Background()
	store := NewMemoryStore()
	if err := store.Threads().CreateThread(ready, ThreadRecord{ID: "thread-1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.WorkflowRuns().SaveWorkflowRun(ready, WorkflowRunRecord{ID: "run-1", WorkflowID: "workflow-1"}); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ready)
	cancel()

	tests := []struct {
		name string
		call func() error
	}{
		{"get thread", func() error { _, err := store.Threads().GetThread(canceled, "thread-1"); return err }},
		{"update thread", func() error { return store.Threads().UpdateThread(canceled, ThreadRecord{ID: "thread-1"}) }},
		{"append messages", func() error {
			return store.Messages().AppendMessages(canceled, []MessageRecord{{ID: "message-1", ThreadID: "thread-1", Message: Message{Role: RoleUser}}})
		}},
		{"list messages", func() error { _, err := store.Messages().ListMessages(canceled, "thread-1", PageRequest{}); return err }},
		{"save run", func() error {
			return store.WorkflowRuns().SaveWorkflowRun(canceled, WorkflowRunRecord{ID: "run-2", WorkflowID: "workflow-1"})
		}},
		{"get run", func() error { _, err := store.WorkflowRuns().GetWorkflowRun(canceled, "run-1"); return err }},
		{"save snapshot", func() error {
			return store.WorkflowSnapshots().SaveWorkflowSnapshot(canceled, WorkflowSnapshotRecord{ID: "snapshot-1", RunID: "run-1"})
		}},
		{"list snapshots", func() error {
			_, err := store.WorkflowSnapshots().ListWorkflowSnapshots(canceled, "run-1", PageRequest{})
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
}

func TestMemoryStoreTransactionAllowsOuterStoreReads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Threads().CreateThread(ctx, ThreadRecord{ID: "thread-1"}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- store.Transaction(ctx, func(ctx context.Context, repositories Repositories) error {
			if _, err := store.Threads().GetThread(ctx, "thread-1"); err != nil {
				return err
			}
			return repositories.Messages().AppendMessages(ctx, []MessageRecord{{ID: "message-1", ThreadID: "thread-1", Message: Message{Role: RoleUser}}})
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Transaction deadlocked after outer-store read")
	}
}

func TestMemoryStoreTransactionDetectsConcurrentWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Threads().CreateThread(ctx, ThreadRecord{ID: "thread-1"}); err != nil {
		t.Fatal(err)
	}
	err := store.Transaction(ctx, func(ctx context.Context, repositories Repositories) error {
		if err := repositories.Messages().AppendMessages(ctx, []MessageRecord{{ID: "message-1", ThreadID: "thread-1", Message: Message{Role: RoleUser}}}); err != nil {
			return err
		}
		return store.Threads().UpdateThread(ctx, ThreadRecord{ID: "thread-1", Metadata: json.RawMessage(`{"source":"outer"}`)})
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Transaction conflict error = %v, want ErrConflict", err)
	}
	messages, err := store.Messages().ListMessages(ctx, "thread-1", PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages.Records) != 0 {
		t.Fatalf("conflicted transaction committed messages: %#v", messages)
	}
}

func TestMemoryStoreRejectsRecordsThatCannotRoundTripThroughJSON(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	invalidTime := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := store.Threads().CreateThread(ctx, ThreadRecord{ID: "thread-1", CreatedAt: invalidTime}); err == nil {
		t.Fatal("CreateThread invalid timestamp error = nil")
	}
	if _, err := store.Threads().GetThread(ctx, "thread-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetThread invalid timestamp error = %v, want ErrNotFound", err)
	}
}
