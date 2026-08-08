package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var _ Store = (*SQLiteStore)(nil)

func TestSQLiteStoreFreshDatabaseMigrates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestSQLiteStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate(): %v", err)
	}
	var version int
	if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != len(sqliteSchemaMigrations) {
		t.Fatalf("user_version = %d, want %d", version, len(sqliteSchemaMigrations))
	}
	thread := ThreadRecord{ID: "thread-1"}
	if err := store.Threads().CreateThread(ctx, thread); err != nil {
		t.Fatalf("CreateThread after migrate: %v", err)
	}
}

func TestSQLiteStorePersistsAcrossReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := testSQLitePath(t)

	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := timeUTC()
	thread := ThreadRecord{ID: "thread-1", Metadata: json.RawMessage(`{"tenant":"acme"}`), CreatedAt: now, UpdatedAt: now}
	if err := store.Transaction(ctx, func(ctx context.Context, repositories Repositories) error {
		if err := repositories.Threads().CreateThread(ctx, thread); err != nil {
			return err
		}
		if err := repositories.Messages().AppendMessages(ctx, []MessageRecord{{
			ID:        "message-1",
			ThreadID:  "thread-1",
			Message:   Message{Role: RoleUser, Content: "hello"},
			Metadata:  json.RawMessage(`{"attempt":1}`),
			CreatedAt: now,
		}}); err != nil {
			return err
		}
		return repositories.WorkflowRuns().SaveWorkflowRun(ctx, WorkflowRunRecord{
			ID:         "run-1",
			WorkflowID: "workflow-1",
			ThreadID:   "thread-1",
			Status:     RunStatusSucceeded,
			Input:      json.RawMessage(`{"input":"value"}`),
			Output:     json.RawMessage(`{"output":true}`),
			Metadata:   json.RawMessage(`{"source":"test"}`),
			StartedAt:  now,
			FinishedAt: &now,
			UpdatedAt:  now,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, WorkflowSnapshotRecord{
		ID: "snapshot-1", RunID: "run-1", Sequence: 1, State: json.RawMessage(`{"step":"done"}`), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	gotThread, err := reopened.Threads().GetThread(ctx, "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	assertEqualJSON(t, "thread", thread, gotThread)

	messages, err := reopened.Messages().ListMessages(ctx, "thread-1", PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages.Records) != 1 {
		t.Fatalf("messages = %d, want 1", len(messages.Records))
	}
	assertEqualJSON(t, "message", MessageRecord{
		ID:        "message-1",
		ThreadID:  "thread-1",
		Message:   Message{Role: RoleUser, Content: "hello"},
		Metadata:  json.RawMessage(`{"attempt":1}`),
		CreatedAt: now,
	}, messages.Records[0])

	gotRun, err := reopened.WorkflowRuns().GetWorkflowRun(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	assertEqualJSON(t, "workflow run", WorkflowRunRecord{
		ID: "run-1", WorkflowID: "workflow-1", ThreadID: "thread-1", Status: RunStatusSucceeded,
		Input: json.RawMessage(`{"input":"value"}`), Output: json.RawMessage(`{"output":true}`),
		Metadata: json.RawMessage(`{"source":"test"}`), StartedAt: now, FinishedAt: &now, UpdatedAt: now,
	}, gotRun)

	snapshots, err := reopened.WorkflowSnapshots().ListWorkflowSnapshots(ctx, "run-1", PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots.Records) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(snapshots.Records))
	}
	assertEqualJSON(t, "workflow snapshot", WorkflowSnapshotRecord{
		ID: "snapshot-1", RunID: "run-1", Sequence: 1, State: json.RawMessage(`{"step":"done"}`), CreatedAt: now,
	}, snapshots.Records[0])
}

func TestSQLiteStoreMigrationFailuresLeaveDatabaseSafe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("newer schema version is rejected", func(t *testing.T) {
		t.Parallel()
		store := newTestSQLiteStore(t)
		if err := store.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec("PRAGMA user_version = 999"); err != nil {
			t.Fatal(err)
		}
		err := store.Migrate(ctx)
		if err == nil || !strings.Contains(err.Error(), "newer") {
			t.Fatalf("Migrate() error = %v, want newer-schema error", err)
		}
		if _, err := store.Threads().GetThread(ctx, "missing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("database unusable after rejected migration: %v", err)
		}
	})

	t.Run("conflicting partial schema is rejected and unchanged", func(t *testing.T) {
		t.Parallel()
		store := newTestSQLiteStore(t)
		if _, err := store.db.Exec(`CREATE TABLE threads (id TEXT PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		err := store.Migrate(ctx)
		if err == nil || !strings.Contains(err.Error(), "migration 1") {
			t.Fatalf("Migrate() error = %v, want migration 1 failure", err)
		}
		var version int
		if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version != 0 {
			t.Fatalf("user_version = %d after failed migration, want 0", version)
		}
		var hasRuns bool
		rows, err := store.db.Query("SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'workflow_runs'")
		if err != nil {
			t.Fatal(err)
		}
		hasRuns = rows.Next()
		_ = rows.Close()
		if hasRuns {
			t.Fatal("failed migration left partial tables behind")
		}
	})
}

func TestSQLiteStoreConcurrentTransactions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestSQLiteStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const writers = 8
	const perWriter = 10
	if err := store.Threads().CreateThread(ctx, ThreadRecord{ID: "thread-1"}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			err := store.Transaction(ctx, func(ctx context.Context, repositories Repositories) error {
				records := make([]MessageRecord, 0, perWriter)
				for j := 0; j < perWriter; j++ {
					records = append(records, MessageRecord{
						ID:       fmt.Sprintf("writer-%d-%d", writer, j),
						ThreadID: "thread-1",
						Message:  Message{Role: RoleUser, Content: "hello"},
					})
				}
				return repositories.Messages().AppendMessages(ctx, records)
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent transaction failed: %v", err)
	}

	messages, err := store.Messages().ListMessages(ctx, "thread-1", PageRequest{Limit: int(^uint(0) >> 1)})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages.Records) != writers*perWriter {
		t.Fatalf("messages = %d, want %d", len(messages.Records), writers*perWriter)
	}
}

func TestSQLiteStoreReadersDuringWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestSQLiteStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Threads().CreateThread(ctx, ThreadRecord{ID: "thread-1"}); err != nil {
		t.Fatal(err)
	}
	records := make([]MessageRecord, 0, 50)
	for i := 0; i < 50; i++ {
		records = append(records, MessageRecord{ID: fmt.Sprintf("seed-%d", i), ThreadID: "thread-1", Message: Message{Role: RoleUser}})
	}
	if err := store.Messages().AppendMessages(ctx, records); err != nil {
		t.Fatal(err)
	}

	var writerErr atomic.Value
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for i := 0; i < 50; i++ {
			if err := store.Messages().AppendMessages(ctx, []MessageRecord{{
				ID: fmt.Sprintf("late-%d", i), ThreadID: "thread-1", Message: Message{Role: RoleUser},
			}}); err != nil {
				writerErr.Store(err)
				return
			}
		}
	}()

	for writerDone != nil {
		if _, err := store.Messages().ListMessages(ctx, "thread-1", PageRequest{Limit: 10}); err != nil {
			t.Fatalf("reader failed during concurrent write: %v", err)
		}
		select {
		case <-writerDone:
			writerDone = nil
		default:
		}
	}
	if err, ok := writerErr.Load().(error); ok {
		t.Fatalf("writer failed: %v", err)
	}
	messages, err := store.Messages().ListMessages(ctx, "thread-1", PageRequest{Limit: int(^uint(0) >> 1)})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages.Records) != 100 {
		t.Fatalf("messages = %d, want 100", len(messages.Records))
	}
}

// newTestSQLiteStore opens a store on a fresh temp file and registers cleanup.
func newTestSQLiteStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(testSQLitePath(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func testSQLitePath(t *testing.T) string {
	t.Helper()
	return t.TempDir() + "/test.db"
}

func timeUTC() time.Time {
	return time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
}

func assertEqualJSON(t *testing.T, name string, want, got any) {
	t.Helper()
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(wantJSON) != string(gotJSON) {
		t.Fatalf("%s changed after reopen:\n got %s\nwant %s", name, gotJSON, wantJSON)
	}
}
