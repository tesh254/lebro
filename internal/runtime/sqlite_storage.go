package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// SQLiteStore is a file-backed Store backed by SQLite. It preserves the same
// validation, pagination, and transaction semantics as MemoryStore, and keeps
// records durable across process restarts.
//
// The store expects its schema to be installed with Migrate before use. Writes
// are serialized by SQLite's transactional locking; a transaction that cannot
// acquire the write lock within the busy timeout reports ErrConflict so
// callers may retry, matching MemoryStore's optimistic-conflict contract.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore opens (or creates) the SQLite database at dsn and returns a
// store whose repositories share the database handle. The DSN may be a plain
// file path, a file: URI, or ":memory:". The database is left uninitialized;
// call Migrate to install the schema.
//
// A ":memory:" DSN gives every pooled connection its own private database,
// which would scatter records across connections, so such DSNs are pinned to
// a single connection; pass an explicit shared-cache URI
// (file::memory:?cache=shared) to share one in-memory database across the
// pool instead.
func NewSQLiteStore(dsn string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", sqliteDSN(dsn))
	if err != nil {
		return nil, fmt.Errorf("lebro: sqlite: open %q: %w", dsn, err)
	}
	// A private in-memory database gives every pooled connection its own
	// empty database, which would scatter records across connections, so
	// such DSNs are pinned to a single connection. Both ":memory:" and the
	// file:...?mode=memory URI form are private unless a shared cache is
	// requested explicitly (cache=shared).
	privateMemory := strings.Contains(dsn, ":memory:") || strings.Contains(dsn, "mode=memory")
	if privateMemory && !strings.Contains(dsn, "cache=shared") {
		db.SetMaxOpenConns(1)
	}
	if err := db.Ping(); err != nil {
		// Close best-effort so a failed open leaks nothing.
		_ = db.Close()
		return nil, fmt.Errorf("lebro: sqlite: connect to %q: %w", dsn, err)
	}
	return &SQLiteStore{db: db}, nil
}

// Close releases the underlying database handle.
func (s *SQLiteStore) Close() error { return s.db.Close() }

// sqliteDSN rewrites a caller DSN to enable the pragmas the adapter relies on:
// WAL journaling for concurrent readers during writes, foreign-key enforcement
// as a backstop to repository checks, and a busy timeout so lock contention
// surfaces as ErrConflict instead of silent failures.
func sqliteDSN(dsn string) string {
	const filePrefix = "file:"
	switch {
	case strings.HasPrefix(dsn, filePrefix), dsn == ":memory:":
		// Keep the caller's DSN.
	default:
		dsn = filePrefix + dsn
	}
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	// modernc.org/sqlite shorthand keys are validated and run per connection:
	// WAL journaling for concurrent readers during writes, foreign-key
	// enforcement as a backstop to repository checks, and a busy timeout so
	// lock contention surfaces as ErrConflict instead of silent failures.
	return dsn + separator + strings.Join([]string{
		"_journal_mode=WAL",
		"_foreign_keys=1",
		"_busy_timeout=5000",
	}, "&")
}

// sqliteSchemaMigrations installs the schema one statement at a time. The
// version is tracked in PRAGMA user_version and each migration runs inside the
// Migrate transaction, so a failed migration leaves the database untouched.
// Migrations must be append-only; never reorder or edit an applied step.
var sqliteSchemaMigrations = []string{
	`CREATE TABLE threads (
		id         TEXT PRIMARY KEY,
		metadata   TEXT,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE workflow_runs (
		id          TEXT PRIMARY KEY,
		workflow_id TEXT NOT NULL,
		thread_id   TEXT,
		status      TEXT NOT NULL,
		input       TEXT,
		output      TEXT,
		metadata    TEXT,
		started_at  TEXT NOT NULL,
		finished_at TEXT,
		updated_at  TEXT NOT NULL
	)`,
	`CREATE TABLE messages (
		id         TEXT NOT NULL,
		thread_id  TEXT NOT NULL REFERENCES threads(id),
		seq        INTEGER PRIMARY KEY AUTOINCREMENT,
		message    TEXT NOT NULL,
		metadata   TEXT,
		created_at TEXT NOT NULL,
		UNIQUE (thread_id, id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_thread_seq ON messages(thread_id, seq)`,
	`CREATE TABLE workflow_snapshots (
		id         TEXT NOT NULL,
		run_id     TEXT NOT NULL REFERENCES workflow_runs(id),
		sequence   INTEGER NOT NULL,
		state      TEXT NOT NULL,
		created_at TEXT NOT NULL,
		UNIQUE (run_id, id),
		UNIQUE (run_id, sequence)
	)`,
	`ALTER TABLE threads ADD COLUMN namespace TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE threads ADD COLUMN owner_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE workflow_runs ADD COLUMN current_step INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE workflow_runs ADD COLUMN current_step_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE workflow_runs ADD COLUMN step_outputs TEXT`,
	`ALTER TABLE workflow_runs ADD COLUMN failure TEXT`,
	`ALTER TABLE workflow_runs ADD COLUMN workflow_version TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE workflow_snapshots ADD COLUMN schema_version INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE workflow_runs ADD COLUMN path TEXT`,
	`ALTER TABLE workflow_runs ADD COLUMN fan_out TEXT`,
	`CREATE TABLE schedules (
		id           TEXT PRIMARY KEY,
		workflow_id  TEXT NOT NULL,
		spec         TEXT NOT NULL,
		paused       INTEGER NOT NULL DEFAULT 0,
		concurrency  TEXT NOT NULL DEFAULT '',
		input        TEXT,
		metadata     TEXT,
		next_fire_at TEXT,
		last_fire_at TEXT,
		created_at   TEXT NOT NULL,
		updated_at   TEXT NOT NULL
	)`,
	`CREATE TABLE schedule_executions (
		id            TEXT NOT NULL,
		schedule_id   TEXT NOT NULL REFERENCES schedules(id),
		run_id        TEXT,
		status        TEXT NOT NULL,
		scheduled_for TEXT NOT NULL,
		started_at    TEXT NOT NULL,
		finished_at   TEXT,
		error         TEXT NOT NULL DEFAULT '',
		seq           INTEGER PRIMARY KEY AUTOINCREMENT,
		UNIQUE (schedule_id, id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_schedule_executions_schedule_seq ON schedule_executions(schedule_id, seq)`,
	`CREATE TABLE working_memory_facts (
		id TEXT NOT NULL, namespace TEXT NOT NULL, owner_id TEXT NOT NULL, key TEXT NOT NULL,
		value TEXT NOT NULL, version INTEGER NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
		PRIMARY KEY (namespace, owner_id, key), UNIQUE (id)
	)`,
	`ALTER TABLE schedules ADD COLUMN wake_run_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE schedules ADD COLUMN wake_token TEXT NOT NULL DEFAULT ''`,
	`CREATE TABLE run_events (
		id               TEXT NOT NULL,
		run_id           TEXT NOT NULL,
		thread_id        TEXT NOT NULL DEFAULT '',
		seq              INTEGER NOT NULL,
		type             TEXT NOT NULL,
		timestamp        TEXT NOT NULL,
		step             INTEGER NOT NULL DEFAULT 0,
		step_id          TEXT NOT NULL DEFAULT '',
		parent_run_id    TEXT NOT NULL DEFAULT '',
		parent_step_id   TEXT NOT NULL DEFAULT '',
		branch           TEXT NOT NULL DEFAULT '',
		tool_call_id     TEXT NOT NULL DEFAULT '',
		tool_id          TEXT NOT NULL DEFAULT '',
		provider         TEXT NOT NULL DEFAULT '',
		provider_model   TEXT NOT NULL DEFAULT '',
		attempt_status   TEXT NOT NULL DEFAULT '',
		processor_phase  TEXT NOT NULL DEFAULT '',
		processor_action TEXT NOT NULL DEFAULT '',
		status           TEXT NOT NULL DEFAULT '',
		finish_reason    TEXT NOT NULL DEFAULT '',
		input_tokens     INTEGER NOT NULL DEFAULT 0,
		output_tokens    INTEGER NOT NULL DEFAULT 0,
		reasoning_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens     INTEGER NOT NULL DEFAULT 0,
		duration_ns      INTEGER NOT NULL DEFAULT 0,
		error_kind       TEXT NOT NULL DEFAULT '',
		error_message    TEXT NOT NULL DEFAULT '',
		payload          TEXT,
		plugin_id        TEXT NOT NULL DEFAULT '',
		plugin_version   TEXT NOT NULL DEFAULT '',
		plugin_action    TEXT NOT NULL DEFAULT '',
		plugin_outcome   TEXT NOT NULL DEFAULT '',
		annotations      TEXT,
		UNIQUE (run_id, id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_run_events_run_seq ON run_events(run_id, seq)`,
	`CREATE INDEX IF NOT EXISTS idx_run_events_thread_ts ON run_events(thread_id, timestamp)`,
	`CREATE INDEX IF NOT EXISTS idx_run_events_type ON run_events(type)`,
	`CREATE TABLE model_attempts (
		id                  TEXT NOT NULL,
		run_id              TEXT NOT NULL,
		thread_id           TEXT NOT NULL DEFAULT '',
		step                INTEGER NOT NULL DEFAULT 0,
		step_id             TEXT NOT NULL DEFAULT '',
		idx                 INTEGER NOT NULL,
		provider            TEXT NOT NULL DEFAULT '',
		model               TEXT NOT NULL DEFAULT '',
		routed_model        TEXT NOT NULL DEFAULT '',
		status              TEXT NOT NULL,
		finish_reason       TEXT NOT NULL DEFAULT '',
		input_tokens        INTEGER NOT NULL DEFAULT 0,
		output_tokens       INTEGER NOT NULL DEFAULT 0,
		reasoning_tokens    INTEGER NOT NULL DEFAULT 0,
		total_tokens        INTEGER NOT NULL DEFAULT 0,
		started_at          TEXT NOT NULL,
		finished_at         TEXT NOT NULL,
		message_ids         TEXT,
		error_kind          TEXT NOT NULL DEFAULT '',
		error_message       TEXT NOT NULL DEFAULT '',
		provider_request_id TEXT NOT NULL DEFAULT '',
		cost_micros         INTEGER NOT NULL DEFAULT 0,
		currency            TEXT NOT NULL DEFAULT '',
		annotations         TEXT,
		seq                 INTEGER PRIMARY KEY AUTOINCREMENT,
		UNIQUE (run_id, id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_model_attempts_run ON model_attempts(run_id, seq)`,
	`CREATE INDEX IF NOT EXISTS idx_model_attempts_thread ON model_attempts(thread_id, started_at)`,
	`CREATE INDEX IF NOT EXISTS idx_model_attempts_provider ON model_attempts(provider)`,
	`CREATE INDEX IF NOT EXISTS idx_model_attempts_status ON model_attempts(status)`,
	`CREATE TABLE tool_executions (
		id            TEXT NOT NULL,
		run_id        TEXT NOT NULL,
		thread_id     TEXT NOT NULL DEFAULT '',
		step          INTEGER NOT NULL DEFAULT 0,
		step_id       TEXT NOT NULL DEFAULT '',
		tool_call_id  TEXT NOT NULL,
		tool_id       TEXT NOT NULL,
		state         TEXT NOT NULL,
		started_at    TEXT NOT NULL,
		finished_at   TEXT,
		error_kind    TEXT NOT NULL DEFAULT '',
		error_message TEXT NOT NULL DEFAULT '',
		annotations   TEXT,
		seq           INTEGER PRIMARY KEY AUTOINCREMENT,
		UNIQUE (run_id, id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_tool_executions_run ON tool_executions(run_id, seq)`,
	`CREATE INDEX IF NOT EXISTS idx_tool_executions_thread ON tool_executions(thread_id, started_at)`,
	`CREATE INDEX IF NOT EXISTS idx_tool_executions_tool ON tool_executions(tool_id)`,
	`ALTER TABLE run_events ADD COLUMN namespace TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE run_events ADD COLUMN owner_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE model_attempts ADD COLUMN namespace TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE model_attempts ADD COLUMN owner_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE tool_executions ADD COLUMN namespace TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE tool_executions ADD COLUMN owner_id TEXT NOT NULL DEFAULT ''`,
}

// Migrate applies any pending schema migrations atomically. It is idempotent;
// a database already at the current version is a no-op. A failure rolls the
// transaction back, leaving the database unchanged and the error actionable
// (it names the failing migration).
func (s *SQLiteStore) Migrate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("lebro: sqlite: begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("lebro: sqlite: read schema version: %w", err)
	}
	if version > len(sqliteSchemaMigrations) {
		return fmt.Errorf("lebro: sqlite: database schema version %d is newer than this build supports (max %d)", version, len(sqliteSchemaMigrations))
	}
	for i := version; i < len(sqliteSchemaMigrations); i++ {
		if _, err := tx.ExecContext(ctx, sqliteSchemaMigrations[i]); err != nil {
			return fmt.Errorf("lebro: sqlite: migration %d (schema version %d) failed: %w; database left unchanged", i+1, i+1, err)
		}
	}
	if version < len(sqliteSchemaMigrations) {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", len(sqliteSchemaMigrations))); err != nil {
			return fmt.Errorf("lebro: sqlite: record schema version: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lebro: sqlite: commit migration: %w", sqliteError(err))
	}
	return nil
}

// Transaction runs fn against repositories bound to one SQLite transaction.
// The transaction begins with BEGIN IMMEDIATE, so concurrent writers
// serialize: a writer blocked for longer than the busy timeout reports
// ErrConflict, which callers may retry. A deferred (read-then-write)
// transaction would instead hit SQLite's upgrade deadlock case, which
// surfaces as an unretryable instant SQLITE_BUSY; IMMEDIATE avoids that path.
// A non-nil fn error or a canceled context rolls the transaction back.
func (s *SQLiteStore) Transaction(ctx context.Context, fn func(context.Context, Repositories) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("lebro: sqlite: acquire transaction connection: %w", sqliteError(err))
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		return fmt.Errorf("lebro: sqlite: begin transaction: %w", sqliteError(err))
	}
	repositories := &sqliteRepositories{q: conn}
	finished := false
	defer func() {
		if !finished {
			// ROLLBACK must run even if the context is canceled, so it uses a
			// fresh background context; a commit failure is the only path that
			// leaves the transaction in a finished state without COMMIT.
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
		_ = conn.Close()
	}()
	if err := fn(ctx, repositories); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("lebro: sqlite: commit transaction: %w", sqliteError(err))
	}
	finished = true
	return nil
}

var (
	_ RuntimeStore       = (*SQLiteStore)(nil)
	_ TranscriptStore    = (*SQLiteStore)(nil)
	_ WorkingMemoryStore = (*SQLiteStore)(nil)
	_ WorkflowStateStore = (*SQLiteStore)(nil)
	_ ScheduleStore      = (*SQLiteStore)(nil)
	_ ObservabilityStore = (*SQLiteStore)(nil)
	_ TransactionalStore = (*SQLiteStore)(nil)
)

// Capabilities reports full support: SQLiteStore implements every repository
// capability and transactional writes.
func (s *SQLiteStore) Capabilities() StoreCapabilities { return AllStoreCapabilities() }

// InTransaction implements TransactionalStore by running fn against the
// store's transaction boundary.
func (s *SQLiteStore) InTransaction(ctx context.Context, fn func(context.Context, RuntimeStore) error) error {
	return s.Transaction(ctx, func(ctx context.Context, repos Repositories) error {
		return fn(ctx, newRuntimeStoreView(s.Capabilities(), repos))
	})
}

func (s *SQLiteStore) Threads() ThreadRepository           { return &sqliteRepositories{q: s.db} }
func (s *SQLiteStore) Messages() MessageRepository         { return &sqliteRepositories{q: s.db} }
func (s *SQLiteStore) WorkflowRuns() WorkflowRunRepository { return &sqliteRepositories{q: s.db} }
func (s *SQLiteStore) WorkflowSnapshots() WorkflowSnapshotRepository {
	return &sqliteRepositories{q: s.db}
}
func (s *SQLiteStore) Schedules() ScheduleRepository { return &sqliteRepositories{q: s.db} }
func (s *SQLiteStore) ScheduleExecutions() ScheduleExecutionRepository {
	return &sqliteRepositories{q: s.db}
}
func (s *SQLiteStore) WorkingMemory() WorkingMemoryRepository { return &sqliteRepositories{q: s.db} }

// sqlQueryer is satisfied by both *sql.DB and *sql.Tx so the repositories work
// standalone and transaction-scoped.
type sqlQueryer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type sqliteRepositories struct {
	q sqlQueryer
}

func (r *sqliteRepositories) Threads() ThreadRepository                     { return r }
func (r *sqliteRepositories) Messages() MessageRepository                   { return r }
func (r *sqliteRepositories) WorkflowRuns() WorkflowRunRepository           { return r }
func (r *sqliteRepositories) WorkflowSnapshots() WorkflowSnapshotRepository { return r }
func (r *sqliteRepositories) Schedules() ScheduleRepository                 { return r }
func (r *sqliteRepositories) ScheduleExecutions() ScheduleExecutionRepository {
	return r
}
func (r *sqliteRepositories) WorkingMemory() WorkingMemoryRepository { return r }

func (r *sqliteRepositories) CreateThread(ctx context.Context, v ThreadRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if v.ID == "" {
		return errors.New("lebro: thread ID is required")
	}
	if err := validateJSON(v.Metadata); err != nil {
		return fmt.Errorf("lebro: thread metadata: %w", err)
	}
	if err := validateRecord(v); err != nil {
		return fmt.Errorf("lebro: thread: %w", err)
	}
	if _, err := r.q.ExecContext(ctx, `INSERT INTO threads (id, namespace, owner_id, metadata, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		v.ID, v.Namespace, v.OwnerID, sqliteJSON(v.Metadata), sqliteTime(v.CreatedAt), sqliteTime(v.UpdatedAt)); err != nil {
		return fmt.Errorf("lebro: create thread %q: %w", v.ID, sqliteError(err))
	}
	return nil
}

func (r *sqliteRepositories) GetThread(ctx context.Context, id ThreadID) (ThreadRecord, error) {
	if err := ctx.Err(); err != nil {
		return ThreadRecord{}, err
	}
	row := r.q.QueryRowContext(ctx, `SELECT id, namespace, owner_id, metadata, created_at, updated_at FROM threads WHERE id = ?`, id)
	record, err := scanThread(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadRecord{}, ErrNotFound
	}
	if err != nil {
		return ThreadRecord{}, fmt.Errorf("lebro: get thread %q: %w", id, sqliteError(err))
	}
	return record, nil
}

func (r *sqliteRepositories) UpdateThread(ctx context.Context, v ThreadRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.threadExists(ctx, v.ID); err != nil {
		return err
	}
	if err := validateJSON(v.Metadata); err != nil {
		return fmt.Errorf("lebro: thread metadata: %w", err)
	}
	if err := validateRecord(v); err != nil {
		return fmt.Errorf("lebro: thread: %w", err)
	}
	if _, err := r.q.ExecContext(ctx, `UPDATE threads SET namespace = ?, owner_id = ?, metadata = ?, updated_at = ? WHERE id = ?`,
		v.Namespace, v.OwnerID, sqliteJSON(v.Metadata), sqliteTime(v.UpdatedAt), v.ID); err != nil {
		return fmt.Errorf("lebro: update thread %q: %w", v.ID, sqliteError(err))
	}
	return nil
}

func (r *sqliteRepositories) AppendMessages(ctx context.Context, vs []MessageRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// An empty batch is a no-op, matching MemoryStore, and avoids building a
	// VALUES clause with no rows.
	if len(vs) == 0 {
		return nil
	}
	// Validate the whole batch before writing anything, mirroring
	// MemoryStore: message and thread IDs are required, the thread must
	// exist, the message role must be legal, JSON payloads must be valid,
	// and IDs must be unique per thread against both the batch and the
	// stored records.
	seen := make(map[ThreadID]map[string]struct{}, len(vs))
	existing := make(map[ThreadID]map[string]struct{}, len(vs))
	for _, v := range vs {
		if v.ID == "" || v.ThreadID == "" {
			return errors.New("lebro: message and thread IDs are required")
		}
		if err := r.threadExists(ctx, v.ThreadID); err != nil {
			return err
		}
		if err := v.Message.Validate(); err != nil {
			return fmt.Errorf("lebro: message %q: %w", v.ID, err)
		}
		if err := validateJSON(v.Metadata); err != nil {
			return fmt.Errorf("lebro: message metadata: %w", err)
		}
		if err := validateRecord(v); err != nil {
			return fmt.Errorf("lebro: message: %w", err)
		}
		if existing[v.ThreadID] == nil {
			var err error
			existing[v.ThreadID], err = r.messageIDs(ctx, v.ThreadID)
			if err != nil {
				return err
			}
		}
		if _, ok := existing[v.ThreadID][v.ID]; ok {
			return fmt.Errorf("lebro: message %q already exists", v.ID)
		}
		if seen[v.ThreadID] == nil {
			seen[v.ThreadID] = map[string]struct{}{}
		}
		if _, ok := seen[v.ThreadID][v.ID]; ok {
			return fmt.Errorf("lebro: message %q already exists", v.ID)
		}
		seen[v.ThreadID][v.ID] = struct{}{}
		existing[v.ThreadID][v.ID] = struct{}{}
	}
	// SQLite limits the number of bound parameters per statement, so the
	// append is split into bounded chunks. The chunks run inside one
	// transaction so the batch stays atomic: a duplicate that slipped in
	// after validation fails the whole append without partial writes.
	const chunkSize = 1000
	insert := func(q sqlQueryer) error {
		for start := 0; start < len(vs); start += chunkSize {
			end := start + chunkSize
			if end > len(vs) {
				end = len(vs)
			}
			chunk := vs[start:end]
			placeholders := make([]string, 0, len(chunk))
			args := make([]any, 0, len(chunk)*5)
			for _, v := range chunk {
				message, err := json.Marshal(v.Message)
				if err != nil {
					return fmt.Errorf("lebro: encode message %q: %w", v.ID, err)
				}
				placeholders = append(placeholders, "(?, ?, ?, ?, ?)")
				args = append(args, v.ID, v.ThreadID, string(message), sqliteJSON(v.Metadata), sqliteTime(v.CreatedAt))
			}
			if _, err := q.ExecContext(ctx, `INSERT INTO messages (id, thread_id, message, metadata, created_at) VALUES `+strings.Join(placeholders, ", "), args...); err != nil {
				return fmt.Errorf("lebro: append messages: %w", sqliteError(err))
			}
		}
		return nil
	}
	return r.withAutoTx(ctx, insert)
}

func (r *sqliteRepositories) UpdateMessages(ctx context.Context, vs []MessageRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(vs) == 0 {
		return nil
	}
	seen := make(map[ThreadID]map[string]struct{}, len(vs))
	return r.withAutoTx(ctx, func(q sqlQueryer) error {
		for _, v := range vs {
			if v.ID == "" || v.ThreadID == "" {
				return errors.New("lebro: message and thread IDs are required")
			}
			if err := v.Message.Validate(); err != nil {
				return fmt.Errorf("lebro: message %q: %w", v.ID, err)
			}
			if err := validateJSON(v.Metadata); err != nil {
				return fmt.Errorf("lebro: message metadata: %w", err)
			}
			if err := validateRecord(v); err != nil {
				return fmt.Errorf("lebro: message: %w", err)
			}
			if seen[v.ThreadID] == nil {
				seen[v.ThreadID] = map[string]struct{}{}
			}
			if _, duplicate := seen[v.ThreadID][v.ID]; duplicate {
				return fmt.Errorf("lebro: duplicate message %q", v.ID)
			}
			seen[v.ThreadID][v.ID] = struct{}{}
			message, err := json.Marshal(v.Message)
			if err != nil {
				return fmt.Errorf("lebro: encode message %q: %w", v.ID, err)
			}
			result, err := q.ExecContext(ctx, `UPDATE messages SET message = ?, metadata = ? WHERE id = ? AND thread_id = ?`, string(message), sqliteJSON(v.Metadata), v.ID, v.ThreadID)
			if err != nil {
				return fmt.Errorf("lebro: update message %q: %w", v.ID, sqliteError(err))
			}
			n, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("lebro: update message %q: %w", v.ID, sqliteError(err))
			}
			if n == 0 {
				return ErrNotFound
			}
		}
		return nil
	})
}

func (r *sqliteRepositories) DeleteMessages(ctx context.Context, id ThreadID, ids []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.threadExists(ctx, id); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	for _, messageID := range ids {
		if messageID == "" {
			return errors.New("lebro: message ID is required")
		}
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, id)
	for i, messageID := range ids {
		placeholders[i] = "?"
		args = append(args, messageID)
	}
	if _, err := r.q.ExecContext(ctx, `DELETE FROM messages WHERE thread_id = ? AND id IN (`+strings.Join(placeholders, ", ")+`)`, args...); err != nil {
		return fmt.Errorf("lebro: delete messages: %w", sqliteError(err))
	}
	return nil
}

// withAutoTx runs fn against the same queryer the repositories already use.
// When the repositories are standalone (backed by *sql.DB) it wraps fn in a
// BEGIN IMMEDIATE transaction so multi-statement operations like chunked
// appends stay atomic; when they are already inside a caller's Transaction
// (backed by a connection that holds BEGIN IMMEDIATE) fn runs against that
// transaction directly.
func (r *sqliteRepositories) withAutoTx(ctx context.Context, fn func(sqlQueryer) error) error {
	db, ok := r.q.(*sql.DB)
	if !ok {
		return fn(r.q)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("lebro: sqlite: acquire append connection: %w", sqliteError(err))
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		return fmt.Errorf("lebro: sqlite: begin append: %w", sqliteError(err))
	}
	finished := false
	defer func() {
		if !finished {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
		_ = conn.Close()
	}()
	if err := fn(conn); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("lebro: sqlite: commit append: %w", sqliteError(err))
	}
	finished = true
	return nil
}

// messageIDs loads the stored message IDs for a thread so AppendMessages can
// reject duplicates up front with the same error as MemoryStore, instead of
// deferring them to the UNIQUE constraint.
func (r *sqliteRepositories) messageIDs(ctx context.Context, id ThreadID) (map[string]struct{}, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT id FROM messages WHERE thread_id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("lebro: load message IDs for thread %q: %w", id, sqliteError(err))
	}
	defer func() { _ = rows.Close() }()
	ids := make(map[string]struct{})
	for rows.Next() {
		var messageID string
		if err := rows.Scan(&messageID); err != nil {
			return nil, fmt.Errorf("lebro: scan message ID: %w", sqliteError(err))
		}
		ids[messageID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lebro: load message IDs for thread %q: %w", id, sqliteError(err))
	}
	return ids, nil
}

func (r *sqliteRepositories) ListMessages(ctx context.Context, id ThreadID, p PageRequest) (Page[MessageRecord], error) {
	if err := ctx.Err(); err != nil {
		return Page[MessageRecord]{}, err
	}
	if err := r.threadExists(ctx, id); err != nil {
		return Page[MessageRecord]{}, err
	}
	offset, limit, err := sqlPageBounds(p)
	if err != nil {
		return Page[MessageRecord]{}, err
	}
	rows, err := r.q.QueryContext(ctx, `SELECT id, thread_id, message, metadata, created_at FROM messages WHERE thread_id = ? ORDER BY seq LIMIT ? OFFSET ?`, id, limit+1, offset)
	if err != nil {
		return Page[MessageRecord]{}, fmt.Errorf("lebro: list messages for thread %q: %w", id, sqliteError(err))
	}
	defer func() { _ = rows.Close() }()
	return scanMessagePage(rows, offset, limit)
}

func (r *sqliteRepositories) SaveWorkflowRun(ctx context.Context, v WorkflowRunRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if v.ID == "" || v.WorkflowID == "" {
		return errors.New("lebro: workflow run and workflow IDs are required")
	}
	for name, value := range map[string]json.RawMessage{"input": v.Input, "output": v.Output, "metadata": v.Metadata} {
		if err := validateJSON(value); err != nil {
			return fmt.Errorf("lebro: workflow run %s: %w", name, err)
		}
	}
	for i, output := range v.StepOutputs {
		if err := validateJSON(output); err != nil {
			return fmt.Errorf("lebro: workflow run step output %d: %w", i, err)
		}
	}
	if v.Failure != nil {
		if err := validateRecord(v.Failure); err != nil {
			return fmt.Errorf("lebro: workflow run failure: %w", err)
		}
	}
	if err := validateRecord(v); err != nil {
		return fmt.Errorf("lebro: workflow run: %w", err)
	}
	stepOutputs, err := sqliteJSONArray(v.StepOutputs)
	if err != nil {
		return fmt.Errorf("lebro: save workflow run %q: %w", v.ID, err)
	}
	failure, err := sqliteFailureJSON(v.Failure)
	if err != nil {
		return fmt.Errorf("lebro: save workflow run %q: %w", v.ID, err)
	}
	path, err := sqliteStepIDArray(v.Path)
	if err != nil {
		return fmt.Errorf("lebro: save workflow run %q: %w", v.ID, err)
	}
	fanOut, err := sqliteFanOutJSON(v.FanOut)
	if err != nil {
		return fmt.Errorf("lebro: save workflow run %q: %w", v.ID, err)
	}
	if _, err := r.q.ExecContext(ctx, `INSERT INTO workflow_runs (id, workflow_id, thread_id, status, input, output, metadata, started_at, finished_at, updated_at, current_step, current_step_id, step_outputs, failure, workflow_version, path, fan_out)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			workflow_id     = excluded.workflow_id,
			thread_id       = excluded.thread_id,
			status          = excluded.status,
			input           = excluded.input,
			output          = excluded.output,
			metadata        = excluded.metadata,
			started_at      = excluded.started_at,
			finished_at     = excluded.finished_at,
			updated_at      = excluded.updated_at,
			current_step    = excluded.current_step,
			current_step_id = excluded.current_step_id,
			step_outputs    = excluded.step_outputs,
			failure         = excluded.failure,
			workflow_version= excluded.workflow_version,
			path            = excluded.path,
			fan_out         = excluded.fan_out`,
		v.ID, v.WorkflowID, sqliteNullableString(string(v.ThreadID)), v.Status, sqliteJSON(v.Input), sqliteJSON(v.Output), sqliteJSON(v.Metadata), sqliteTime(v.StartedAt), sqliteNullableTime(v.FinishedAt), sqliteTime(v.UpdatedAt),
		v.CurrentStep, string(v.CurrentStepID), stepOutputs, failure, v.WorkflowVersion, path, fanOut); err != nil {
		return fmt.Errorf("lebro: save workflow run %q: %w", v.ID, sqliteError(err))
	}
	return nil
}

func (r *sqliteRepositories) GetWorkflowRun(ctx context.Context, id RunID) (WorkflowRunRecord, error) {
	if err := ctx.Err(); err != nil {
		return WorkflowRunRecord{}, err
	}
	row := r.q.QueryRowContext(ctx, `SELECT id, workflow_id, thread_id, status, input, output, metadata, started_at, finished_at, updated_at, current_step, current_step_id, step_outputs, failure, workflow_version, path, fan_out FROM workflow_runs WHERE id = ?`, id)
	record, err := scanWorkflowRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkflowRunRecord{}, ErrNotFound
	}
	if err != nil {
		return WorkflowRunRecord{}, fmt.Errorf("lebro: get workflow run %q: %w", id, sqliteError(err))
	}
	return record, nil
}

func (r *sqliteRepositories) ListWorkflowRuns(ctx context.Context, filter WorkflowRunFilter, p PageRequest) (Page[WorkflowRunRecord], error) {
	if err := ctx.Err(); err != nil {
		return Page[WorkflowRunRecord]{}, err
	}
	offset, limit, err := sqlPageBounds(p)
	if err != nil {
		return Page[WorkflowRunRecord]{}, err
	}
	var (
		query = `SELECT id, workflow_id, thread_id, status, input, output, metadata, started_at, finished_at, updated_at, current_step, current_step_id, step_outputs, failure, workflow_version, path, fan_out FROM workflow_runs`
		args  []any
		where []string
	)
	if filter.WorkflowID != "" {
		where = append(where, "workflow_id = ?")
		args = append(args, filter.WorkflowID)
	}
	if filter.Status != "" {
		where = append(where, "status = ?")
		args = append(args, filter.Status)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY started_at, id LIMIT ? OFFSET ?"
	args = append(args, limit+1, offset)
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return Page[WorkflowRunRecord]{}, fmt.Errorf("lebro: list workflow runs: %w", sqliteError(err))
	}
	defer func() { _ = rows.Close() }()
	return scanWorkflowRunPage(rows, offset, limit)
}

func (r *sqliteRepositories) SaveWorkflowSnapshot(ctx context.Context, v WorkflowSnapshotRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if v.ID == "" || v.RunID == "" {
		return errors.New("lebro: workflow snapshot and run IDs are required")
	}
	if err := validateJSON(v.State); err != nil {
		return fmt.Errorf("lebro: workflow snapshot state: %w", err)
	}
	if err := validateRecord(v); err != nil {
		return fmt.Errorf("lebro: workflow snapshot: %w", err)
	}
	if err := r.runExists(ctx, v.RunID); err != nil {
		return err
	}
	// Snapshot IDs are scoped per run like MemoryStore's, so the same ID may
	// be reused across runs; duplicates within a run are rejected here with
	// the memory adapter's error instead of deferring to the constraints.
	var found string
	switch err := r.q.QueryRowContext(ctx, `SELECT id FROM workflow_snapshots WHERE run_id = ? AND id = ?`, v.RunID, v.ID).Scan(&found); {
	case err == nil:
		return errors.New("lebro: workflow snapshot already exists")
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("lebro: check workflow snapshot %q: %w", v.ID, sqliteError(err))
	}
	var foundSequence int64
	switch err := r.q.QueryRowContext(ctx, `SELECT sequence FROM workflow_snapshots WHERE run_id = ? AND sequence = ?`, v.RunID, v.Sequence).Scan(&foundSequence); {
	case err == nil:
		return errors.New("lebro: workflow snapshot already exists")
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("lebro: check workflow snapshot sequence %d: %w", v.Sequence, sqliteError(err))
	}
	if _, err := r.q.ExecContext(ctx, `INSERT INTO workflow_snapshots (id, run_id, sequence, schema_version, state, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		v.ID, v.RunID, v.Sequence, v.SchemaVersion, string(v.State), sqliteTime(v.CreatedAt)); err != nil {
		return fmt.Errorf("lebro: save workflow snapshot %q: %w", v.ID, sqliteError(err))
	}
	return nil
}

func (r *sqliteRepositories) ListWorkflowSnapshots(ctx context.Context, id RunID, p PageRequest) (Page[WorkflowSnapshotRecord], error) {
	if err := ctx.Err(); err != nil {
		return Page[WorkflowSnapshotRecord]{}, err
	}
	if err := r.runExists(ctx, id); err != nil {
		return Page[WorkflowSnapshotRecord]{}, err
	}
	offset, limit, err := sqlPageBounds(p)
	if err != nil {
		return Page[WorkflowSnapshotRecord]{}, err
	}
	rows, err := r.q.QueryContext(ctx, `SELECT id, run_id, sequence, schema_version, state, created_at FROM workflow_snapshots WHERE run_id = ? ORDER BY sequence LIMIT ? OFFSET ?`, id, limit+1, offset)
	if err != nil {
		return Page[WorkflowSnapshotRecord]{}, fmt.Errorf("lebro: list workflow snapshots for run %q: %w", id, sqliteError(err))
	}
	defer func() { _ = rows.Close() }()
	return scanSnapshotPage(rows, offset, limit)
}

func (r *sqliteRepositories) SaveSchedule(ctx context.Context, v ScheduleRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if v.ID == "" || v.WorkflowID == "" {
		return errors.New("lebro: schedule and workflow IDs are required")
	}
	if v.Spec == "" {
		return errors.New("lebro: schedule spec is required")
	}
	for name, value := range map[string]json.RawMessage{"input": v.Input, "metadata": v.Metadata} {
		if err := validateJSON(value); err != nil {
			return fmt.Errorf("lebro: schedule %s: %w", name, err)
		}
	}
	if err := validateRecord(v); err != nil {
		return fmt.Errorf("lebro: schedule: %w", err)
	}
	if _, err := r.q.ExecContext(ctx, `INSERT INTO schedules (id, workflow_id, spec, paused, concurrency, input, metadata, next_fire_at, last_fire_at, wake_run_id, wake_token, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			workflow_id  = excluded.workflow_id,
			spec         = excluded.spec,
			paused       = excluded.paused,
			concurrency  = excluded.concurrency,
			input        = excluded.input,
			metadata     = excluded.metadata,
			next_fire_at = excluded.next_fire_at,
			last_fire_at = excluded.last_fire_at,
			wake_run_id  = excluded.wake_run_id,
			wake_token   = excluded.wake_token,
			created_at   = excluded.created_at,
			updated_at   = excluded.updated_at`,
		v.ID, v.WorkflowID, v.Spec, sqliteBool(v.Paused), string(v.Concurrency), sqliteJSON(v.Input), sqliteJSON(v.Metadata),
		sqliteNullableTime(v.NextFireAt), sqliteNullableTime(v.LastFireAt), v.WakeRunID, v.WakeToken, sqliteTime(v.CreatedAt), sqliteTime(v.UpdatedAt)); err != nil {
		return fmt.Errorf("lebro: save schedule %q: %w", v.ID, sqliteError(err))
	}
	return nil
}

func (r *sqliteRepositories) GetSchedule(ctx context.Context, id ScheduleID) (ScheduleRecord, error) {
	if err := ctx.Err(); err != nil {
		return ScheduleRecord{}, err
	}
	row := r.q.QueryRowContext(ctx, `SELECT id, workflow_id, spec, paused, concurrency, input, metadata, next_fire_at, last_fire_at, wake_run_id, wake_token, created_at, updated_at FROM schedules WHERE id = ?`, id)
	record, err := scanSchedule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ScheduleRecord{}, ErrNotFound
	}
	if err != nil {
		return ScheduleRecord{}, fmt.Errorf("lebro: get schedule %q: %w", id, sqliteError(err))
	}
	return record, nil
}

func (r *sqliteRepositories) ListSchedules(ctx context.Context, filter ScheduleFilter, p PageRequest) (Page[ScheduleRecord], error) {
	if err := ctx.Err(); err != nil {
		return Page[ScheduleRecord]{}, err
	}
	offset, limit, err := sqlPageBounds(p)
	if err != nil {
		return Page[ScheduleRecord]{}, err
	}
	var (
		query = `SELECT id, workflow_id, spec, paused, concurrency, input, metadata, next_fire_at, last_fire_at, wake_run_id, wake_token, created_at, updated_at FROM schedules`
		args  []any
		where []string
	)
	if filter.WorkflowID != "" {
		where = append(where, "workflow_id = ?")
		args = append(args, filter.WorkflowID)
	}
	if filter.DueBy != nil {
		// Due work is non-paused, has a next fire time, and it is at or before
		// the cutoff. A NULL next_fire_at (unscheduled) never matches.
		where = append(where, "paused = 0", "next_fire_at IS NOT NULL", "next_fire_at <= ?")
		args = append(args, sqliteTime(*filter.DueBy))
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY created_at, id LIMIT ? OFFSET ?"
	args = append(args, limit+1, offset)
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return Page[ScheduleRecord]{}, fmt.Errorf("lebro: list schedules: %w", sqliteError(err))
	}
	defer func() { _ = rows.Close() }()
	return scanSchedulePage(rows, offset, limit)
}

func (r *sqliteRepositories) DeleteSchedule(ctx context.Context, id ScheduleID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// The schedule_executions foreign key has no ON DELETE CASCADE (the schema
	// migration is append-only and predates it), so the child history is
	// removed first. Both deletes run in one transaction so a schedule with
	// history is never left half-deleted.
	var missing bool
	err := r.withAutoTx(ctx, func(q sqlQueryer) error {
		if _, err := q.ExecContext(ctx, `DELETE FROM schedule_executions WHERE schedule_id = ?`, id); err != nil {
			return fmt.Errorf("lebro: delete schedule executions %q: %w", id, sqliteError(err))
		}
		result, err := q.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("lebro: delete schedule %q: %w", id, sqliteError(err))
		}
		n, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("lebro: delete schedule %q: %w", id, sqliteError(err))
		}
		missing = n == 0
		return nil
	})
	if err != nil {
		return err
	}
	if missing {
		return ErrNotFound
	}
	return nil
}

func (r *sqliteRepositories) SaveScheduleExecution(ctx context.Context, v ScheduleExecutionRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if v.ID == "" || v.ScheduleID == "" {
		return errors.New("lebro: schedule execution and schedule IDs are required")
	}
	if err := validateRecord(v); err != nil {
		return fmt.Errorf("lebro: schedule execution: %w", err)
	}
	if err := r.scheduleExists(ctx, v.ScheduleID); err != nil {
		return err
	}
	var found string
	switch err := r.q.QueryRowContext(ctx, `SELECT id FROM schedule_executions WHERE schedule_id = ? AND id = ?`, v.ScheduleID, string(v.ID)).Scan(&found); {
	case err == nil:
		return errors.New("lebro: schedule execution already exists")
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("lebro: check schedule execution %q: %w", v.ID, sqliteError(err))
	}
	if _, err := r.q.ExecContext(ctx, `INSERT INTO schedule_executions (id, schedule_id, run_id, status, scheduled_for, started_at, finished_at, error) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		string(v.ID), v.ScheduleID, sqliteNullableString(string(v.RunID)), string(v.Status), sqliteTime(v.ScheduledFor), sqliteTime(v.StartedAt), sqliteNullableTime(v.FinishedAt), v.Error); err != nil {
		return fmt.Errorf("lebro: save schedule execution %q: %w", v.ID, sqliteError(err))
	}
	return nil
}

func (r *sqliteRepositories) ListScheduleExecutions(ctx context.Context, id ScheduleID, p PageRequest) (Page[ScheduleExecutionRecord], error) {
	if err := ctx.Err(); err != nil {
		return Page[ScheduleExecutionRecord]{}, err
	}
	if err := r.scheduleExists(ctx, id); err != nil {
		return Page[ScheduleExecutionRecord]{}, err
	}
	offset, limit, err := sqlPageBounds(p)
	if err != nil {
		return Page[ScheduleExecutionRecord]{}, err
	}
	rows, err := r.q.QueryContext(ctx, `SELECT id, schedule_id, run_id, status, scheduled_for, started_at, finished_at, error FROM schedule_executions WHERE schedule_id = ? ORDER BY seq LIMIT ? OFFSET ?`, id, limit+1, offset)
	if err != nil {
		return Page[ScheduleExecutionRecord]{}, fmt.Errorf("lebro: list schedule executions for schedule %q: %w", id, sqliteError(err))
	}
	defer func() { _ = rows.Close() }()
	return scanScheduleExecutionPage(rows, offset, limit)
}

func (r *sqliteRepositories) scheduleExists(ctx context.Context, id ScheduleID) error {
	var found int
	err := r.q.QueryRowContext(ctx, `SELECT 1 FROM schedules WHERE id = ?`, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lebro: sqlite: %w", sqliteError(err))
	}
	return nil
}

// threadExists and runExists reproduce the parent-existence checks that
// MemoryStore performs before accepting child records.
func (r *sqliteRepositories) threadExists(ctx context.Context, id ThreadID) error {
	var found int
	err := r.q.QueryRowContext(ctx, `SELECT 1 FROM threads WHERE id = ?`, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lebro: sqlite: %w", sqliteError(err))
	}
	return nil
}

func (r *sqliteRepositories) runExists(ctx context.Context, id RunID) error {
	var found int
	err := r.q.QueryRowContext(ctx, `SELECT 1 FROM workflow_runs WHERE id = ?`, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lebro: sqlite: %w", sqliteError(err))
	}
	return nil
}

// sqliteError normalizes driver errors into the storage error vocabulary.
func sqliteError(err error) error {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return err
	}
	switch sqliteErr.Code() {
	case sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY:
		return ErrNotFound
	case sqlite3.SQLITE_BUSY:
		return ErrConflict
	default:
		return fmt.Errorf("%w (sqlite %d)", err, sqliteErr.Code())
	}
}

func sqlPageBounds(p PageRequest) (offset, limit int, err error) {
	if p.Cursor != "" {
		offset, err = strconv.Atoi(p.Cursor)
		if err != nil || offset < 0 {
			return 0, 0, ErrInvalidPage
		}
	}
	limit = p.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 0 {
		return 0, 0, ErrInvalidPage
	}
	return offset, limit, nil
}

type messagePageScanner interface{ Scan(dest ...any) error }

func scanThread(row messagePageScanner) (ThreadRecord, error) {
	var record ThreadRecord
	var metadata sql.NullString
	var createdAt, updatedAt string
	if err := row.Scan(&record.ID, &record.Namespace, &record.OwnerID, &metadata, &createdAt, &updatedAt); err != nil {
		return ThreadRecord{}, err
	}
	record.Metadata = sqliteRawJSON(metadata)
	created, err := sqliteParseTime(createdAt)
	if err != nil {
		return ThreadRecord{}, err
	}
	updated, err := sqliteParseTime(updatedAt)
	if err != nil {
		return ThreadRecord{}, err
	}
	record.CreatedAt, record.UpdatedAt = created, updated
	return record, nil
}

func scanMessagePage(rows *sql.Rows, offset, limit int) (Page[MessageRecord], error) {
	var page Page[MessageRecord]
	for rows.Next() && len(page.Records) <= limit {
		var record MessageRecord
		var messageJSON, createdAt string
		var metadata sql.NullString
		if err := rows.Scan(&record.ID, &record.ThreadID, &messageJSON, &metadata, &createdAt); err != nil {
			return Page[MessageRecord]{}, fmt.Errorf("lebro: scan message: %w", sqliteError(err))
		}
		if err := json.Unmarshal([]byte(messageJSON), &record.Message); err != nil {
			return Page[MessageRecord]{}, fmt.Errorf("lebro: decode message %q: %w", record.ID, sqliteError(err))
		}
		record.Metadata = sqliteRawJSON(metadata)
		created, err := sqliteParseTime(createdAt)
		if err != nil {
			return Page[MessageRecord]{}, err
		}
		record.CreatedAt = created
		page.Records = append(page.Records, record)
	}
	if err := rows.Err(); err != nil {
		return Page[MessageRecord]{}, fmt.Errorf("lebro: list messages: %w", sqliteError(err))
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.NextCursor = strconv.Itoa(offset + limit)
	}
	return page, nil
}

func scanWorkflowRun(row messagePageScanner) (WorkflowRunRecord, error) {
	var record WorkflowRunRecord
	var threadID sql.NullString
	var input, output, metadata sql.NullString
	var stepOutputs, failure, path, fanOut sql.NullString
	var finishedAt sql.NullString
	var startedAt, updatedAt string
	if err := row.Scan(&record.ID, &record.WorkflowID, &threadID, &record.Status, &input, &output, &metadata, &startedAt, &finishedAt, &updatedAt, &record.CurrentStep, &record.CurrentStepID, &stepOutputs, &failure, &record.WorkflowVersion, &path, &fanOut); err != nil {
		return WorkflowRunRecord{}, err
	}
	if threadID.Valid {
		record.ThreadID = ThreadID(threadID.String)
	}
	record.Input, record.Output, record.Metadata = sqliteRawJSON(input), sqliteRawJSON(output), sqliteRawJSON(metadata)
	record.StepOutputs = sqliteRawJSONArray(stepOutputs)
	record.Failure = sqliteParseFailure(failure)
	record.Path = sqliteRawStepIDArray(path)
	record.FanOut = sqliteParseFanOut(fanOut)
	started, err := sqliteParseTime(startedAt)
	if err != nil {
		return WorkflowRunRecord{}, err
	}
	record.StartedAt = started
	if finishedAt.Valid {
		finished, err := sqliteParseTime(finishedAt.String)
		if err != nil {
			return WorkflowRunRecord{}, err
		}
		record.FinishedAt = &finished
	}
	updated, err := sqliteParseTime(updatedAt)
	if err != nil {
		return WorkflowRunRecord{}, err
	}
	record.UpdatedAt = updated
	return record, nil
}

func scanWorkflowRunPage(rows *sql.Rows, offset, limit int) (Page[WorkflowRunRecord], error) {
	var page Page[WorkflowRunRecord]
	for rows.Next() && len(page.Records) <= limit {
		record, err := scanWorkflowRun(rows)
		if err != nil {
			return Page[WorkflowRunRecord]{}, fmt.Errorf("lebro: scan workflow run: %w", sqliteError(err))
		}
		page.Records = append(page.Records, record)
	}
	if err := rows.Err(); err != nil {
		return Page[WorkflowRunRecord]{}, fmt.Errorf("lebro: list workflow runs: %w", sqliteError(err))
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.NextCursor = strconv.Itoa(offset + limit)
	}
	return page, nil
}

func scanSnapshotPage(rows *sql.Rows, offset, limit int) (Page[WorkflowSnapshotRecord], error) {
	var page Page[WorkflowSnapshotRecord]
	for rows.Next() && len(page.Records) <= limit {
		var record WorkflowSnapshotRecord
		var state, createdAt string
		if err := rows.Scan(&record.ID, &record.RunID, &record.Sequence, &record.SchemaVersion, &state, &createdAt); err != nil {
			return Page[WorkflowSnapshotRecord]{}, fmt.Errorf("lebro: scan workflow snapshot: %w", sqliteError(err))
		}
		record.State = json.RawMessage(state)
		created, err := sqliteParseTime(createdAt)
		if err != nil {
			return Page[WorkflowSnapshotRecord]{}, err
		}
		record.CreatedAt = created
		page.Records = append(page.Records, record)
	}
	if err := rows.Err(); err != nil {
		return Page[WorkflowSnapshotRecord]{}, fmt.Errorf("lebro: list workflow snapshots: %w", sqliteError(err))
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.NextCursor = strconv.Itoa(offset + limit)
	}
	return page, nil
}

func scanSchedule(row messagePageScanner) (ScheduleRecord, error) {
	var record ScheduleRecord
	var concurrency string
	var input, metadata sql.NullString
	var nextFireAt, lastFireAt sql.NullString
	var paused int
	var createdAt, updatedAt string
	if err := row.Scan(&record.ID, &record.WorkflowID, &record.Spec, &paused, &concurrency, &input, &metadata, &nextFireAt, &lastFireAt, &record.WakeRunID, &record.WakeToken, &createdAt, &updatedAt); err != nil {
		return ScheduleRecord{}, err
	}
	record.Paused = paused != 0
	record.Concurrency = ConcurrencyPolicy(concurrency)
	record.Input, record.Metadata = sqliteRawJSON(input), sqliteRawJSON(metadata)
	next, err := sqliteParseNullableTime(nextFireAt)
	if err != nil {
		return ScheduleRecord{}, err
	}
	record.NextFireAt = next
	last, err := sqliteParseNullableTime(lastFireAt)
	if err != nil {
		return ScheduleRecord{}, err
	}
	record.LastFireAt = last
	created, err := sqliteParseTime(createdAt)
	if err != nil {
		return ScheduleRecord{}, err
	}
	updated, err := sqliteParseTime(updatedAt)
	if err != nil {
		return ScheduleRecord{}, err
	}
	record.CreatedAt, record.UpdatedAt = created, updated
	return record, nil
}

func scanSchedulePage(rows *sql.Rows, offset, limit int) (Page[ScheduleRecord], error) {
	var page Page[ScheduleRecord]
	for rows.Next() && len(page.Records) <= limit {
		record, err := scanSchedule(rows)
		if err != nil {
			return Page[ScheduleRecord]{}, fmt.Errorf("lebro: scan schedule: %w", sqliteError(err))
		}
		page.Records = append(page.Records, record)
	}
	if err := rows.Err(); err != nil {
		return Page[ScheduleRecord]{}, fmt.Errorf("lebro: list schedules: %w", sqliteError(err))
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.NextCursor = strconv.Itoa(offset + limit)
	}
	return page, nil
}

func scanScheduleExecutionPage(rows *sql.Rows, offset, limit int) (Page[ScheduleExecutionRecord], error) {
	var page Page[ScheduleExecutionRecord]
	for rows.Next() && len(page.Records) <= limit {
		var record ScheduleExecutionRecord
		var runID, finishedAt sql.NullString
		var status, scheduledFor, startedAt string
		if err := rows.Scan(&record.ID, &record.ScheduleID, &runID, &status, &scheduledFor, &startedAt, &finishedAt, &record.Error); err != nil {
			return Page[ScheduleExecutionRecord]{}, fmt.Errorf("lebro: scan schedule execution: %w", sqliteError(err))
		}
		if runID.Valid {
			record.RunID = RunID(runID.String)
		}
		record.Status = ScheduleExecStatus(status)
		scheduled, err := sqliteParseTime(scheduledFor)
		if err != nil {
			return Page[ScheduleExecutionRecord]{}, err
		}
		record.ScheduledFor = scheduled
		started, err := sqliteParseTime(startedAt)
		if err != nil {
			return Page[ScheduleExecutionRecord]{}, err
		}
		record.StartedAt = started
		finished, err := sqliteParseNullableTime(finishedAt)
		if err != nil {
			return Page[ScheduleExecutionRecord]{}, err
		}
		record.FinishedAt = finished
		page.Records = append(page.Records, record)
	}
	if err := rows.Err(); err != nil {
		return Page[ScheduleExecutionRecord]{}, fmt.Errorf("lebro: list schedule executions: %w", sqliteError(err))
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.NextCursor = strconv.Itoa(offset + limit)
	}
	return page, nil
}

// Times and JSON payloads are stored in UTC in a format that matches their
// serialized JSON representation, so records round-trip losslessly.
func sqliteNullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func sqliteTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func sqliteNullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return sqliteTime(*t)
}
func sqliteParseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("lebro: decode stored time %q: %w", s, err)
	}
	return t, nil
}

// sqliteParseNullableTime decodes a nullable timestamp column. A NULL yields a
// nil pointer; a present value must parse or the read fails.
func sqliteParseNullableTime(v sql.NullString) (*time.Time, error) {
	if !v.Valid {
		return nil, nil
	}
	t, err := sqliteParseTime(v.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func sqliteBool(v bool) int {
	if v {
		return 1
	}
	return 0
}

func sqliteJSON(v json.RawMessage) any {
	if len(v) == 0 {
		return nil
	}
	return string(v)
}

func sqliteRawJSON(v sql.NullString) json.RawMessage {
	if !v.Valid {
		return nil
	}
	return json.RawMessage(v.String)
}

// sqliteJSONArray marshals a slice of JSON values into a JSON array string
// suitable for the step_outputs column. A nil slice becomes NULL so readers
// can distinguish "no outputs" from "empty array".
func sqliteJSONArray(outputs []json.RawMessage) (any, error) {
	if len(outputs) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(outputs)
	if err != nil {
		return nil, fmt.Errorf("lebro: encode step outputs: %w", err)
	}
	return string(encoded), nil
}

// sqliteRawJSONArray decodes a nullable JSON-array column back into a slice.
// NULL and an empty array both yield a nil slice; invalid JSON is an error.
func sqliteRawJSONArray(v sql.NullString) []json.RawMessage {
	if !v.Valid || v.String == "" || v.String == "null" {
		return nil
	}
	var outputs []json.RawMessage
	if err := json.Unmarshal([]byte(v.String), &outputs); err != nil {
		return nil
	}
	return outputs
}

// sqliteFailureJSON marshals failure data for the failure column. nil becomes
// NULL so readers can distinguish "no failure" from "empty object".
func sqliteFailureJSON(failure *WorkflowFailureData) (any, error) {
	if failure == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(failure)
	if err != nil {
		return nil, fmt.Errorf("lebro: encode workflow failure: %w", err)
	}
	return string(encoded), nil
}

// sqliteParseFailure decodes a nullable failure column. NULL and invalid JSON
// both yield nil; a malformed payload is treated as no failure rather than
// failing the read so a corrupted row remains inspectable.
func sqliteParseFailure(v sql.NullString) *WorkflowFailureData {
	if !v.Valid || v.String == "" || v.String == "null" {
		return nil
	}
	var failure WorkflowFailureData
	if err := json.Unmarshal([]byte(v.String), &failure); err != nil {
		return nil
	}
	return &failure
}

// sqliteStepIDArray marshals a slice of StepIDs into a JSON array string for
// the path column. A nil slice becomes NULL so readers can distinguish "no
// path" from "empty path".
func sqliteStepIDArray(ids []StepID) (any, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, fmt.Errorf("lebro: encode path: %w", err)
	}
	return string(encoded), nil
}

// sqliteFanOutJSON marshals fan-out join results for the fan_out column. nil or
// empty becomes NULL so readers can distinguish "no fan-out" from "empty array".
func sqliteFanOutJSON(results []FanOutJoinResult) (any, error) {
	if len(results) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(results)
	if err != nil {
		return nil, fmt.Errorf("lebro: encode fan-out: %w", err)
	}
	return string(encoded), nil
}

// sqliteParseFanOut decodes a nullable fan_out column. NULL and invalid JSON
// both yield nil; a malformed payload is treated as no fan-out rather than
// failing the read so a corrupted row remains inspectable.
func sqliteParseFanOut(v sql.NullString) []FanOutJoinResult {
	if !v.Valid || v.String == "" || v.String == "null" {
		return nil
	}
	var results []FanOutJoinResult
	if err := json.Unmarshal([]byte(v.String), &results); err != nil {
		return nil
	}
	return results
}

// sqliteRawStepIDArray decodes a nullable JSON-array column of StepIDs back
// into a slice. NULL, empty, and invalid JSON all yield nil so a corrupted
// row remains inspectable instead of failing the read.
func sqliteRawStepIDArray(v sql.NullString) []StepID {
	if !v.Valid || v.String == "" || v.String == "null" {
		return nil
	}
	var ids []StepID
	if err := json.Unmarshal([]byte(v.String), &ids); err != nil {
		return nil
	}
	return ids
}
