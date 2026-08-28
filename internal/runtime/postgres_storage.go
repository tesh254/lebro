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
	`ALTER TABLE workflow_runs ADD COLUMN IF NOT EXISTS current_step INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE workflow_runs ADD COLUMN IF NOT EXISTS current_step_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE workflow_runs ADD COLUMN IF NOT EXISTS step_outputs TEXT`,
	`ALTER TABLE workflow_runs ADD COLUMN IF NOT EXISTS failure TEXT`,
	`ALTER TABLE workflow_runs ADD COLUMN IF NOT EXISTS workflow_version TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE workflow_snapshots ADD COLUMN IF NOT EXISTS schema_version INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE workflow_runs ADD COLUMN IF NOT EXISTS path TEXT`,
	`ALTER TABLE workflow_runs ADD COLUMN IF NOT EXISTS fan_out TEXT`,
	`CREATE TABLE schedules (
		id           TEXT PRIMARY KEY,
		workflow_id  TEXT NOT NULL,
		spec         TEXT NOT NULL,
		paused       BOOLEAN NOT NULL DEFAULT FALSE,
		concurrency  TEXT NOT NULL DEFAULT '',
		input        TEXT,
		metadata     TEXT,
		next_fire_at TIMESTAMPTZ,
		last_fire_at TIMESTAMPTZ,
		created_at   TIMESTAMPTZ NOT NULL,
		updated_at   TIMESTAMPTZ NOT NULL
	)`,
	`CREATE TABLE schedule_executions (
		id            TEXT NOT NULL,
		schedule_id   TEXT NOT NULL REFERENCES schedules(id) ON DELETE CASCADE,
		seq           BIGSERIAL,
		run_id        TEXT,
		status        TEXT NOT NULL,
		scheduled_for TIMESTAMPTZ NOT NULL,
		started_at    TIMESTAMPTZ NOT NULL,
		finished_at   TIMESTAMPTZ,
		error         TEXT NOT NULL DEFAULT '',
		UNIQUE (schedule_id, id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_schedule_executions_schedule_seq ON schedule_executions(schedule_id, seq)`,
	`CREATE INDEX IF NOT EXISTS idx_schedules_due ON schedules(next_fire_at) WHERE paused = FALSE AND next_fire_at IS NOT NULL`,
	`CREATE TABLE working_memory_facts (
		id TEXT NOT NULL UNIQUE, namespace TEXT NOT NULL, owner_id TEXT NOT NULL, key TEXT NOT NULL,
		value TEXT NOT NULL, version BIGINT NOT NULL, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (namespace, owner_id, key)
	)`,
	`ALTER TABLE schedules ADD COLUMN IF NOT EXISTS wake_run_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE schedules ADD COLUMN IF NOT EXISTS wake_token TEXT NOT NULL DEFAULT ''`,
	`CREATE TABLE IF NOT EXISTS run_events (
		id               TEXT NOT NULL,
		run_id           TEXT NOT NULL,
		thread_id        TEXT NOT NULL DEFAULT '',
		seq              BIGINT NOT NULL,
		type             TEXT NOT NULL,
		timestamp        TIMESTAMPTZ NOT NULL,
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
		input_tokens     BIGINT NOT NULL DEFAULT 0,
		output_tokens    BIGINT NOT NULL DEFAULT 0,
		reasoning_tokens BIGINT NOT NULL DEFAULT 0,
		total_tokens     BIGINT NOT NULL DEFAULT 0,
		duration_ns      BIGINT NOT NULL DEFAULT 0,
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
	`CREATE TABLE IF NOT EXISTS model_attempts (
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
		input_tokens        BIGINT NOT NULL DEFAULT 0,
		output_tokens       BIGINT NOT NULL DEFAULT 0,
		reasoning_tokens    BIGINT NOT NULL DEFAULT 0,
		total_tokens        BIGINT NOT NULL DEFAULT 0,
		started_at          TIMESTAMPTZ NOT NULL,
		finished_at         TIMESTAMPTZ NOT NULL,
		message_ids         TEXT,
		error_kind          TEXT NOT NULL DEFAULT '',
		error_message       TEXT NOT NULL DEFAULT '',
		provider_request_id TEXT NOT NULL DEFAULT '',
		cost_micros         BIGINT NOT NULL DEFAULT 0,
		currency            TEXT NOT NULL DEFAULT '',
		annotations         TEXT,
		seq                 BIGSERIAL,
		UNIQUE (run_id, id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_model_attempts_run ON model_attempts(run_id, seq)`,
	`CREATE INDEX IF NOT EXISTS idx_model_attempts_thread ON model_attempts(thread_id, started_at)`,
	`CREATE INDEX IF NOT EXISTS idx_model_attempts_provider ON model_attempts(provider)`,
	`CREATE INDEX IF NOT EXISTS idx_model_attempts_status ON model_attempts(status)`,
	`CREATE TABLE IF NOT EXISTS tool_executions (
		id            TEXT NOT NULL,
		run_id        TEXT NOT NULL,
		thread_id     TEXT NOT NULL DEFAULT '',
		step          INTEGER NOT NULL DEFAULT 0,
		step_id       TEXT NOT NULL DEFAULT '',
		tool_call_id  TEXT NOT NULL,
		tool_id       TEXT NOT NULL,
		state         TEXT NOT NULL,
		started_at    TIMESTAMPTZ NOT NULL,
		finished_at   TIMESTAMPTZ,
		error_kind    TEXT NOT NULL DEFAULT '',
		error_message TEXT NOT NULL DEFAULT '',
		annotations   TEXT,
		seq           BIGSERIAL,
		UNIQUE (run_id, id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_tool_executions_run ON tool_executions(run_id, seq)`,
	`CREATE INDEX IF NOT EXISTS idx_tool_executions_thread ON tool_executions(thread_id, started_at)`,
	`CREATE INDEX IF NOT EXISTS idx_tool_executions_tool ON tool_executions(tool_id)`,
	`ALTER TABLE run_events ADD COLUMN IF NOT EXISTS namespace TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE run_events ADD COLUMN IF NOT EXISTS owner_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE model_attempts ADD COLUMN IF NOT EXISTS namespace TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE model_attempts ADD COLUMN IF NOT EXISTS owner_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE tool_executions ADD COLUMN IF NOT EXISTS namespace TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE tool_executions ADD COLUMN IF NOT EXISTS owner_id TEXT NOT NULL DEFAULT ''`,
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

var (
	_ RuntimeStore       = (*PostgresStore)(nil)
	_ TranscriptStore    = (*PostgresStore)(nil)
	_ WorkingMemoryStore = (*PostgresStore)(nil)
	_ WorkflowStateStore = (*PostgresStore)(nil)
	_ ScheduleStore      = (*PostgresStore)(nil)
	_ ObservabilityStore = (*PostgresStore)(nil)
	_ TransactionalStore = (*PostgresStore)(nil)
)

// Capabilities reports full support: PostgresStore implements every repository
// capability and transactional writes.
func (s *PostgresStore) Capabilities() StoreCapabilities { return AllStoreCapabilities() }

// InTransaction implements TransactionalStore by running fn against the
// store's transaction boundary.
func (s *PostgresStore) InTransaction(ctx context.Context, fn func(context.Context, RuntimeStore) error) error {
	return s.Transaction(ctx, func(ctx context.Context, repos Repositories) error {
		return fn(ctx, newRuntimeStoreView(s.Capabilities(), repos))
	})
}

func (s *PostgresStore) Threads() ThreadRepository           { return &postgresRepositories{q: s.db} }
func (s *PostgresStore) Messages() MessageRepository         { return &postgresRepositories{q: s.db} }
func (s *PostgresStore) WorkflowRuns() WorkflowRunRepository { return &postgresRepositories{q: s.db} }
func (s *PostgresStore) WorkflowSnapshots() WorkflowSnapshotRepository {
	return &postgresRepositories{q: s.db}
}
func (s *PostgresStore) Schedules() ScheduleRepository { return &postgresRepositories{q: s.db} }
func (s *PostgresStore) ScheduleExecutions() ScheduleExecutionRepository {
	return &postgresRepositories{q: s.db}
}
func (s *PostgresStore) WorkingMemory() WorkingMemoryRepository {
	return &postgresRepositories{q: s.db}
}

type postgresRepositories struct {
	q sqlQueryer
}

func (r *postgresRepositories) Threads() ThreadRepository                     { return r }
func (r *postgresRepositories) Messages() MessageRepository                   { return r }
func (r *postgresRepositories) WorkflowRuns() WorkflowRunRepository           { return r }
func (r *postgresRepositories) WorkflowSnapshots() WorkflowSnapshotRepository { return r }
func (r *postgresRepositories) Schedules() ScheduleRepository                 { return r }
func (r *postgresRepositories) ScheduleExecutions() ScheduleExecutionRepository {
	return r
}
func (r *postgresRepositories) WorkingMemory() WorkingMemoryRepository { return r }

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

func (r *postgresRepositories) UpdateMessages(ctx context.Context, vs []MessageRecord) error {
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
			result, err := q.ExecContext(ctx, `UPDATE messages SET message = $1, metadata = $2 WHERE id = $3 AND thread_id = $4`, string(message), postgresJSON(v.Metadata), v.ID, v.ThreadID)
			if err != nil {
				return fmt.Errorf("lebro: update message %q: %w", v.ID, postgresError(err))
			}
			n, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("lebro: update message %q: %w", v.ID, postgresError(err))
			}
			if n == 0 {
				return ErrNotFound
			}
		}
		return nil
	})
}

func (r *postgresRepositories) DeleteMessages(ctx context.Context, id ThreadID, ids []string) error {
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
	if _, err := r.q.ExecContext(ctx, `DELETE FROM messages WHERE thread_id = $1 AND id = ANY($2)`, id, ids); err != nil {
		return fmt.Errorf("lebro: delete messages: %w", postgresError(err))
	}
	return nil
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
	var threadID any
	if v.ThreadID != "" {
		threadID = string(v.ThreadID)
	}
	var finishedAt any
	if v.FinishedAt != nil {
		finishedAt = v.FinishedAt.UTC()
	}
	stepOutputs, err := postgresJSONArray(v.StepOutputs)
	if err != nil {
		return fmt.Errorf("lebro: save workflow run %q: %w", v.ID, err)
	}
	failure, err := postgresFailureJSON(v.Failure)
	if err != nil {
		return fmt.Errorf("lebro: save workflow run %q: %w", v.ID, err)
	}
	path, err := postgresStepIDArray(v.Path)
	if err != nil {
		return fmt.Errorf("lebro: save workflow run %q: %w", v.ID, err)
	}
	fanOut, err := postgresFanOutJSON(v.FanOut)
	if err != nil {
		return fmt.Errorf("lebro: save workflow run %q: %w", v.ID, err)
	}
	if _, err := r.q.ExecContext(ctx, `INSERT INTO workflow_runs (id, workflow_id, thread_id, status, input, output, metadata, started_at, finished_at, updated_at, current_step, current_step_id, step_outputs, failure, workflow_version, path, fan_out)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		ON CONFLICT (id) DO UPDATE SET
			workflow_id      = EXCLUDED.workflow_id,
			thread_id        = EXCLUDED.thread_id,
			status           = EXCLUDED.status,
			input            = EXCLUDED.input,
			output           = EXCLUDED.output,
			metadata         = EXCLUDED.metadata,
			started_at       = EXCLUDED.started_at,
			finished_at      = EXCLUDED.finished_at,
			updated_at       = EXCLUDED.updated_at,
			current_step     = EXCLUDED.current_step,
			current_step_id  = EXCLUDED.current_step_id,
			step_outputs     = EXCLUDED.step_outputs,
			failure          = EXCLUDED.failure,
			workflow_version = EXCLUDED.workflow_version,
			path             = EXCLUDED.path,
			fan_out          = EXCLUDED.fan_out`,
		v.ID, v.WorkflowID, threadID, v.Status, postgresJSON(v.Input), postgresJSON(v.Output), postgresJSON(v.Metadata), v.StartedAt.UTC(), finishedAt, v.UpdatedAt.UTC(),
		v.CurrentStep, string(v.CurrentStepID), stepOutputs, failure, v.WorkflowVersion, path, fanOut); err != nil {
		return fmt.Errorf("lebro: save workflow run %q: %w", v.ID, postgresError(err))
	}
	return nil
}

func (r *postgresRepositories) GetWorkflowRun(ctx context.Context, id RunID) (WorkflowRunRecord, error) {
	if err := ctx.Err(); err != nil {
		return WorkflowRunRecord{}, err
	}
	row := r.q.QueryRowContext(ctx, `SELECT id, workflow_id, thread_id, status, input, output, metadata, started_at, finished_at, updated_at, current_step, current_step_id, step_outputs, failure, workflow_version, path, fan_out FROM workflow_runs WHERE id = $1`, id)
	record, err := scanWorkflowRunPG(row)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkflowRunRecord{}, ErrNotFound
	}
	if err != nil {
		return WorkflowRunRecord{}, fmt.Errorf("lebro: get workflow run %q: %w", id, postgresError(err))
	}
	return record, nil
}

func (r *postgresRepositories) ListWorkflowRuns(ctx context.Context, filter WorkflowRunFilter, p PageRequest) (Page[WorkflowRunRecord], error) {
	if err := ctx.Err(); err != nil {
		return Page[WorkflowRunRecord]{}, err
	}
	offset, limit, err := sqlPageBounds(p)
	if err != nil {
		return Page[WorkflowRunRecord]{}, err
	}
	query := `SELECT id, workflow_id, thread_id, status, input, output, metadata, started_at, finished_at, updated_at, current_step, current_step_id, step_outputs, failure, workflow_version, path, fan_out FROM workflow_runs`
	args := []any{}
	param := 1
	where := []string{}
	if filter.WorkflowID != "" {
		where = append(where, fmt.Sprintf("workflow_id = $%d", param))
		args = append(args, filter.WorkflowID)
		param++
	}
	if filter.Status != "" {
		where = append(where, fmt.Sprintf("status = $%d", param))
		args = append(args, filter.Status)
		param++
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += fmt.Sprintf(" ORDER BY started_at, id LIMIT $%d OFFSET $%d", param, param+1)
	args = append(args, postgresFetchLimit(limit), offset)
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return Page[WorkflowRunRecord]{}, fmt.Errorf("lebro: list workflow runs: %w", postgresError(err))
	}
	defer func() { _ = rows.Close() }()
	return scanWorkflowRunPagePG(rows, offset, limit)
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
	if _, err := r.q.ExecContext(ctx, `INSERT INTO workflow_snapshots (id, run_id, sequence, schema_version, state, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		v.ID, v.RunID, v.Sequence, v.SchemaVersion, postgresJSON(v.State), v.CreatedAt.UTC()); err != nil {
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
	rows, err := r.q.QueryContext(ctx, `SELECT id, run_id, sequence, schema_version, state, created_at FROM workflow_snapshots WHERE run_id = $1 ORDER BY sequence LIMIT $2 OFFSET $3`, id, postgresFetchLimit(limit), offset)
	if err != nil {
		return Page[WorkflowSnapshotRecord]{}, fmt.Errorf("lebro: list workflow snapshots for run %q: %w", id, postgresError(err))
	}
	defer func() { _ = rows.Close() }()
	return scanSnapshotPagePG(rows, offset, limit)
}

func (r *postgresRepositories) SaveSchedule(ctx context.Context, v ScheduleRecord) error {
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
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (id) DO UPDATE SET
			workflow_id  = EXCLUDED.workflow_id,
			spec         = EXCLUDED.spec,
			paused       = EXCLUDED.paused,
			concurrency  = EXCLUDED.concurrency,
			input        = EXCLUDED.input,
			metadata     = EXCLUDED.metadata,
			next_fire_at = EXCLUDED.next_fire_at,
			last_fire_at = EXCLUDED.last_fire_at,
			wake_run_id  = EXCLUDED.wake_run_id,
			wake_token   = EXCLUDED.wake_token,
			created_at   = EXCLUDED.created_at,
			updated_at   = EXCLUDED.updated_at`,
		v.ID, v.WorkflowID, v.Spec, v.Paused, string(v.Concurrency), postgresJSON(v.Input), postgresJSON(v.Metadata),
		postgresNullableTime(v.NextFireAt), postgresNullableTime(v.LastFireAt), v.WakeRunID, v.WakeToken, v.CreatedAt.UTC(), v.UpdatedAt.UTC()); err != nil {
		return fmt.Errorf("lebro: save schedule %q: %w", v.ID, postgresError(err))
	}
	return nil
}

func (r *postgresRepositories) GetSchedule(ctx context.Context, id ScheduleID) (ScheduleRecord, error) {
	if err := ctx.Err(); err != nil {
		return ScheduleRecord{}, err
	}
	row := r.q.QueryRowContext(ctx, `SELECT id, workflow_id, spec, paused, concurrency, input, metadata, next_fire_at, last_fire_at, wake_run_id, wake_token, created_at, updated_at FROM schedules WHERE id = $1`, id)
	record, err := scanSchedulePG(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ScheduleRecord{}, ErrNotFound
	}
	if err != nil {
		return ScheduleRecord{}, fmt.Errorf("lebro: get schedule %q: %w", id, postgresError(err))
	}
	return record, nil
}

func (r *postgresRepositories) ListSchedules(ctx context.Context, filter ScheduleFilter, p PageRequest) (Page[ScheduleRecord], error) {
	if err := ctx.Err(); err != nil {
		return Page[ScheduleRecord]{}, err
	}
	offset, limit, err := sqlPageBounds(p)
	if err != nil {
		return Page[ScheduleRecord]{}, err
	}
	query := `SELECT id, workflow_id, spec, paused, concurrency, input, metadata, next_fire_at, last_fire_at, wake_run_id, wake_token, created_at, updated_at FROM schedules`
	args := []any{}
	param := 1
	where := []string{}
	if filter.WorkflowID != "" {
		where = append(where, fmt.Sprintf("workflow_id = $%d", param))
		args = append(args, filter.WorkflowID)
		param++
	}
	if filter.DueBy != nil {
		where = append(where, "paused = FALSE", "next_fire_at IS NOT NULL", fmt.Sprintf("next_fire_at <= $%d", param))
		args = append(args, filter.DueBy.UTC())
		param++
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += fmt.Sprintf(" ORDER BY created_at, id LIMIT $%d OFFSET $%d", param, param+1)
	args = append(args, postgresFetchLimit(limit), offset)
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return Page[ScheduleRecord]{}, fmt.Errorf("lebro: list schedules: %w", postgresError(err))
	}
	defer func() { _ = rows.Close() }()
	return scanSchedulePagePG(rows, offset, limit)
}

func (r *postgresRepositories) DeleteSchedule(ctx context.Context, id ScheduleID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	result, err := r.q.ExecContext(ctx, `DELETE FROM schedules WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("lebro: delete schedule %q: %w", id, postgresError(err))
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("lebro: delete schedule %q: %w", id, postgresError(err))
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *postgresRepositories) SaveScheduleExecution(ctx context.Context, v ScheduleExecutionRecord) error {
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
	switch err := r.q.QueryRowContext(ctx, `SELECT id FROM schedule_executions WHERE schedule_id = $1 AND id = $2`, v.ScheduleID, string(v.ID)).Scan(&found); {
	case err == nil:
		return errors.New("lebro: schedule execution already exists")
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("lebro: check schedule execution %q: %w", v.ID, postgresError(err))
	}
	var runID any
	if v.RunID != "" {
		runID = string(v.RunID)
	}
	if _, err := r.q.ExecContext(ctx, `INSERT INTO schedule_executions (id, schedule_id, run_id, status, scheduled_for, started_at, finished_at, error) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		string(v.ID), v.ScheduleID, runID, string(v.Status), v.ScheduledFor.UTC(), v.StartedAt.UTC(), postgresNullableTime(v.FinishedAt), v.Error); err != nil {
		return fmt.Errorf("lebro: save schedule execution %q: %w", v.ID, postgresError(err))
	}
	return nil
}

func (r *postgresRepositories) ListScheduleExecutions(ctx context.Context, id ScheduleID, p PageRequest) (Page[ScheduleExecutionRecord], error) {
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
	rows, err := r.q.QueryContext(ctx, `SELECT id, schedule_id, run_id, status, scheduled_for, started_at, finished_at, error FROM schedule_executions WHERE schedule_id = $1 ORDER BY seq LIMIT $2 OFFSET $3`, id, postgresFetchLimit(limit), offset)
	if err != nil {
		return Page[ScheduleExecutionRecord]{}, fmt.Errorf("lebro: list schedule executions for schedule %q: %w", id, postgresError(err))
	}
	defer func() { _ = rows.Close() }()
	return scanScheduleExecutionPagePG(rows, offset, limit)
}

func (r *postgresRepositories) scheduleExists(ctx context.Context, id ScheduleID) error {
	var found int
	err := r.q.QueryRowContext(ctx, `SELECT 1 FROM schedules WHERE id = $1`, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lebro: postgres: %w", postgresError(err))
	}
	return nil
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
	case "23505", "40001", "55P03":
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
	var input, output, metadata, stepOutputs, failure, path, fanOut sql.NullString
	var finishedAt sql.NullTime
	if err := row.Scan(&record.ID, &record.WorkflowID, &threadID, &record.Status, &input, &output, &metadata, &record.StartedAt, &finishedAt, &record.UpdatedAt, &record.CurrentStep, &record.CurrentStepID, &stepOutputs, &failure, &record.WorkflowVersion, &path, &fanOut); err != nil {
		return WorkflowRunRecord{}, err
	}
	if threadID.Valid {
		record.ThreadID = ThreadID(threadID.String)
	}
	record.Input = postgresRawJSON([]byte(input.String))
	record.Output = postgresRawJSON([]byte(output.String))
	record.Metadata = postgresRawJSON([]byte(metadata.String))
	record.StepOutputs = postgresRawJSONArray(stepOutputs)
	record.Failure = postgresParseFailure(failure)
	record.Path = postgresRawStepIDArray(path)
	record.FanOut = postgresParseFanOut(fanOut)
	if finishedAt.Valid {
		finished := finishedAt.Time.UTC()
		record.FinishedAt = &finished
	}
	record.StartedAt = record.StartedAt.UTC()
	record.UpdatedAt = record.UpdatedAt.UTC()
	return record, nil
}

func scanWorkflowRunPagePG(rows *sql.Rows, offset, limit int) (Page[WorkflowRunRecord], error) {
	var page Page[WorkflowRunRecord]
	for rows.Next() && len(page.Records) <= limit {
		record, err := scanWorkflowRunPG(rows)
		if err != nil {
			return Page[WorkflowRunRecord]{}, fmt.Errorf("lebro: scan workflow run: %w", postgresError(err))
		}
		page.Records = append(page.Records, record)
	}
	if err := rows.Err(); err != nil {
		return Page[WorkflowRunRecord]{}, fmt.Errorf("lebro: list workflow runs: %w", postgresError(err))
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.NextCursor = strconv.Itoa(offset + limit)
	}
	return page, nil
}

func scanSnapshotPagePG(rows *sql.Rows, offset, limit int) (Page[WorkflowSnapshotRecord], error) {
	var page Page[WorkflowSnapshotRecord]
	for rows.Next() && len(page.Records) <= limit {
		var record WorkflowSnapshotRecord
		var state sql.NullString
		if err := rows.Scan(&record.ID, &record.RunID, &record.Sequence, &record.SchemaVersion, &state, &record.CreatedAt); err != nil {
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

func scanSchedulePG(row messagePageScanner) (ScheduleRecord, error) {
	var record ScheduleRecord
	var concurrency string
	var input, metadata sql.NullString
	var nextFireAt, lastFireAt sql.NullTime
	if err := row.Scan(&record.ID, &record.WorkflowID, &record.Spec, &record.Paused, &concurrency, &input, &metadata, &nextFireAt, &lastFireAt, &record.WakeRunID, &record.WakeToken, &record.CreatedAt, &record.UpdatedAt); err != nil {
		return ScheduleRecord{}, err
	}
	record.Concurrency = ConcurrencyPolicy(concurrency)
	record.Input = postgresRawJSON([]byte(input.String))
	record.Metadata = postgresRawJSON([]byte(metadata.String))
	if nextFireAt.Valid {
		next := nextFireAt.Time.UTC()
		record.NextFireAt = &next
	}
	if lastFireAt.Valid {
		last := lastFireAt.Time.UTC()
		record.LastFireAt = &last
	}
	record.CreatedAt = record.CreatedAt.UTC()
	record.UpdatedAt = record.UpdatedAt.UTC()
	return record, nil
}

func scanSchedulePagePG(rows *sql.Rows, offset, limit int) (Page[ScheduleRecord], error) {
	var page Page[ScheduleRecord]
	for rows.Next() && len(page.Records) <= limit {
		record, err := scanSchedulePG(rows)
		if err != nil {
			return Page[ScheduleRecord]{}, fmt.Errorf("lebro: scan schedule: %w", postgresError(err))
		}
		page.Records = append(page.Records, record)
	}
	if err := rows.Err(); err != nil {
		return Page[ScheduleRecord]{}, fmt.Errorf("lebro: list schedules: %w", postgresError(err))
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.NextCursor = strconv.Itoa(offset + limit)
	}
	return page, nil
}

func scanScheduleExecutionPagePG(rows *sql.Rows, offset, limit int) (Page[ScheduleExecutionRecord], error) {
	var page Page[ScheduleExecutionRecord]
	for rows.Next() && len(page.Records) <= limit {
		var record ScheduleExecutionRecord
		var runID sql.NullString
		var status string
		var finishedAt sql.NullTime
		if err := rows.Scan(&record.ID, &record.ScheduleID, &runID, &status, &record.ScheduledFor, &record.StartedAt, &finishedAt, &record.Error); err != nil {
			return Page[ScheduleExecutionRecord]{}, fmt.Errorf("lebro: scan schedule execution: %w", postgresError(err))
		}
		if runID.Valid {
			record.RunID = RunID(runID.String)
		}
		record.Status = ScheduleExecStatus(status)
		record.ScheduledFor = record.ScheduledFor.UTC()
		record.StartedAt = record.StartedAt.UTC()
		if finishedAt.Valid {
			finished := finishedAt.Time.UTC()
			record.FinishedAt = &finished
		}
		page.Records = append(page.Records, record)
	}
	if err := rows.Err(); err != nil {
		return Page[ScheduleExecutionRecord]{}, fmt.Errorf("lebro: list schedule executions: %w", postgresError(err))
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

// postgresNullableTime returns the UTC time for a non-nil pointer or nil so the
// column is written as SQL NULL.
func postgresNullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
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

// postgresJSONArray marshals a slice of JSON values into a JSON array string
// for the step_outputs column. A nil slice becomes NULL so readers can
// distinguish "no outputs" from "empty array".
func postgresJSONArray(outputs []json.RawMessage) (any, error) {
	if len(outputs) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(outputs)
	if err != nil {
		return nil, fmt.Errorf("lebro: encode step outputs: %w", err)
	}
	return string(encoded), nil
}

// postgresRawJSONArray decodes a nullable JSON-array column back into a slice.
// NULL, empty, and invalid JSON all yield nil so a corrupted row remains
// inspectable instead of failing the read.
func postgresRawJSONArray(v sql.NullString) []json.RawMessage {
	if !v.Valid || v.String == "" || v.String == "null" {
		return nil
	}
	var outputs []json.RawMessage
	if err := json.Unmarshal([]byte(v.String), &outputs); err != nil {
		return nil
	}
	return outputs
}

// postgresFailureJSON marshals failure data for the failure column. nil
// becomes NULL so readers can distinguish "no failure" from "empty object".
func postgresFailureJSON(failure *WorkflowFailureData) (any, error) {
	if failure == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(failure)
	if err != nil {
		return nil, fmt.Errorf("lebro: encode workflow failure: %w", err)
	}
	return string(encoded), nil
}

// postgresParseFailure decodes a nullable failure column. NULL, empty, and
// invalid JSON all yield nil; a corrupted row stays inspectable.
func postgresParseFailure(v sql.NullString) *WorkflowFailureData {
	if !v.Valid || v.String == "" || v.String == "null" {
		return nil
	}
	var failure WorkflowFailureData
	if err := json.Unmarshal([]byte(v.String), &failure); err != nil {
		return nil
	}
	return &failure
}

// postgresStepIDArray marshals a slice of StepIDs into a JSON array string for
// the path column. A nil slice becomes NULL so readers can distinguish "no
// path" from "empty path".
func postgresStepIDArray(ids []StepID) (any, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, fmt.Errorf("lebro: encode path: %w", err)
	}
	return string(encoded), nil
}

// postgresRawStepIDArray decodes a nullable JSON-array column of StepIDs back
// into a slice. NULL, empty, and invalid JSON all yield nil so a corrupted
// row remains inspectable instead of failing the read.
func postgresRawStepIDArray(v sql.NullString) []StepID {
	if !v.Valid || v.String == "" || v.String == "null" {
		return nil
	}
	var ids []StepID
	if err := json.Unmarshal([]byte(v.String), &ids); err != nil {
		return nil
	}
	return ids
}

// postgresFanOutJSON marshals fan-out join results for the fan_out column. nil
// or empty becomes NULL so readers can distinguish "no fan-out" from "empty array".
func postgresFanOutJSON(results []FanOutJoinResult) (any, error) {
	if len(results) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(results)
	if err != nil {
		return nil, fmt.Errorf("lebro: encode fan-out: %w", err)
	}
	return string(encoded), nil
}

// postgresParseFanOut decodes a nullable fan_out column. NULL, empty, and
// invalid JSON all yield nil; a corrupted row stays inspectable.
func postgresParseFanOut(v sql.NullString) []FanOutJoinResult {
	if !v.Valid || v.String == "" || v.String == "null" {
		return nil
	}
	var results []FanOutJoinResult
	if err := json.Unmarshal([]byte(v.String), &results); err != nil {
		return nil
	}
	return results
}

// stringsBuilder is a thin wrapper around a string buffer for building
// multi-row VALUES clauses.
type stringsBuilder struct{ buf []byte }

func (b *stringsBuilder) WriteString(s string) { b.buf = append(b.buf, s...) }
func (b *stringsBuilder) String() string       { return string(b.buf) }
