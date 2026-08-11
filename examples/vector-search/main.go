// vector-search demonstrates the vector storage contracts with the in-memory
// adapter. It creates an index, upserts embeddings with metadata, and runs a
// similarity search with a metadata filter. Production adapters (SQLite,
// PostgreSQL with pgvector) implement the same VectorStore interface.
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tesh254/lebro"
)

func main() {
	ctx := context.Background()
	store := lebro.NewMemoryVectorStore()

	must(store.CreateIndex(ctx, "documents", 3))

	must(store.Upsert(ctx, []lebro.EmbeddingRecord{
		{ID: "doc-1", Index: "documents", Vector: []float32{1, 0, 0}, Metadata: json.RawMessage(`{"source":"api"}`), Content: "Hello world"},
		{ID: "doc-2", Index: "documents", Vector: []float32{0, 1, 0}, Metadata: json.RawMessage(`{"source":"web"}`), Content: "Goodbye world"},
		{ID: "doc-3", Index: "documents", Vector: []float32{1, 1, 0}, Metadata: json.RawMessage(`{"source":"api"}`), Content: "Hello again"},
	}))

	results := mustValue(store.Search(ctx, lebro.SimilarityQuery{
		Vector: []float32{1, 0, 0},
		Index:  "documents",
		Filter: lebro.VectorMetadataFilter{
			Match: map[string]json.RawMessage{"source": json.RawMessage(`"api"`)},
		},
		TopK: 5,
	}))

	fmt.Printf("found %d result(s):\n", len(results))
	for _, r := range results {
		fmt.Printf("  %s (score=%.4f): %s\n", r.ID, r.Score, r.Content)
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func mustValue[T any](value T, err error) T {
	must(err)
	return value
}
