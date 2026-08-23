package runtime_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
)

// TestPostgresVectorStorePassesContract runs the shared adapter-neutral vector
// contract suite against a disposable PostgreSQL instance with the pgvector
// extension. Set LEBRO_POSTGRES_TEST_DSN to opt in; the suite is skipped
// otherwise.
func TestPostgresVectorStorePassesContract(t *testing.T) {
	dsn := os.Getenv("LEBRO_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("skipping PostgreSQL vector contract suite: set LEBRO_POSTGRES_TEST_DSN to a disposable database DSN with pgvector installed")
	}
	cleanupDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanupDB.Close() }()

	testkit.VectorContractSuite(t, func(t *testing.T) lebro.VectorStore {
		t.Helper()
		ctx := context.Background()
		for _, table := range []string{"vector_records", "vector_indices", "vector_schema_migrations"} {
			if _, err := cleanupDB.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE", table)); err != nil {
				t.Fatalf("drop %s: %v", table, err)
			}
		}
		store, err := lebro.NewPostgresVectorStore(dsn, lebro.PostgresVectorStoreOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		if err := store.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		return store
	})
}

func TestPostgresThreadHistoryPassesDeletionContract(t *testing.T) {
	dsn := os.Getenv("LEBRO_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("skipping PostgreSQL thread-history contract suite: set LEBRO_POSTGRES_TEST_DSN to a disposable database DSN with pgvector installed")
	}
	cleanupDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanupDB.Close() }()

	testkit.ThreadHistoryContractSuite(t,
		func(t *testing.T) lebro.Store {
			t.Helper()
			ctx := context.Background()
			for _, table := range []string{"vector_records", "vector_indices", "vector_schema_migrations", "messages", "threads", "schema_migrations"} {
				if _, err := cleanupDB.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE", table)); err != nil {
					t.Fatalf("drop %s: %v", table, err)
				}
			}
			store, err := lebro.NewPostgresStore(dsn, lebro.PostgresStoreOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			if err := store.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			return store
		},
		func(t *testing.T) lebro.VectorStore {
			t.Helper()
			store, err := lebro.NewPostgresVectorStore(dsn, lebro.PostgresVectorStoreOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			if err := store.Migrate(context.Background()); err != nil {
				t.Fatal(err)
			}
			return store
		},
	)
}
