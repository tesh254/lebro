package runtime_test

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
)

// TestQdrantVectorStorePassesContract targets a disposable Qdrant server.
// Set LEBRO_QDRANT_TEST_HOST (and optionally LEBRO_QDRANT_TEST_PORT) to opt in.
func TestQdrantVectorStorePassesContract(t *testing.T) {
	host := os.Getenv("LEBRO_QDRANT_TEST_HOST")
	if host == "" {
		t.Skip("skipping Qdrant vector contract suite: set LEBRO_QDRANT_TEST_HOST to a disposable Qdrant gRPC endpoint")
	}
	port := 6334
	if raw := os.Getenv("LEBRO_QDRANT_TEST_PORT"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("parse LEBRO_QDRANT_TEST_PORT: %v", err)
		}
		port = parsed
	}
	testkit.VectorContractSuite(t, func(t *testing.T) lebro.VectorStore {
		t.Helper()
		store, err := lebro.NewQdrantVectorStore(lebro.QdrantVectorStoreConfig{Host: host, Port: port, PoolSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		t.Cleanup(func() { _ = store.DeleteIndex(context.Background(), "docs") })
		return store
	})
}
