package runtime_test

import (
	"context"
	"testing"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
)

// The runtime package itself cannot import testkit (it would form a test
// import cycle), so the shared storage contract suite runs from the external
// test package against both adapters.
func TestSQLiteStorePassesStorageContract(t *testing.T) {
	testkit.StorageContractSuite(t, func(t *testing.T) lebro.Store {
		t.Helper()
		store, err := lebro.NewSQLiteStore(t.TempDir() + "/contract.db")
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

func TestMemoryStorePassesStorageContract(t *testing.T) {
	testkit.StorageContractSuite(t, func(t *testing.T) lebro.Store {
		t.Helper()
		return lebro.NewMemoryStore()
	})
}

func TestMemoryVectorStorePassesContract(t *testing.T) {
	testkit.VectorContractSuite(t, func(t *testing.T) lebro.VectorStore {
		t.Helper()
		return lebro.NewMemoryVectorStore()
	})
}
