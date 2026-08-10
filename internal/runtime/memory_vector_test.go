package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestMemoryVectorCreateDeleteIndex(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()

	if err := store.CreateIndex(ctx, "", 128); !errors.Is(err, ErrVectorInvalidInput) {
		t.Fatalf("CreateIndex empty name error = %v, want ErrVectorInvalidInput", err)
	}
	if err := store.CreateIndex(ctx, "docs", 0); !errors.Is(err, ErrVectorInvalidInput) {
		t.Fatalf("CreateIndex zero dimension error = %v, want ErrVectorInvalidInput", err)
	}
	if err := store.CreateIndex(ctx, "docs", 128); err != nil {
		t.Fatalf("CreateIndex error = %v", err)
	}
	if err := store.CreateIndex(ctx, "docs", 128); !errors.Is(err, ErrVectorAlreadyExists) {
		t.Fatalf("CreateIndex duplicate error = %v, want ErrVectorAlreadyExists", err)
	}
	if err := store.DeleteIndex(ctx, "missing"); !errors.Is(err, ErrVectorNotFound) {
		t.Fatalf("DeleteIndex missing error = %v, want ErrVectorNotFound", err)
	}
	if err := store.DeleteIndex(ctx, "docs"); err != nil {
		t.Fatalf("DeleteIndex error = %v", err)
	}
	if err := store.DeleteIndex(ctx, "docs"); !errors.Is(err, ErrVectorNotFound) {
		t.Fatalf("DeleteIndex after delete error = %v, want ErrVectorNotFound", err)
	}
}

func TestMemoryVectorUpsertAndSearch(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()

	if err := store.CreateIndex(ctx, "docs", 3); err != nil {
		t.Fatal(err)
	}
	records := []EmbeddingRecord{
		{ID: "r1", Index: "docs", Vector: []float32{1, 0, 0}, Metadata: json.RawMessage(`{"cat":"a"}`)},
		{ID: "r2", Index: "docs", Vector: []float32{0, 1, 0}, Metadata: json.RawMessage(`{"cat":"b"}`)},
		{ID: "r3", Index: "docs", Vector: []float32{1, 1, 0}, Metadata: json.RawMessage(`{"cat":"a"}`)},
	}
	if err := store.Upsert(ctx, records); err != nil {
		t.Fatal(err)
	}

	results, err := store.Search(ctx, SimilarityQuery{
		Vector: []float32{1, 0, 0},
		Index:  "docs",
		TopK:   3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 || results[0].ID != "r1" {
		t.Fatalf("Search results = %#v, want r1 first", results)
	}
	if results[0].Score < 0.99 {
		t.Fatalf("r1 score = %f, want ~1.0", results[0].Score)
	}
}

func TestMemoryVectorUpsertDimensionMismatch(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()
	if err := store.CreateIndex(ctx, "docs", 3); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, []EmbeddingRecord{{ID: "r1", Index: "docs", Vector: []float32{1, 0}}}); !errors.Is(err, ErrVectorInvalidDimension) {
		t.Fatalf("Upsert dimension mismatch error = %v, want ErrVectorInvalidDimension", err)
	}
}

func TestMemoryVectorUpsertMissingIndex(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()
	if err := store.Upsert(ctx, []EmbeddingRecord{{ID: "r1", Index: "missing", Vector: []float32{1}}}); !errors.Is(err, ErrVectorNotFound) {
		t.Fatalf("Upsert missing index error = %v, want ErrVectorNotFound", err)
	}
}

func TestMemoryVectorSearchMissingIndex(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()
	if _, err := store.Search(ctx, SimilarityQuery{Vector: []float32{1}, Index: "missing", TopK: 1}); !errors.Is(err, ErrVectorNotFound) {
		t.Fatalf("Search missing index error = %v, want ErrVectorNotFound", err)
	}
}

func TestMemoryVectorMetadataFilter(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()
	if err := store.CreateIndex(ctx, "docs", 2); err != nil {
		t.Fatal(err)
	}
	records := []EmbeddingRecord{
		{ID: "r1", Index: "docs", Vector: []float32{1, 0}, Metadata: json.RawMessage(`{"cat":"a"}`)},
		{ID: "r2", Index: "docs", Vector: []float32{0, 1}, Metadata: json.RawMessage(`{"cat":"b"}`)},
		{ID: "r3", Index: "docs", Vector: []float32{1, 1}, Metadata: json.RawMessage(`{"cat":"a"}`)},
	}
	if err := store.Upsert(ctx, records); err != nil {
		t.Fatal(err)
	}
	results, err := store.Search(ctx, SimilarityQuery{
		Vector: []float32{1, 0},
		Index:  "docs",
		Filter: VectorMetadataFilter{Match: map[string]json.RawMessage{"cat": json.RawMessage(`"a"`)}},
		TopK:   5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("filtered results = %d, want 2", len(results))
	}
	for _, r := range results {
		var meta map[string]string
		if err := json.Unmarshal(r.Metadata, &meta); err != nil {
			t.Fatal(err)
		}
		if meta["cat"] != "a" {
			t.Fatalf("result %s has cat=%q, want a", r.ID, meta["cat"])
		}
	}
}

func TestMemoryVectorMinScore(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()
	if err := store.CreateIndex(ctx, "docs", 2); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, []EmbeddingRecord{
		{ID: "r1", Index: "docs", Vector: []float32{1, 0}},
		{ID: "r2", Index: "docs", Vector: []float32{0, 1}},
		{ID: "r3", Index: "docs", Vector: []float32{-1, 0}},
	}); err != nil {
		t.Fatal(err)
	}
	results, err := store.Search(ctx, SimilarityQuery{
		Vector:   []float32{1, 0},
		Index:    "docs",
		TopK:     5,
		MinScore: 0.5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ID != "r1" {
		t.Fatalf("min_score results = %#v, want only r1", results)
	}
}

func TestMemoryVectorDelete(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()
	if err := store.CreateIndex(ctx, "docs", 2); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, []EmbeddingRecord{
		{ID: "r1", Index: "docs", Vector: []float32{1, 0}},
		{ID: "r2", Index: "docs", Vector: []float32{0, 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "docs", []string{"r1", "nonexistent"}); err != nil {
		t.Fatal(err)
	}
	results, err := store.Search(ctx, SimilarityQuery{Vector: []float32{1, 0}, Index: "docs", TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ID != "r2" {
		t.Fatalf("after delete results = %#v, want only r2", results)
	}
	if err := store.Delete(ctx, "missing", []string{"r1"}); !errors.Is(err, ErrVectorNotFound) {
		t.Fatalf("Delete missing index error = %v, want ErrVectorNotFound", err)
	}
}

func TestMemoryVectorUpsertReplace(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()
	if err := store.CreateIndex(ctx, "docs", 2); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, []EmbeddingRecord{{ID: "r1", Index: "docs", Vector: []float32{1, 0}, Content: "old"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, []EmbeddingRecord{{ID: "r1", Index: "docs", Vector: []float32{0, 1}, Content: "new"}}); err != nil {
		t.Fatal(err)
	}
	results, err := store.Search(ctx, SimilarityQuery{Vector: []float32{0, 1}, Index: "docs", TopK: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Content != "new" {
		t.Fatalf("upsert replace result = %#v, want new content", results)
	}
}

func TestMemoryVectorDefensiveCopy(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()
	if err := store.CreateIndex(ctx, "docs", 2); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, []EmbeddingRecord{{ID: "r1", Index: "docs", Vector: []float32{1, 0}, Metadata: json.RawMessage(`{"v":1}`)}}); err != nil {
		t.Fatal(err)
	}
	vec := []float32{1, 0}
	meta := json.RawMessage(`{"v":1}`)
	if err := store.Upsert(ctx, []EmbeddingRecord{{ID: "r1", Index: "docs", Vector: vec, Metadata: meta}}); err != nil {
		t.Fatal(err)
	}
	vec[0] = 999
	meta[0] = 'x'

	results, err := store.Search(ctx, SimilarityQuery{Vector: []float32{1, 0}, Index: "docs", TopK: 1})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Metadata[0] == 'x' {
		t.Fatal("defensive copy failed: caller mutation visible in store")
	}
	results[0].Metadata[0] = 'y'
	again, err := store.Search(ctx, SimilarityQuery{Vector: []float32{1, 0}, Index: "docs", TopK: 1})
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Metadata[0] == 'y' {
		t.Fatal("defensive copy failed: result mutation visible in store")
	}
}

func TestMemoryVectorInvalidInput(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()
	if err := store.CreateIndex(ctx, "docs", 2); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, []EmbeddingRecord{{ID: "", Index: "docs", Vector: []float32{1, 0}}}); !errors.Is(err, ErrVectorInvalidInput) {
		t.Fatalf("empty ID error = %v, want ErrVectorInvalidInput", err)
	}
	if err := store.Upsert(ctx, []EmbeddingRecord{{ID: "r1", Index: "docs", Vector: nil}}); !errors.Is(err, ErrVectorInvalidInput) {
		t.Fatalf("empty vector error = %v, want ErrVectorInvalidInput", err)
	}
	if err := store.Upsert(ctx, []EmbeddingRecord{{ID: "r1", Index: "docs", Vector: []float32{1, 0}, Metadata: json.RawMessage(`{`)}}); !errors.Is(err, ErrVectorInvalidInput) {
		t.Fatalf("invalid metadata error = %v, want ErrVectorInvalidInput", err)
	}
	if _, err := store.Search(ctx, SimilarityQuery{Vector: []float32{1, 0}, Index: "docs", TopK: 0}); !errors.Is(err, ErrVectorInvalidInput) {
		t.Fatalf("zero TopK error = %v, want ErrVectorInvalidInput", err)
	}
}

func TestMemoryVectorCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := NewMemoryVectorStore()
	if err := store.CreateIndex(ctx, "docs", 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("CreateIndex canceled error = %v, want context.Canceled", err)
	}
}
