package runtime_test

import (
	"context"
	"testing"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
)

func TestSQLiteVectorStorePassesContract(t *testing.T) {
	testkit.VectorContractSuite(t, func(t *testing.T) lebro.VectorStore {
		t.Helper()
		store, err := lebro.NewSQLiteVectorStore(t.TempDir() + "/vector.db")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		if err := store.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		return store
	})
}
