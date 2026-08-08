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

// TestPostgresStorePassesStorageContract runs the shared adapter-neutral
// storage contract suite against a disposable PostgreSQL instance. Set
// LEBRO_POSTGRES_TEST_DSN to opt in; the suite is skipped otherwise.
func TestPostgresStorePassesStorageContract(t *testing.T) {
	dsn := os.Getenv("LEBRO_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("skipping PostgreSQL contract suite: set LEBRO_POSTGRES_TEST_DSN to a disposable database DSN")
	}
	// A standalone connection used solely to drop tables between subtests so
	// each starts from a clean schema. The store itself is opened fresh by
	// the factory.
	cleanupDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanupDB.Close() }()

	testkit.StorageContractSuite(t, func(t *testing.T) lebro.Store {
		t.Helper()
		ctx := context.Background()
		for _, table := range []string{"workflow_snapshots", "workflow_runs", "messages", "threads", "schema_migrations"} {
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
	})
}
