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

func (s *SQLiteStore) Threads() ThreadRepository           { return &sqliteRepositories{q: s.db} }
func (s *SQLiteStore) Messages() MessageRepository         { return &sqliteRepositories{q: s.db} }
func (s *SQLiteStore) WorkflowRuns() WorkflowRunRepository { return &sqliteRepositories{q: s.db} }
func (s *SQLiteStore) WorkflowSnapshots() WorkflowSnapshotRepository {
	return &sqliteRepositories{q: s.db}
}

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
	if _, err := r.q.ExecContext(ctx, `INSERT INTO threads (id, metadata, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		v.ID, sqliteJSON(v.Metadata), sqliteTime(v.CreatedAt), sqliteTime(v.UpdatedAt)); err != nil {
		return fmt.Errorf("lebro: create thread %q: %w", v.ID, sqliteError(err))
	}
	return nil
}

func (r *sqliteRepositories) GetThread(ctx context.Context, id ThreadID) (ThreadRecord, error) {
	if err := ctx.Err(); err != nil {
		return ThreadRecord{}, err
	}
	row := r.q.QueryRowContext(ctx, `SELECT id, metadata, created_at, updated_at FROM threads WHERE id = ?`, id)
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
	if _, err := r.q.ExecContext(ctx, `UPDATE threads SET metadata = ?, updated_at = ? WHERE id = ?`,
		sqliteJSON(v.Metadata), sqliteTime(v.UpdatedAt), v.ID); err != nil {
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
	if err := validateRecord(v); err != nil {
		return fmt.Errorf("lebro: workflow run: %w", err)
	}
	if _, err := r.q.ExecContext(ctx, `INSERT INTO workflow_runs (id, workflow_id, thread_id, status, input, output, metadata, started_at, finished_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			workflow_id = excluded.workflow_id,
			thread_id   = excluded.thread_id,
			status      = excluded.status,
			input       = excluded.input,
			output      = excluded.output,
			metadata    = excluded.metadata,
			started_at  = excluded.started_at,
			finished_at = excluded.finished_at,
			updated_at  = excluded.updated_at`,
		v.ID, v.WorkflowID, sqliteNullableString(string(v.ThreadID)), v.Status, sqliteJSON(v.Input), sqliteJSON(v.Output), sqliteJSON(v.Metadata), sqliteTime(v.StartedAt), sqliteNullableTime(v.FinishedAt), sqliteTime(v.UpdatedAt)); err != nil {
		return fmt.Errorf("lebro: save workflow run %q: %w", v.ID, sqliteError(err))
	}
	return nil
}

func (r *sqliteRepositories) GetWorkflowRun(ctx context.Context, id RunID) (WorkflowRunRecord, error) {
	if err := ctx.Err(); err != nil {
		return WorkflowRunRecord{}, err
	}
	row := r.q.QueryRowContext(ctx, `SELECT id, workflow_id, thread_id, status, input, output, metadata, started_at, finished_at, updated_at FROM workflow_runs WHERE id = ?`, id)
	record, err := scanWorkflowRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkflowRunRecord{}, ErrNotFound
	}
	if err != nil {
		return WorkflowRunRecord{}, fmt.Errorf("lebro: get workflow run %q: %w", id, sqliteError(err))
	}
	return record, nil
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
	if _, err := r.q.ExecContext(ctx, `INSERT INTO workflow_snapshots (id, run_id, sequence, state, created_at) VALUES (?, ?, ?, ?, ?)`,
		v.ID, v.RunID, v.Sequence, string(v.State), sqliteTime(v.CreatedAt)); err != nil {
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
	rows, err := r.q.QueryContext(ctx, `SELECT id, run_id, sequence, state, created_at FROM workflow_snapshots WHERE run_id = ? ORDER BY sequence LIMIT ? OFFSET ?`, id, limit+1, offset)
	if err != nil {
		return Page[WorkflowSnapshotRecord]{}, fmt.Errorf("lebro: list workflow snapshots for run %q: %w", id, sqliteError(err))
	}
	defer func() { _ = rows.Close() }()
	return scanSnapshotPage(rows, offset, limit)
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
	if err := row.Scan(&record.ID, &metadata, &createdAt, &updatedAt); err != nil {
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
	var finishedAt sql.NullString
	var startedAt, updatedAt string
	if err := row.Scan(&record.ID, &record.WorkflowID, &threadID, &record.Status, &input, &output, &metadata, &startedAt, &finishedAt, &updatedAt); err != nil {
		return WorkflowRunRecord{}, err
	}
	if threadID.Valid {
		record.ThreadID = ThreadID(threadID.String)
	}
	record.Input, record.Output, record.Metadata = sqliteRawJSON(input), sqliteRawJSON(output), sqliteRawJSON(metadata)
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

func scanSnapshotPage(rows *sql.Rows, offset, limit int) (Page[WorkflowSnapshotRecord], error) {
	var page Page[WorkflowSnapshotRecord]
	for rows.Next() && len(page.Records) <= limit {
		var record WorkflowSnapshotRecord
		var state, createdAt string
		if err := rows.Scan(&record.ID, &record.RunID, &record.Sequence, &state, &createdAt); err != nil {
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
