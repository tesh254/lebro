package testkit

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/tesh254/lebro/internal/runtime"
)

// VectorStoreFactory builds the vector adapter under contract scrutiny. It
// must return a store whose schema is ready for records (adapters with
// migrations should run Migrate themselves or arrange for the suite to do it).
type VectorStoreFactory func(*testing.T) runtime.VectorStore

// VectorContractSuite runs the adapter-neutral vector-store behaviors that
// every VectorStore implementation — in-memory or database-backed — must
// satisfy: index lifecycle, upsert round-trips, dimension validation, delete
// semantics, similarity search ordering, metadata filtering, score
// thresholds, defensive copies, invalid input, and context cancellation.
func VectorContractSuite(t *testing.T, newStore VectorStoreFactory) {
	t.Helper()
	t.Run("index lifecycle", func(t *testing.T) { vectorContractIndexLifecycle(t, newStore) })
	t.Run("upsert and search round-trip", func(t *testing.T) { vectorContractUpsertSearch(t, newStore) })
	t.Run("dimension validation", func(t *testing.T) { vectorContractDimension(t, newStore) })
	t.Run("delete semantics", func(t *testing.T) { vectorContractDelete(t, newStore) })
	t.Run("similarity ordering", func(t *testing.T) { vectorContractSimilarityOrdering(t, newStore) })
	t.Run("metadata filter", func(t *testing.T) { vectorContractMetadataFilter(t, newStore) })
	t.Run("min score threshold", func(t *testing.T) { vectorContractMinScore(t, newStore) })
	t.Run("upsert replace", func(t *testing.T) { vectorContractUpsertReplace(t, newStore) })
	t.Run("defensive copies", func(t *testing.T) { vectorContractDefensiveCopies(t, newStore) })
	t.Run("invalid input", func(t *testing.T) { vectorContractInvalidInput(t, newStore) })
	t.Run("canceled context", func(t *testing.T) { vectorContractCanceledContext(t, newStore) })
	t.Run("empty index search", func(t *testing.T) { vectorContractEmptySearch(t, newStore) })
}

func vectorContractIndexLifecycle(t *testing.T, newStore VectorStoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)

	if err := store.CreateIndex(ctx, "", 128); !errors.Is(err, runtime.ErrVectorInvalidInput) {
		t.Fatalf("CreateIndex empty name error = %v, want ErrVectorInvalidInput", err)
	}
	if err := store.CreateIndex(ctx, "docs", 0); !errors.Is(err, runtime.ErrVectorInvalidInput) {
		t.Fatalf("CreateIndex zero dimension error = %v, want ErrVectorInvalidInput", err)
	}
	if err := store.CreateIndex(ctx, "docs", 128); err != nil {
		t.Fatalf("CreateIndex error = %v", err)
	}
	if err := store.CreateIndex(ctx, "docs", 128); !errors.Is(err, runtime.ErrVectorAlreadyExists) {
		t.Fatalf("CreateIndex duplicate error = %v, want ErrVectorAlreadyExists", err)
	}
	if err := store.DeleteIndex(ctx, "missing"); !errors.Is(err, runtime.ErrVectorNotFound) {
		t.Fatalf("DeleteIndex missing error = %v, want ErrVectorNotFound", err)
	}
	if err := store.DeleteIndex(ctx, "docs"); err != nil {
		t.Fatalf("DeleteIndex error = %v", err)
	}
	if err := store.DeleteIndex(ctx, "docs"); !errors.Is(err, runtime.ErrVectorNotFound) {
		t.Fatalf("DeleteIndex after delete error = %v, want ErrVectorNotFound", err)
	}
}

func vectorContractUpsertSearch(t *testing.T, newStore VectorStoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)

	if err := store.CreateIndex(ctx, "docs", 3); err != nil {
		t.Fatal(err)
	}
	records := []runtime.EmbeddingRecord{
		{ID: "r1", Index: "docs", Vector: []float32{1, 0, 0}, Metadata: json.RawMessage(`{"cat":"a"}`), Content: "hello"},
		{ID: "r2", Index: "docs", Vector: []float32{0, 1, 0}, Metadata: json.RawMessage(`{"cat":"b"}`), Content: "world"},
		{ID: "r3", Index: "docs", Vector: []float32{1, 1, 0}, Metadata: json.RawMessage(`{"cat":"a"}`), Content: "foo"},
	}
	if err := store.Upsert(ctx, records); err != nil {
		t.Fatal(err)
	}

	results, err := store.Search(ctx, runtime.SimilarityQuery{
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
	if results[0].Content != "hello" {
		t.Fatalf("r1 content = %q, want hello", results[0].Content)
	}
	if string(results[0].Metadata) == "" {
		t.Fatalf("r1 metadata empty, want original")
	}
}

func vectorContractDimension(t *testing.T, newStore VectorStoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)

	if err := store.CreateIndex(ctx, "docs", 3); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, []runtime.EmbeddingRecord{{ID: "r1", Index: "docs", Vector: []float32{1, 0}}}); !errors.Is(err, runtime.ErrVectorInvalidDimension) {
		t.Fatalf("Upsert dimension mismatch error = %v, want ErrVectorInvalidDimension", err)
	}
	if _, err := store.Search(ctx, runtime.SimilarityQuery{Vector: []float32{1, 0}, Index: "docs", TopK: 1}); !errors.Is(err, runtime.ErrVectorInvalidDimension) {
		t.Fatalf("Search dimension mismatch error = %v, want ErrVectorInvalidDimension", err)
	}
}

func vectorContractDelete(t *testing.T, newStore VectorStoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)

	if err := store.CreateIndex(ctx, "docs", 2); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, []runtime.EmbeddingRecord{
		{ID: "r1", Index: "docs", Vector: []float32{1, 0}},
		{ID: "r2", Index: "docs", Vector: []float32{0, 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "docs", []string{"r1", "nonexistent"}); err != nil {
		t.Fatal(err)
	}
	results, err := store.Search(ctx, runtime.SimilarityQuery{Vector: []float32{1, 0}, Index: "docs", TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ID != "r2" {
		t.Fatalf("after delete results = %#v, want only r2", results)
	}
	if err := store.Delete(ctx, "missing", []string{"r1"}); !errors.Is(err, runtime.ErrVectorNotFound) {
		t.Fatalf("Delete missing index error = %v, want ErrVectorNotFound", err)
	}
}

func vectorContractSimilarityOrdering(t *testing.T, newStore VectorStoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)

	if err := store.CreateIndex(ctx, "docs", 2); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, []runtime.EmbeddingRecord{
		{ID: "exact", Index: "docs", Vector: []float32{1, 0}},
		{ID: "close", Index: "docs", Vector: []float32{1, 1}},
		{ID: "orthogonal", Index: "docs", Vector: []float32{0, 1}},
		{ID: "opposite", Index: "docs", Vector: []float32{-1, 0}},
	}); err != nil {
		t.Fatal(err)
	}
	results, err := store.Search(ctx, runtime.SimilarityQuery{
		Vector: []float32{1, 0},
		Index:  "docs",
		TopK:   4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 4 {
		t.Fatalf("results = %d, want 4", len(results))
	}
	if results[0].ID != "exact" || results[1].ID != "close" {
		t.Fatalf("ordering = %s, %s; want exact, close", results[0].ID, results[1].ID)
	}
	if results[0].Score < results[1].Score || results[1].Score < results[2].Score {
		t.Fatalf("scores not descending: %v", results)
	}

	top2, err := store.Search(ctx, runtime.SimilarityQuery{
		Vector: []float32{1, 0},
		Index:  "docs",
		TopK:   2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(top2) != 2 {
		t.Fatalf("TopK=2 results = %d, want 2", len(top2))
	}
}

func vectorContractMetadataFilter(t *testing.T, newStore VectorStoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)

	if err := store.CreateIndex(ctx, "docs", 2); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, []runtime.EmbeddingRecord{
		{ID: "r1", Index: "docs", Vector: []float32{1, 0}, Metadata: json.RawMessage(`{"cat":"a","lang":"en"}`)},
		{ID: "r2", Index: "docs", Vector: []float32{0, 1}, Metadata: json.RawMessage(`{"cat":"b","lang":"en"}`)},
		{ID: "r3", Index: "docs", Vector: []float32{1, 1}, Metadata: json.RawMessage(`{"cat":"a","lang":"fr"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	results, err := store.Search(ctx, runtime.SimilarityQuery{
		Vector: []float32{1, 0},
		Index:  "docs",
		Filter: runtime.VectorMetadataFilter{Match: map[string]json.RawMessage{"cat": json.RawMessage(`"a"`)}},
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

	results2, err := store.Search(ctx, runtime.SimilarityQuery{
		Vector: []float32{1, 0},
		Index:  "docs",
		Filter: runtime.VectorMetadataFilter{Match: map[string]json.RawMessage{"cat": json.RawMessage(`"a"`), "lang": json.RawMessage(`"en"`)}},
		TopK:   5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results2) != 1 || results2[0].ID != "r1" {
		t.Fatalf("multi-filter results = %#v, want only r1", results2)
	}
}

func vectorContractMinScore(t *testing.T, newStore VectorStoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)

	if err := store.CreateIndex(ctx, "docs", 2); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, []runtime.EmbeddingRecord{
		{ID: "r1", Index: "docs", Vector: []float32{1, 0}},
		{ID: "r2", Index: "docs", Vector: []float32{0, 1}},
		{ID: "r3", Index: "docs", Vector: []float32{-1, 0}},
	}); err != nil {
		t.Fatal(err)
	}
	results, err := store.Search(ctx, runtime.SimilarityQuery{
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

func vectorContractUpsertReplace(t *testing.T, newStore VectorStoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)

	if err := store.CreateIndex(ctx, "docs", 2); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, []runtime.EmbeddingRecord{{ID: "r1", Index: "docs", Vector: []float32{1, 0}, Content: "old"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, []runtime.EmbeddingRecord{{ID: "r1", Index: "docs", Vector: []float32{0, 1}, Content: "new"}}); err != nil {
		t.Fatal(err)
	}
	results, err := store.Search(ctx, runtime.SimilarityQuery{Vector: []float32{0, 1}, Index: "docs", TopK: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Content != "new" {
		t.Fatalf("upsert replace result = %#v, want new content", results)
	}
}

func vectorContractDefensiveCopies(t *testing.T, newStore VectorStoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)

	if err := store.CreateIndex(ctx, "docs", 2); err != nil {
		t.Fatal(err)
	}
	vec := []float32{1, 0}
	meta := json.RawMessage(`{"v":1}`)
	if err := store.Upsert(ctx, []runtime.EmbeddingRecord{{ID: "r1", Index: "docs", Vector: vec, Metadata: meta}}); err != nil {
		t.Fatal(err)
	}
	// Mutate the caller's vector direction so a non-defensive copy would
	// change the stored vector and thus the similarity score.
	vec[0] = 0
	vec[1] = 999
	meta[0] = 'x'

	results, err := store.Search(ctx, runtime.SimilarityQuery{Vector: []float32{1, 0}, Index: "docs", TopK: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].Score < 0.99 {
		t.Fatalf("defensive copy failed: score = %.4f, want ~1.0 (caller vector mutation visible in store)", results[0].Score)
	}
	if results[0].Metadata[0] == 'x' {
		t.Fatal("defensive copy failed: caller metadata mutation visible in store")
	}
	results[0].Metadata[0] = 'y'
	again, err := store.Search(ctx, runtime.SimilarityQuery{Vector: []float32{1, 0}, Index: "docs", TopK: 1})
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Metadata[0] == 'y' {
		t.Fatal("defensive copy failed: result metadata mutation visible in store")
	}
}

func vectorContractInvalidInput(t *testing.T, newStore VectorStoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)

	if err := store.CreateIndex(ctx, "docs", 2); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, []runtime.EmbeddingRecord{{ID: "", Index: "docs", Vector: []float32{1, 0}}}); !errors.Is(err, runtime.ErrVectorInvalidInput) {
		t.Fatalf("empty ID error = %v, want ErrVectorInvalidInput", err)
	}
	if err := store.Upsert(ctx, []runtime.EmbeddingRecord{{ID: "r1", Index: "docs", Vector: nil}}); !errors.Is(err, runtime.ErrVectorInvalidInput) {
		t.Fatalf("empty vector error = %v, want ErrVectorInvalidInput", err)
	}
	if err := store.Upsert(ctx, []runtime.EmbeddingRecord{{ID: "r1", Index: "docs", Vector: []float32{1, 0}, Metadata: json.RawMessage(`{`)}}); !errors.Is(err, runtime.ErrVectorInvalidInput) {
		t.Fatalf("invalid metadata error = %v, want ErrVectorInvalidInput", err)
	}
	if _, err := store.Search(ctx, runtime.SimilarityQuery{Vector: []float32{1, 0}, Index: "docs", TopK: 0}); !errors.Is(err, runtime.ErrVectorInvalidInput) {
		t.Fatalf("zero TopK error = %v, want ErrVectorInvalidInput", err)
	}
	if _, err := store.Search(ctx, runtime.SimilarityQuery{Vector: []float32{1, 0}, Index: "", TopK: 1}); !errors.Is(err, runtime.ErrVectorInvalidInput) {
		t.Fatalf("empty index error = %v, want ErrVectorInvalidInput", err)
	}
	if _, err := store.Search(ctx, runtime.SimilarityQuery{Vector: []float32{1, 0}, Index: "docs", TopK: 1, MinScore: -0.1}); !errors.Is(err, runtime.ErrVectorInvalidInput) {
		t.Fatalf("negative MinScore error = %v, want ErrVectorInvalidInput", err)
	}
	if _, err := store.Search(ctx, runtime.SimilarityQuery{Vector: []float32{1, 0}, Index: "docs", TopK: 1, MinScore: 1.1}); !errors.Is(err, runtime.ErrVectorInvalidInput) {
		t.Fatalf("MinScore > 1 error = %v, want ErrVectorInvalidInput", err)
	}
}

func vectorContractCanceledContext(t *testing.T, newStore VectorStoreFactory) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := newStore(t)
	if err := store.CreateIndex(ctx, "docs", 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("CreateIndex canceled error = %v, want context.Canceled", err)
	}
}

func vectorContractEmptySearch(t *testing.T, newStore VectorStoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)

	if err := store.CreateIndex(ctx, "docs", 2); err != nil {
		t.Fatal(err)
	}
	results, err := store.Search(ctx, runtime.SimilarityQuery{Vector: []float32{1, 0}, Index: "docs", TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("empty index search = %d results, want 0", len(results))
	}
}
