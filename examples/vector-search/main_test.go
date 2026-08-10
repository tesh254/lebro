package main

import (
	"encoding/json"
	"testing"

	"github.com/tesh254/lebro"
)

func TestExample(t *testing.T) {
	main()
}

func TestVectorSearchFiltersByMetadata(t *testing.T) {
	ctx := t.Context()
	store := lebro.NewMemoryVectorStore()

	if err := store.CreateIndex(ctx, "documents", 3); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, []lebro.EmbeddingRecord{
		{ID: "doc-1", Index: "documents", Vector: []float32{1, 0, 0}, Metadata: json.RawMessage(`{"source":"api"}`)},
		{ID: "doc-2", Index: "documents", Vector: []float32{0, 1, 0}, Metadata: json.RawMessage(`{"source":"web"}`)},
	}); err != nil {
		t.Fatal(err)
	}

	results, err := store.Search(ctx, lebro.SimilarityQuery{
		Vector: []float32{1, 0, 0},
		Index:  "documents",
		Filter: lebro.VectorMetadataFilter{Match: map[string]json.RawMessage{"source": json.RawMessage(`"api"`)}},
		TopK:   5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ID != "doc-1" {
		t.Fatalf("results = %#v, want only doc-1", results)
	}
}
