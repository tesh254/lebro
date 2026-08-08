package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// PostgresStore is a Store backed by PostgreSQL. It preserves the same
// validation, pagination, and transaction semantics as MemoryStore and
// SQLiteStore, and keeps records durable across process restarts and safe
// under concurrent access from multiple processes.
//
// The store expects its schema to be installed with Migrate before use.
// Transactions use READ COMMITTED isolation with a retryable write pattern:
// serialization failures and lock timeouts surface as ErrConflict so callers
// may retry, matching the optimistic-conflict contract of the other adapters.
type PostgresStore struct {
	db *sql.DB
}

// PostgresStoreOptions tunes connection-pool behavior. A zero value leaves
// the database/sql defaults in place, which are suitable for short-lived
// processes. Long-running services should set MaxOpenConns, MaxIdleConns,
// and MaxConnIdleTime to match their workload.
type PostgresStoreOptions struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxIdleTime time.Duration
}

// NewPostgresStore opens a PostgreSQL connection pool at dsn and returns a
// store whose repositories share the pool. The DSN must be a libpq-style
// connection string or URL (e.g. "postgres://user:pass@host:5432/db?sslmode=disable").
// The database is left uninitialized; call Migrate to install the schema.
//
// The pool is opened through the pgx stdlib adapter so the same database/sql
// machinery powers both standalone and transaction-scoped repositories.
func NewPostgresStore(dsn string, opts PostgresStoreOptions) (*PostgresStore, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("lebro: postgres: parse DSN %q: %w", dsn, err)
	}
	db := stdlib.OpenDB(*cfg)
	if opts.MaxOpenConns > 0 {
		db.SetMaxOpenConns(opts.MaxOpenConns)
	}
	if opts.MaxIdleConns > 0 {
		db.SetMaxIdleConns(opts.MaxIdleConns)
	}
	if opts.ConnMaxIdleTime > 0 {
		db.SetConnMaxIdleTime(opts.ConnMaxIdleTime)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("lebro: postgres: connect to %q: %w", dsn, err)
	}
	return &PostgresStore{db: db}, nil
}

// Close releases the underlying connection pool.
func (s *PostgresStore) Close() error { return s.db.Close() }

// postgresSchemaMigrations installs the schema one statement at a time. The
// version is tracked in a schema_migrations table and each migration runs
// inside the Migrate transaction, so a failed migration leaves the database
// untouched. Migrations must be append-only; never reorder or edit an
// applied step.
var postgresSchemaMigrations = []string{
	`CREATE TABLE threads (
		id         TEXT PRIMARY KEY,
		metadata   TEXT,
		created_at TIMESTAMPTZ NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL
	)`,
	`CREATE TABLE workflow_runs (
		id          TEXT PRIMARY KEY,
		workflow_id TEXT NOT NULL,
		thread_id   TEXT,
		status      TEXT NOT NULL,
		input       TEXT,
		output      TEXT,
		metadata    TEXT,
		started_at  TIMESTAMPTZ NOT NULL,
		finished_at TIMESTAMPTZ,
		updated_at  TIMESTAMPTZ NOT NULL
	)`,
	`CREATE TABLE messages (
		id         TEXT NOT NULL,
		thread_id  TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
		seq        BIGSERIAL,
		message    TEXT NOT NULL,
		metadata   TEXT,
		created_at TIMESTAMPTZ NOT NULL,
		UNIQUE (thread_id, id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_thread_seq ON messages(thread_id, seq)`,
	`CREATE TABLE workflow_snapshots (
		id         TEXT NOT NULL,
		run_id     TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
		sequence   BIGINT NOT NULL,
		state      TEXT,
		created_at TIMESTAMPTZ NOT NULL,
		UNIQUE (run_id, id),
		UNIQUE (run_id, sequence)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_workflow_snapshots_run_seq ON workflow_snapshots(run_id, sequence)`,
	`ALTER TABLE threads ADD COLUMN IF NOT EXISTS namespace TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE threads ADD COLUMN IF NOT EXISTS owner_id TEXT NOT NULL DEFAULT ''`,
	`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
}

const postgresSchemaVersionQuery = `SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`

// Migrate applies any pending schema migrations atomically. It is
// idempotent; a database already at the current version is a no-op. A
// failure rolls the transaction back, leaving the database unchanged and
// the error actionable (it names the failing migration).
//
// Concurrent Migrate calls from multiple processes are safe: a
// transaction-scoped advisory lock (pg_advisory_xact_lock) serializes
// migration so only one process runs DDL at a time. The lock is released
// automatically when the transaction commits or rolls back, so there is no
// separate unlock step and no way to leak a held lock back onto a pooled
// connection. A process that acquires the lock after another has already
// migrated sees the updated version and skips its own migrations.
func (s *PostgresStore) Migrate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Ensure the schema_migrations table exists so the first run can record
	// its version. This is separate from the versioned migrations so a
	// brand-new database does not need a bootstrapping special case.
	if _, err := s.db.ExecContext(ctx, postgresSchemaMigrations[len(postgresSchemaMigrations)-1]); err != nil {
		return fmt.Errorf("lebro: postgres: ensure schema_migrations table: %w", postgresError(err))
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("lebro: postgres: begin migration: %w", postgresError(err))
	}
	defer func() { _ = tx.Rollback() }()

	// Acquire a transaction-scoped advisory lock so concurrent migrations
	// serialize. The lock is released automatically on commit or rollback,
	// so it cannot leak back onto a pooled connection.
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", postgresAdvisoryLockKey); err != nil {
		return fmt.Errorf("lebro: postgres: acquire migration lock: %w", postgresError(err))
	}

	var version int
	switch err := tx.QueryRowContext(ctx, postgresSchemaVersionQuery).Scan(&version); {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		version = 0
	default:
		return fmt.Errorf("lebro: postgres: read schema version: %w", postgresError(err))
	}
	if version > len(postgresSchemaMigrations)-1 {
		return fmt.Errorf("lebro: postgres: database schema version %d is newer than this build supports (max %d)", version, len(postgresSchemaMigrations)-1)
	}
	for i := version; i < len(postgresSchemaMigrations)-1; i++ {
		if _, err := tx.ExecContext(ctx, postgresSchemaMigrations[i]); err != nil {
			return fmt.Errorf("lebro: postgres: migration %d (schema version %d) failed: %w; database left unchanged", i+1, i+1, postgresError(err))
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, i+1); err != nil {
			return fmt.Errorf("lebro: postgres: record schema version %d: %w", i+1, postgresError(err))
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lebro: postgres: commit migration: %w", postgresError(err))
	}
	return nil
}

// postgresAdvisoryLockKey is a fixed 64-bit key for the transaction-scoped
// advisory lock that serializes concurrent migrations. The value is
// arbitrary but must be stable across processes and builds.
const postgresAdvisoryLockKey int64 = 0x6c6562726f000001

// Transaction runs fn against repositories bound to one PostgreSQL
// transaction at the READ COMMITTED isolation level. A non-nil fn error or a
// canceled context rolls the transaction back. Serialization failures
// (SQLSTATE 40001) and lock timeouts (55P03) are mapped to ErrConflict so
// callers may retry, matching the optimistic-conflict contract of the other
// adapters.
func (s *PostgresStore) Transaction(ctx context.Context, fn func(context.Context, Repositories) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("lebro: postgres: begin transaction: %w", postgresError(err))
	}
	repositories := &postgresRepositories{q: tx}
	finished := false
	defer func() {
		if !finished {
			_ = tx.Rollback()
		}
	}()
	if err := fn(ctx, repositories); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lebro: postgres: commit transaction: %w", postgresError(err))
	}
	finished = true
	return nil
}

func (s *PostgresStore) Threads() ThreadRepository           { return &postgresRepositories{q: s.db} }
func (s *PostgresStore) Messages() MessageRepository         { return &postgresRepositories{q: s.db} }
func (s *PostgresStore) WorkflowRuns() WorkflowRunRepository { return &postgresRepositories{q: s.db} }
func (s *PostgresStore) WorkflowSnapshots() WorkflowSnapshotRepository {
	return &postgresRepositories{q: s.db}
}

type postgresRepositories struct {
	q sqlQueryer
}

func (r *postgresRepositories) Threads() ThreadRepository                     { return r }
func (r *postgresRepositories) Messages() MessageRepository                   { return r }
func (r *postgresRepositories) WorkflowRuns() WorkflowRunRepository           { return r }
func (r *postgresRepositories) WorkflowSnapshots() WorkflowSnapshotRepository { return r }

func (r *postgresRepositories) CreateThread(ctx context.Context, v ThreadRecord) error {
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
	if _, err := r.q.ExecContext(ctx, `INSERT INTO threads (id, namespace, owner_id, metadata, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		v.ID, v.Namespace, v.OwnerID, postgresJSON(v.Metadata), v.CreatedAt.UTC(), v.UpdatedAt.UTC()); err != nil {
		return fmt.Errorf("lebro: create thread %q: %w", v.ID, postgresError(err))
	}
	return nil
}

func (r *postgresRepositories) GetThread(ctx context.Context, id ThreadID) (ThreadRecord, error) {
	if err := ctx.Err(); err != nil {
		return ThreadRecord{}, err
	}
	row := r.q.QueryRowContext(ctx, `SELECT id, namespace, owner_id, metadata, created_at, updated_at FROM threads WHERE id = $1`, id)
	record, err := scanThreadPG(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadRecord{}, ErrNotFound
	}
	if err != nil {
		return ThreadRecord{}, fmt.Errorf("lebro: get thread %q: %w", id, postgresError(err))
	}
	return record, nil
}

func (r *postgresRepositories) UpdateThread(ctx context.Context, v ThreadRecord) error {
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
	result, err := r.q.ExecContext(ctx, `UPDATE threads SET namespace = $1, owner_id = $2, metadata = $3, updated_at = $4 WHERE id = $5`,
		v.Namespace, v.OwnerID, postgresJSON(v.Metadata), v.UpdatedAt.UTC(), v.ID)
	if err != nil {
		return fmt.Errorf("lebro: update thread %q: %w", v.ID, postgresError(err))
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("lebro: update thread %q: %w", v.ID, postgresError(err))
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *postgresRepositories) AppendMessages(ctx context.Context, vs []MessageRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(vs) == 0 {
		return nil
	}
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
	const chunkSize = 1000
	insert := func(q sqlQueryer) error {
		for start := 0; start < len(vs); start += chunkSize {
			end := start + chunkSize
			if end > len(vs) {
				end = len(vs)
			}
			chunk := vs[start:end]
			var b stringsBuilder
			args := make([]any, 0, len(chunk)*5)
			for i, v := range chunk {
				message, err := json.Marshal(v.Message)
				if err != nil {
					return fmt.Errorf("lebro: encode message %q: %w", v.ID, err)
				}
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(fmt.Sprintf("($%d, $%d, $%d, $%d, $%d)", i*5+1, i*5+2, i*5+3, i*5+4, i*5+5))
				args = append(args, v.ID, v.ThreadID, string(message), postgresJSON(v.Metadata), v.CreatedAt.UTC())
			}
			if _, err := q.ExecContext(ctx, `INSERT INTO messages (id, thread_id, message, metadata, created_at) VALUES `+b.String(), args...); err != nil {
				return fmt.Errorf("lebro: append messages: %w", postgresError(err))
			}
		}
		return nil
	}
	return r.withAutoTx(ctx, insert)
}

// withAutoTx wraps multi-statement operations in a transaction when the
// repositories are standalone (backed by *sql.DB). When they are already
// inside a caller's Transaction (backed by *sql.Tx) fn runs directly.
func (r *postgresRepositories) withAutoTx(ctx context.Context, fn func(sqlQueryer) error) error {
	db, ok := r.q.(*sql.DB)
	if !ok {
		return fn(r.q)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("lebro: postgres: begin append: %w", postgresError(err))
	}
	finished := false
	defer func() {
		if !finished {
			_ = tx.Rollback()
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lebro: postgres: commit append: %w", postgresError(err))
	}
	finished = true
	return nil
}

func (r *postgresRepositories) messageIDs(ctx context.Context, id ThreadID) (map[string]struct{}, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT id FROM messages WHERE thread_id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("lebro: load message IDs for thread %q: %w", id, postgresError(err))
	}
	defer func() { _ = rows.Close() }()
	ids := make(map[string]struct{})
	for rows.Next() {
		var messageID string
		if err := rows.Scan(&messageID); err != nil {
			return nil, fmt.Errorf("lebro: scan message ID: %w", postgresError(err))
		}
		ids[messageID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lebro: load message IDs for thread %q: %w", id, postgresError(err))
	}
	return ids, nil
}

func (r *postgresRepositories) ListMessages(ctx context.Context, id ThreadID, p PageRequest) (Page[MessageRecord], error) {
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
	rows, err := r.q.QueryContext(ctx, `SELECT id, thread_id, message, metadata, created_at FROM messages WHERE thread_id = $1 ORDER BY seq LIMIT $2 OFFSET $3`, id, postgresFetchLimit(limit), offset)
	if err != nil {
		return Page[MessageRecord]{}, fmt.Errorf("lebro: list messages for thread %q: %w", id, postgresError(err))
	}
	defer func() { _ = rows.Close() }()
	return scanMessagePagePG(rows, offset, limit)
}

func (r *postgresRepositories) SaveWorkflowRun(ctx context.Context, v WorkflowRunRecord) error {
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
	var threadID any
	if v.ThreadID != "" {
		threadID = string(v.ThreadID)
	}
	var finishedAt any
	if v.FinishedAt != nil {
		finishedAt = v.FinishedAt.UTC()
	}
	if _, err := r.q.ExecContext(ctx, `INSERT INTO workflow_runs (id, workflow_id, thread_id, status, input, output, metadata, started_at, finished_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (id) DO UPDATE SET
			workflow_id = EXCLUDED.workflow_id,
			thread_id   = EXCLUDED.thread_id,
			status      = EXCLUDED.status,
			input       = EXCLUDED.input,
			output      = EXCLUDED.output,
			metadata    = EXCLUDED.metadata,
			started_at  = EXCLUDED.started_at,
			finished_at = EXCLUDED.finished_at,
			updated_at  = EXCLUDED.updated_at`,
		v.ID, v.WorkflowID, threadID, v.Status, postgresJSON(v.Input), postgresJSON(v.Output), postgresJSON(v.Metadata), v.StartedAt.UTC(), finishedAt, v.UpdatedAt.UTC()); err != nil {
		return fmt.Errorf("lebro: save workflow run %q: %w", v.ID, postgresError(err))
	}
	return nil
}

func (r *postgresRepositories) GetWorkflowRun(ctx context.Context, id RunID) (WorkflowRunRecord, error) {
	if err := ctx.Err(); err != nil {
		return WorkflowRunRecord{}, err
	}
	row := r.q.QueryRowContext(ctx, `SELECT id, workflow_id, thread_id, status, input, output, metadata, started_at, finished_at, updated_at FROM workflow_runs WHERE id = $1`, id)
	record, err := scanWorkflowRunPG(row)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkflowRunRecord{}, ErrNotFound
	}
	if err != nil {
		return WorkflowRunRecord{}, fmt.Errorf("lebro: get workflow run %q: %w", id, postgresError(err))
	}
	return record, nil
}

func (r *postgresRepositories) SaveWorkflowSnapshot(ctx context.Context, v WorkflowSnapshotRecord) error {
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
	var found string
	switch err := r.q.QueryRowContext(ctx, `SELECT id FROM workflow_snapshots WHERE run_id = $1 AND id = $2`, v.RunID, v.ID).Scan(&found); {
	case err == nil:
		return errors.New("lebro: workflow snapshot already exists")
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("lebro: check workflow snapshot %q: %w", v.ID, postgresError(err))
	}
	var foundSequence int64
	switch err := r.q.QueryRowContext(ctx, `SELECT sequence FROM workflow_snapshots WHERE run_id = $1 AND sequence = $2`, v.RunID, v.Sequence).Scan(&foundSequence); {
	case err == nil:
		return errors.New("lebro: workflow snapshot already exists")
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("lebro: check workflow snapshot sequence %d: %w", v.Sequence, postgresError(err))
	}
	if _, err := r.q.ExecContext(ctx, `INSERT INTO workflow_snapshots (id, run_id, sequence, state, created_at) VALUES ($1, $2, $3, $4, $5)`,
		v.ID, v.RunID, v.Sequence, postgresJSON(v.State), v.CreatedAt.UTC()); err != nil {
		return fmt.Errorf("lebro: save workflow snapshot %q: %w", v.ID, postgresError(err))
	}
	return nil
}

func (r *postgresRepositories) ListWorkflowSnapshots(ctx context.Context, id RunID, p PageRequest) (Page[WorkflowSnapshotRecord], error) {
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
	rows, err := r.q.QueryContext(ctx, `SELECT id, run_id, sequence, state, created_at FROM workflow_snapshots WHERE run_id = $1 ORDER BY sequence LIMIT $2 OFFSET $3`, id, postgresFetchLimit(limit), offset)
	if err != nil {
		return Page[WorkflowSnapshotRecord]{}, fmt.Errorf("lebro: list workflow snapshots for run %q: %w", id, postgresError(err))
	}
	defer func() { _ = rows.Close() }()
	return scanSnapshotPagePG(rows, offset, limit)
}

func (r *postgresRepositories) threadExists(ctx context.Context, id ThreadID) error {
	var found int
	err := r.q.QueryRowContext(ctx, `SELECT 1 FROM threads WHERE id = $1`, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lebro: postgres: %w", postgresError(err))
	}
	return nil
}

func (r *postgresRepositories) runExists(ctx context.Context, id RunID) error {
	var found int
	err := r.q.QueryRowContext(ctx, `SELECT 1 FROM workflow_runs WHERE id = $1`, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lebro: postgres: %w", postgresError(err))
	}
	return nil
}

// postgresError normalizes driver errors into the storage error vocabulary.
// Foreign-key violations (23503) mean a parent record is missing and map to
// ErrNotFound. Serialization failures (40001) and lock timeouts (55P03) map
// to ErrConflict so callers may retry.
func postgresError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "23503":
		return ErrNotFound
	case "40001", "55P03":
		return ErrConflict
	default:
		return fmt.Errorf("%w (postgres %s)", err, pgErr.Code)
	}
}

func scanThreadPG(row messagePageScanner) (ThreadRecord, error) {
	var record ThreadRecord
	var metadata sql.NullString
	if err := row.Scan(&record.ID, &record.Namespace, &record.OwnerID, &metadata, &record.CreatedAt, &record.UpdatedAt); err != nil {
		return ThreadRecord{}, err
	}
	record.Metadata = postgresRawJSON([]byte(metadata.String))
	record.CreatedAt = record.CreatedAt.UTC()
	record.UpdatedAt = record.UpdatedAt.UTC()
	return record, nil
}

func scanMessagePagePG(rows *sql.Rows, offset, limit int) (Page[MessageRecord], error) {
	var page Page[MessageRecord]
	for rows.Next() && len(page.Records) <= limit {
		var record MessageRecord
		var messageJSON string
		var metadata sql.NullString
		if err := rows.Scan(&record.ID, &record.ThreadID, &messageJSON, &metadata, &record.CreatedAt); err != nil {
			return Page[MessageRecord]{}, fmt.Errorf("lebro: scan message: %w", postgresError(err))
		}
		if err := json.Unmarshal([]byte(messageJSON), &record.Message); err != nil {
			return Page[MessageRecord]{}, fmt.Errorf("lebro: decode message %q: %w", record.ID, postgresError(err))
		}
		record.Metadata = postgresRawJSON([]byte(metadata.String))
		record.CreatedAt = record.CreatedAt.UTC()
		page.Records = append(page.Records, record)
	}
	if err := rows.Err(); err != nil {
		return Page[MessageRecord]{}, fmt.Errorf("lebro: list messages: %w", postgresError(err))
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.NextCursor = strconv.Itoa(offset + limit)
	}
	return page, nil
}

func scanWorkflowRunPG(row messagePageScanner) (WorkflowRunRecord, error) {
	var record WorkflowRunRecord
	var threadID sql.NullString
	var input, output, metadata sql.NullString
	var finishedAt sql.NullTime
	if err := row.Scan(&record.ID, &record.WorkflowID, &threadID, &record.Status, &input, &output, &metadata, &record.StartedAt, &finishedAt, &record.UpdatedAt); err != nil {
		return WorkflowRunRecord{}, err
	}
	if threadID.Valid {
		record.ThreadID = ThreadID(threadID.String)
	}
	record.Input = postgresRawJSON([]byte(input.String))
	record.Output = postgresRawJSON([]byte(output.String))
	record.Metadata = postgresRawJSON([]byte(metadata.String))
	if finishedAt.Valid {
		finished := finishedAt.Time.UTC()
		record.FinishedAt = &finished
	}
	record.StartedAt = record.StartedAt.UTC()
	record.UpdatedAt = record.UpdatedAt.UTC()
	return record, nil
}

func scanSnapshotPagePG(rows *sql.Rows, offset, limit int) (Page[WorkflowSnapshotRecord], error) {
	var page Page[WorkflowSnapshotRecord]
	for rows.Next() && len(page.Records) <= limit {
		var record WorkflowSnapshotRecord
		var state sql.NullString
		if err := rows.Scan(&record.ID, &record.RunID, &record.Sequence, &state, &record.CreatedAt); err != nil {
			return Page[WorkflowSnapshotRecord]{}, fmt.Errorf("lebro: scan workflow snapshot: %w", postgresError(err))
		}
		record.State = postgresRawJSON([]byte(state.String))
		record.CreatedAt = record.CreatedAt.UTC()
		page.Records = append(page.Records, record)
	}
	if err := rows.Err(); err != nil {
		return Page[WorkflowSnapshotRecord]{}, fmt.Errorf("lebro: list workflow snapshots: %w", postgresError(err))
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.NextCursor = strconv.Itoa(offset + limit)
	}
	return page, nil
}

func postgresJSON(v json.RawMessage) any {
	if len(v) == 0 {
		return nil
	}
	return string(v)
}

// postgresFetchLimit returns limit+1 without overflowing on very large
// limits. PostgreSQL rejects a negative LIMIT (SQLSTATE 2201W), so when
// limit+1 wraps to a negative value we cap it at limit itself.
func postgresFetchLimit(limit int) int {
	if limit >= int(^uint(0)>>1) {
		return limit
	}
	return limit + 1
}

func postgresRawJSON(v []byte) json.RawMessage {
	if len(v) == 0 {
		return nil
	}
	return json.RawMessage(v)
}

// stringsBuilder is a thin wrapper around a string buffer for building
// multi-row VALUES clauses. It avoids importing strings in this file since
// the SQLite adapter already owns that import for DSN rewriting.
type stringsBuilder struct{ buf []byte }

func (b *stringsBuilder) WriteString(s string) { b.buf = append(b.buf, s...) }
func (b *stringsBuilder) String() string       { return string(b.buf) }
