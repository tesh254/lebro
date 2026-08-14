// vector-qdrant demonstrates Qdrant-backed cosine vector search. Start a
// disposable Qdrant server with: docker run --rm -p 6334:6334 qdrant/qdrant
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/tesh254/lebro"
)

func main() {
	ctx := context.Background()
	store, err := lebro.NewQdrantVectorStore(lebro.QdrantVectorStoreConfig{Host: "localhost", Port: 6334})
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	if err := store.CreateIndex(ctx, "documents", 3); err != nil && !errors.Is(err, lebro.ErrVectorAlreadyExists) {
		log.Fatal(err)
	}
	if err := store.Upsert(ctx, []lebro.EmbeddingRecord{{ID: "doc-1", Index: "documents", Vector: []float32{1, 0, 0}, Metadata: json.RawMessage(`{"tenant":"acme"}`), Content: "Qdrant stores vectors"}}); err != nil {
		log.Fatal(err)
	}
	results, err := store.Search(ctx, lebro.SimilarityQuery{Index: "documents", Vector: []float32{1, 0, 0}, TopK: 1, Filter: lebro.VectorMetadataFilter{Match: map[string]json.RawMessage{"tenant": json.RawMessage(`"acme"`)}}})
	if err != nil {
		log.Fatal(err)
	}
	for _, result := range results {
		fmt.Printf("%s %.3f %s\n", result.ID, result.Score, result.Content)
	}
}
