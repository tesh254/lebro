package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pgvector/pgvector-go"
)

// PostgresVectorStore is a VectorStore backed by PostgreSQL with the pgvector
// extension. It uses the vector type and cosine similarity operator (<=>)
// for efficient similarity search. The pgvector extension must be installed
// on the target database before Migrate is called.
//
// The store is intentionally separate from PostgresStore so vector storage
// remains optional. It can share the same database as PostgresStore because
// it tracks its own schema version in a dedicated table.
type PostgresVectorStore struct {
	db *sql.DB
}

// PostgresVectorStoreOptions tunes connection-pool behavior. A zero value
// leaves the database/sql defaults in place.
type PostgresVectorStoreOptions struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxIdleTime time.Duration
}

// postgresVectorMigrations installs the vector schema. The version is tracked
// in vector_schema_migrations. Migrations must be append-only.
var postgresVectorMigrations = []string{
	`CREATE EXTENSION IF NOT EXISTS vector`,
	`CREATE TABLE IF NOT EXISTS vector_schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`CREATE TABLE IF NOT EXISTS vector_indices (
		name      TEXT PRIMARY KEY,
		dimension INTEGER NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`CREATE TABLE IF NOT EXISTS vector_records (
		id         TEXT NOT NULL,
		index_name TEXT NOT NULL REFERENCES vector_indices(name) ON DELETE CASCADE,
		vector     vector NOT NULL,
		dimension  INTEGER NOT NULL,
		metadata   TEXT,
		content    TEXT,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		UNIQUE (index_name, id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_vector_records_index ON vector_records(index_name)`,
}

// NewPostgresVectorStore opens a PostgreSQL connection pool at dsn and returns
// a vector store. The DSN must be a libpq-style connection string. The
// database is left uninitialized; call Migrate to install the vector schema
// (requires the pgvector extension).
func NewPostgresVectorStore(dsn string, opts PostgresVectorStoreOptions) (*PostgresVectorStore, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("lebro: postgres vector: parse DSN %q: %w", dsn, err)
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
		return nil, fmt.Errorf("lebro: postgres vector: connect to %q: %w", dsn, err)
	}
	return &PostgresVectorStore{db: db}, nil
}

// Close releases the underlying connection pool.
func (s *PostgresVectorStore) Close() error { return s.db.Close() }

// Migrate applies any pending vector schema migrations atomically. It is
// idempotent. Requires the pgvector extension to be available.
func (s *PostgresVectorStore) Migrate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("lebro: postgres vector: begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var version int
	row := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM vector_schema_migrations")
	if err := row.Scan(&version); err != nil {
		version = 0
	}
	if version > len(postgresVectorMigrations) {
		return fmt.Errorf("lebro: postgres vector: schema version %d is newer than this build supports (max %d)", version, len(postgresVectorMigrations))
	}
	for i := version; i < len(postgresVectorMigrations); i++ {
		if _, err := tx.ExecContext(ctx, postgresVectorMigrations[i]); err != nil {
			return fmt.Errorf("lebro: postgres vector: migration %d failed: %w; database left unchanged", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO vector_schema_migrations (version) VALUES ($1)", i+1); err != nil {
			return fmt.Errorf("lebro: postgres vector: record migration %d: %w", i+1, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lebro: postgres vector: commit migration: %w", err)
	}
	return nil
}

func (s *PostgresVectorStore) CreateIndex(ctx context.Context, index string, dimension int) error {
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
		"INSERT INTO vector_indices (name, dimension) VALUES ($1, $2)",
		index, dimension)
	if err != nil {
		if isPostgresUniqueViolation(err) {
			return fmt.Errorf("%w: index %q", ErrVectorAlreadyExists, index)
		}
		return fmt.Errorf("lebro: postgres vector: create index: %w", err)
	}
	return nil
}

func (s *PostgresVectorStore) DeleteIndex(ctx context.Context, index string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if index == "" {
		return fmt.Errorf("%w: empty index name", ErrVectorInvalidInput)
	}
	result, err := s.db.ExecContext(ctx, "DELETE FROM vector_indices WHERE name = $1", index)
	if err != nil {
		return fmt.Errorf("lebro: postgres vector: delete index: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("lebro: postgres vector: delete index rows: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: index %q", ErrVectorNotFound, index)
	}
	return nil
}

func (s *PostgresVectorStore) Upsert(ctx context.Context, records []EmbeddingRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("lebro: postgres vector: begin upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, record := range records {
		if err := validateVectorRecord(record); err != nil {
			return err
		}
		var idxDimension int
		err := tx.QueryRowContext(ctx, "SELECT dimension FROM vector_indices WHERE name = $1", record.Index).Scan(&idxDimension)
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: index %q", ErrVectorNotFound, record.Index)
		}
		if err != nil {
			return fmt.Errorf("lebro: postgres vector: lookup index: %w", err)
		}
		if len(record.Vector) != idxDimension {
			return fmt.Errorf("%w: record %q has dimension %d, index %q expects %d", ErrVectorInvalidDimension, record.ID, len(record.Vector), record.Index, idxDimension)
		}
		vec := pgvector.NewVector(record.Vector)
		var metaVal any
		if len(record.Metadata) > 0 {
			metaVal = string(record.Metadata)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO vector_records (id, index_name, vector, dimension, metadata, content)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (index_name, id) DO UPDATE SET
			   vector = excluded.vector,
			   dimension = excluded.dimension,
			   metadata = excluded.metadata,
			   content = excluded.content`,
			record.ID, record.Index, vec, len(record.Vector), metaVal, record.Content)
		if err != nil {
			return fmt.Errorf("lebro: postgres vector: upsert record %q: %w", record.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lebro: postgres vector: commit upsert: %w", err)
	}
	return nil
}

func (s *PostgresVectorStore) Delete(ctx context.Context, index string, ids []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if index == "" {
		return fmt.Errorf("%w: empty index name", ErrVectorInvalidInput)
	}
	var exists int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM vector_indices WHERE name = $1", index).Scan(&exists)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: index %q", ErrVectorNotFound, index)
	}
	if err != nil {
		return fmt.Errorf("lebro: postgres vector: lookup index: %w", err)
	}
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, "DELETE FROM vector_records WHERE index_name = $1 AND id = $2", index, id); err != nil {
			return fmt.Errorf("lebro: postgres vector: delete record %q: %w", id, err)
		}
	}
	return nil
}

func (s *PostgresVectorStore) Search(ctx context.Context, query SimilarityQuery) ([]SimilarityResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateSimilarityQuery(query); err != nil {
		return nil, err
	}
	var idxDimension int
	err := s.db.QueryRowContext(ctx, "SELECT dimension FROM vector_indices WHERE name = $1", query.Index).Scan(&idxDimension)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: index %q", ErrVectorNotFound, query.Index)
	}
	if err != nil {
		return nil, fmt.Errorf("lebro: postgres vector: lookup index: %w", err)
	}
	if len(query.Vector) != idxDimension {
		return nil, fmt.Errorf("%w: query vector has dimension %d, index %q expects %d", ErrVectorInvalidDimension, len(query.Vector), query.Index, idxDimension)
	}
	vec := pgvector.NewVector(query.Vector)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, 1 - (vector <=> $1) AS score, metadata, content
		 FROM vector_records WHERE index_name = $2
		 ORDER BY vector <=> $1`,
		vec, query.Index)
	if err != nil {
		return nil, fmt.Errorf("lebro: postgres vector: query records: %w", err)
	}
	defer func() { _ = rows.Close() }()
	results := []SimilarityResult{}
	for rows.Next() {
		var id string
		var score float64
		var metadata, content sql.NullString
		if err := rows.Scan(&id, &score, &metadata, &content); err != nil {
			return nil, fmt.Errorf("lebro: postgres vector: scan record: %w", err)
		}
		if query.MinScore > 0 && float32(score) < query.MinScore {
			continue
		}
		if !metadataMatches(json.RawMessage(metadata.String), query.Filter) {
			continue
		}
		var metaRaw json.RawMessage
		if metadata.Valid && metadata.String != "" {
			metaRaw = json.RawMessage(metadata.String)
		}
		results = append(results, SimilarityResult{
			ID:       id,
			Score:    float32(score),
			Metadata: cloneJSON(metaRaw),
			Content:  content.String,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lebro: postgres vector: rows: %w", err)
	}
	// pgvector ORDER BY already sorts by distance; trim to TopK.
	if query.TopK > 0 && query.TopK < len(results) {
		results = results[:query.TopK]
	}
	return results, nil
}

// isPostgresUniqueViolation reports whether err is a PostgreSQL unique
// constraint violation (SQLSTATE 23505).
func isPostgresUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23505"
}

// pgvectorVectorToString converts a pgvector.Vector to a string representation
// for debugging. Unused but kept for reference.
var _ = func(v pgvector.Vector) string { return strings.TrimPrefix(v.String(), "[") }
