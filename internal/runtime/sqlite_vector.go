package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
)

// SQLiteVectorStore is a file-backed VectorStore backed by SQLite. Vectors are
// stored as JSON TEXT to preserve binary round-trip fidelity with the memory
// adapter. Similarity search is brute-force cosine in Go, suitable for
// development and small datasets.
//
// The store is intentionally separate from SQLiteStore so vector storage
// remains optional: agent and workflow code never reference it. It can
// share the same database file as SQLiteStore because it tracks its own
// schema version in a dedicated table.
type SQLiteVectorStore struct {
	db *sql.DB
}

// sqliteVectorMigrations installs the vector schema one statement at a time.
// The version is tracked in vector_schema_migrations (created inside the
// migration transaction by sqliteVectorBootstrapSQL so a failed initial
// migration rolls it back). Migrations must be append-only; never reorder or
// edit an applied step.
var sqliteVectorMigrations = []string{
	`CREATE TABLE IF NOT EXISTS vector_indices (
		name      TEXT PRIMARY KEY,
		dimension INTEGER NOT NULL,
		created_at TEXT NOT NULL
	)`,
	`CREATE TABLE vector_records (
		id         TEXT NOT NULL,
		index_name TEXT NOT NULL REFERENCES vector_indices(name) ON DELETE CASCADE,
		vector     TEXT NOT NULL,
		dimension  INTEGER NOT NULL,
		metadata   TEXT,
		content    TEXT,
		created_at TEXT NOT NULL,
		UNIQUE (index_name, id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_vector_records_index ON vector_records(index_name)`,
}

// NewSQLiteVectorStore opens (or creates) the SQLite database at dsn and
// returns a vector store. The database is left uninitialized; call Migrate
// to install the vector schema.
func NewSQLiteVectorStore(dsn string) (*SQLiteVectorStore, error) {
	db, err := sql.Open("sqlite", sqliteDSN(dsn))
	if err != nil {
		return nil, fmt.Errorf("lebro: sqlite vector: open %q: %w", dsn, err)
	}
	privateMemory := strings.Contains(dsn, ":memory:") || strings.Contains(dsn, "mode=memory")
	if privateMemory && !strings.Contains(dsn, "cache=shared") {
		db.SetMaxOpenConns(1)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("lebro: sqlite vector: connect to %q: %w", dsn, err)
	}
	return &SQLiteVectorStore{db: db}, nil
}

// Close releases the underlying database handle.
func (s *SQLiteVectorStore) Close() error { return s.db.Close() }

// sqliteVectorBootstrapSQL creates the migration tracking table. It runs
// inside the migration transaction so a failed initial migration rolls it
// back, preserving the all-or-nothing rollback guarantee.
const sqliteVectorBootstrapSQL = `CREATE TABLE IF NOT EXISTS vector_schema_migrations (
	version    INTEGER PRIMARY KEY,
	applied_at TEXT NOT NULL DEFAULT (datetime('now'))
)`

// Migrate applies any pending vector schema migrations atomically. It is
// idempotent; a database already at the current version is a no-op. The
// tracking table is created inside the migration transaction so a failed
// initial migration leaves no artifacts.
func (s *SQLiteVectorStore) Migrate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("lebro: sqlite vector: begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Create the tracking table inside the tx so a failed initial migration
	// rolls it back. IF NOT EXISTS makes this a no-op on subsequent runs.
	if _, err := tx.ExecContext(ctx, sqliteVectorBootstrapSQL); err != nil {
		return fmt.Errorf("lebro: sqlite vector: ensure schema_migrations table: %w", err)
	}

	var version int
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM vector_schema_migrations").Scan(&version); err != nil {
		return fmt.Errorf("lebro: sqlite vector: read schema version: %w", err)
	}
	if version > len(sqliteVectorMigrations) {
		return fmt.Errorf("lebro: sqlite vector: schema version %d is newer than this build supports (max %d)", version, len(sqliteVectorMigrations))
	}
	for i := version; i < len(sqliteVectorMigrations); i++ {
		if _, err := tx.ExecContext(ctx, sqliteVectorMigrations[i]); err != nil {
			return fmt.Errorf("lebro: sqlite vector: migration %d failed: %w; database left unchanged", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO vector_schema_migrations (version) VALUES (?)", i+1); err != nil {
			return fmt.Errorf("lebro: sqlite vector: record migration %d: %w", i+1, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lebro: sqlite vector: commit migration: %w", sqliteError(err))
	}
	return nil
}

func (s *SQLiteVectorStore) CreateIndex(ctx context.Context, index string, dimension int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if index == "" {
		return fmt.Errorf("%w: empty index name", ErrVectorInvalidInput)
	}
	if dimension <= 0 {
		return fmt.Errorf("%w: dimension must be positive", ErrVectorInvalidInput)
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO vector_indices (name, dimension, created_at) VALUES (?, ?, datetime('now'))",
		index, dimension)
	if err != nil {
		if isSQLiteUniqueConstraint(err) {
			return fmt.Errorf("%w: index %q", ErrVectorAlreadyExists, index)
		}
		return fmt.Errorf("lebro: sqlite vector: create index: %w", sqliteError(err))
	}
	return nil
}

func (s *SQLiteVectorStore) DeleteIndex(ctx context.Context, index string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if index == "" {
		return fmt.Errorf("%w: empty index name", ErrVectorInvalidInput)
	}
	result, err := s.db.ExecContext(ctx, "DELETE FROM vector_indices WHERE name = ?", index)
	if err != nil {
		return fmt.Errorf("lebro: sqlite vector: delete index: %w", sqliteError(err))
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("lebro: sqlite vector: delete index rows: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: index %q", ErrVectorNotFound, index)
	}
	return nil
}

func (s *SQLiteVectorStore) Upsert(ctx context.Context, records []EmbeddingRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("lebro: sqlite vector: begin upsert: %w", sqliteError(err))
	}
	defer func() { _ = tx.Rollback() }()
	for _, record := range records {
		if err := validateVectorRecord(record); err != nil {
			return err
		}
		var idxDimension int
		err := tx.QueryRowContext(ctx, "SELECT dimension FROM vector_indices WHERE name = ?", record.Index).Scan(&idxDimension)
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: index %q", ErrVectorNotFound, record.Index)
		}
		if err != nil {
			return fmt.Errorf("lebro: sqlite vector: lookup index: %w", sqliteError(err))
		}
		if len(record.Vector) != idxDimension {
			return fmt.Errorf("%w: record %q has dimension %d, index %q expects %d", ErrVectorInvalidDimension, record.ID, len(record.Vector), record.Index, idxDimension)
		}
		vecJSON, err := json.Marshal(record.Vector)
		if err != nil {
			return fmt.Errorf("lebro: sqlite vector: encode vector: %w", err)
		}
		var metaVal any
		if len(record.Metadata) > 0 {
			metaVal = string(record.Metadata)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO vector_records (id, index_name, vector, dimension, metadata, content, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (index_name, id) DO UPDATE SET
			   vector = excluded.vector,
			   dimension = excluded.dimension,
			   metadata = excluded.metadata,
			   content = excluded.content,
			   created_at = excluded.created_at`,
			record.ID, record.Index, string(vecJSON), len(record.Vector), metaVal, record.Content, sqliteTime(time.Now()))
		if err != nil {
			return fmt.Errorf("lebro: sqlite vector: upsert record %q: %w", record.ID, sqliteError(err))
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lebro: sqlite vector: commit upsert: %w", sqliteError(err))
	}
	return nil
}

func (s *SQLiteVectorStore) Delete(ctx context.Context, index string, ids []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if index == "" {
		return fmt.Errorf("%w: empty index name", ErrVectorInvalidInput)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("lebro: sqlite vector: begin delete: %w", sqliteError(err))
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	err = tx.QueryRowContext(ctx, "SELECT 1 FROM vector_indices WHERE name = ?", index).Scan(&exists)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: index %q", ErrVectorNotFound, index)
	}
	if err != nil {
		return fmt.Errorf("lebro: sqlite vector: lookup index: %w", sqliteError(err))
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, "DELETE FROM vector_records WHERE index_name = ? AND id = ?", index, id); err != nil {
			return fmt.Errorf("lebro: sqlite vector: delete record %q: %w", id, sqliteError(err))
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lebro: sqlite vector: commit delete: %w", sqliteError(err))
	}
	return nil
}

func (s *SQLiteVectorStore) Search(ctx context.Context, query SimilarityQuery) ([]SimilarityResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateSimilarityQuery(query); err != nil {
		return nil, err
	}
	var idxDimension int
	err := s.db.QueryRowContext(ctx, "SELECT dimension FROM vector_indices WHERE name = ?", query.Index).Scan(&idxDimension)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: index %q", ErrVectorNotFound, query.Index)
	}
	if err != nil {
		return nil, fmt.Errorf("lebro: sqlite vector: lookup index: %w", sqliteError(err))
	}
	if len(query.Vector) != idxDimension {
		return nil, fmt.Errorf("%w: query vector has dimension %d, index %q expects %d", ErrVectorInvalidDimension, len(query.Vector), query.Index, idxDimension)
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, vector, metadata, content FROM vector_records WHERE index_name = ?", query.Index)
	if err != nil {
		return nil, fmt.Errorf("lebro: sqlite vector: query records: %w", sqliteError(err))
	}
	defer func() { _ = rows.Close() }()
	results := []SimilarityResult{}
	for rows.Next() {
		var id, vecStr string
		var metadata, content sql.NullString
		if err := rows.Scan(&id, &vecStr, &metadata, &content); err != nil {
			return nil, fmt.Errorf("lebro: sqlite vector: scan record: %w", err)
		}
		var vec []float32
		if err := json.Unmarshal([]byte(vecStr), &vec); err != nil {
			return nil, fmt.Errorf("lebro: sqlite vector: decode vector for %q: %w", id, err)
		}
		if !metadataMatches(json.RawMessage(metadata.String), query.Filter) {
			continue
		}
		score := CosineSimilarity(query.Vector, vec)
		var metaRaw json.RawMessage
		if metadata.Valid && metadata.String != "" {
			metaRaw = json.RawMessage(metadata.String)
		}
		results = append(results, SimilarityResult{
			ID:       id,
			Score:    score,
			Metadata: cloneJSON(metaRaw),
			Content:  content.String,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lebro: sqlite vector: rows: %w", err)
	}
	return rankResults(results, query.TopK, query.MinScore), nil
}

// isSQLiteUniqueConstraint reports whether err is a SQLite UNIQUE constraint
// violation.
func isSQLiteUniqueConstraint(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	return strings.Contains(sqliteErr.Error(), "UNIQUE")
}
