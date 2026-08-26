package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const postgresTestDSNEnv = "LEBRO_POSTGRES_TEST_DSN"

// skipIfNoPostgres skips the test when no disposable PostgreSQL instance is
// available via LEBRO_POSTGRES_TEST_DSN. This keeps the suite green in
// environments without a database while still exercising the adapter in CI
// and local runs that opt in.
func skipIfNoPostgres(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(postgresTestDSNEnv)
	if dsn == "" {
		t.Skipf("skipping PostgreSQL tests: set %s to a disposable database DSN", postgresTestDSNEnv)
	}
	return dsn
}

// newTestPostgresStore opens a store against the DSN, drops all tables so
// every test starts from a clean database, migrates, and registers cleanup.
// Dropping is simpler than per-test databases and mirrors how the contract
// suite expects a fresh schema.
func newTestPostgresStore(t *testing.T) *PostgresStore {
	t.Helper()
	dsn := skipIfNoPostgres(t)
	store, err := NewPostgresStore(dsn, PostgresStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	for _, table := range []string{"schedule_executions", "working_memory_facts", "schedules", "workflow_snapshots", "workflow_runs", "messages", "threads", "run_events", "model_attempts", "tool_executions", "schema_migrations"} {
		if _, err := store.db.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE", table)); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return store
}

var _ Store = (*PostgresStore)(nil)

func TestPostgresStoreFreshDatabaseMigrates(t *testing.T) {
	ctx := context.Background()
	store := newTestPostgresStore(t)
	var version int
	if err := store.db.QueryRowContext(ctx, postgresSchemaVersionQuery).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != len(postgresSchemaMigrations)-1 {
		t.Fatalf("schema version = %d, want %d", version, len(postgresSchemaMigrations)-1)
	}
	thread := ThreadRecord{ID: "thread-1"}
	if err := store.Threads().CreateThread(ctx, thread); err != nil {
		t.Fatalf("CreateThread after migrate: %v", err)
	}
}

func TestPostgresStorePersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dsn := skipIfNoPostgres(t)

	// Clean the database before the first open.
	cleanup, err := postgresDropAll(t, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	store, err := NewPostgresStore(dsn, PostgresStoreOptions{})
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

	reopened, err := NewPostgresStore(dsn, PostgresStoreOptions{})
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

func TestPostgresStoreMigrationFailuresLeaveDatabaseSafe(t *testing.T) {
	ctx := context.Background()

	t.Run("newer schema version is rejected", func(t *testing.T) {
		store := newTestPostgresStore(t)
		if _, err := store.db.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, 999); err != nil {
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
		store := newTestPostgresStore(t)
		// Drop all lebro tables so the conflicting threads table is the only
		// schema artifact when Migrate runs.
		for _, table := range []string{"schedule_executions", "working_memory_facts", "schedules", "workflow_snapshots", "workflow_runs", "messages", "threads", "run_events", "model_attempts", "tool_executions", "schema_migrations"} {
			if _, err := store.db.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE", table)); err != nil {
				t.Fatalf("drop %s: %v", table, err)
			}
		}
		if _, err := store.db.ExecContext(ctx, `CREATE TABLE threads (id TEXT PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		// schema_migrations was dropped above, so Migrate will recreate it
		// via its ensure step and then attempt migration 1 against the
		// conflicting threads table.
		err := store.Migrate(ctx)
		if err == nil || !strings.Contains(err.Error(), "migration 1") {
			t.Fatalf("Migrate() error = %v, want migration 1 failure", err)
		}
		var version int
		if err := store.db.QueryRowContext(ctx, postgresSchemaVersionQuery).Scan(&version); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				t.Fatal(err)
			}
			version = 0
		}
		if version != 0 {
			t.Fatalf("schema version = %d after failed migration, want 0", version)
		}
		var hasRuns bool
		rows, err := store.db.QueryContext(ctx, "SELECT to_regclass('workflow_runs')")
		if err != nil {
			t.Fatal(err)
		}
		if rows.Next() {
			var name *string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			hasRuns = name != nil
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
		if hasRuns {
			t.Fatal("failed migration left partial tables behind")
		}
	})
}

func TestPostgresStoreConcurrentTransactions(t *testing.T) {
	ctx := context.Background()
	store := newTestPostgresStore(t)
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

func TestPostgresStoreReadersDuringWrites(t *testing.T) {
	ctx := context.Background()
	store := newTestPostgresStore(t)
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

// postgresDropAll opens a connection to the DSN and drops all lebro tables,
// returning a cleanup function that closes the connection. It is used by
// tests that need a clean database across multiple store opens.
func postgresDropAll(t *testing.T, dsn string) (func(), error) {
	t.Helper()
	store, err := NewPostgresStore(dsn, PostgresStoreOptions{})
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	for _, table := range []string{"schedule_executions", "working_memory_facts", "schedules", "workflow_snapshots", "workflow_runs", "messages", "threads", "run_events", "model_attempts", "tool_executions", "schema_migrations"} {
		if _, err := store.db.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE", table)); err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("drop %s: %w", table, err)
		}
	}
	return func() { _ = store.Close() }, nil
}
