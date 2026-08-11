package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

// stubEmbedder is a deterministic EmbeddingModel for tests. It derives each
// vector from the input text so similarity is reproducible without a provider,
// and records every batch it received so batching behavior is observable.
type stubEmbedder struct {
	dimension int
	mu        sync.Mutex
	batches   [][]string
	err       error
	// vectors, when non-nil, is returned verbatim instead of derived vectors.
	// It lets a test simulate a provider that returns the wrong count or
	// dimension.
	vectors [][]float32
}

func newStubEmbedder(dimension int) *stubEmbedder {
	return &stubEmbedder{dimension: dimension}
}

func (e *stubEmbedder) Dimension() int { return e.dimension }

func (e *stubEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.batches = append(e.batches, append([]string(nil), inputs...))
	err := e.err
	fixed := e.vectors
	e.mu.Unlock()

	if err != nil {
		return nil, err
	}
	if fixed != nil {
		return fixed, nil
	}

	vectors := make([][]float32, len(inputs))
	for i, input := range inputs {
		vectors[i] = deriveVector(input, e.dimension)
	}
	return vectors, nil
}

func (e *stubEmbedder) recordedBatches() [][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([][]string(nil), e.batches...)
}

// deriveVector maps text to a stable vector by bucketing bytes into dimensions.
// Identical text yields identical vectors and similar text yields similar
// vectors, which is all a retrieval test needs.
func deriveVector(text string, dimension int) []float32 {
	vector := make([]float32, dimension)
	for i, b := range []byte(strings.ToLower(text)) {
		vector[(int(b)+i)%dimension] += float32(b%7) + 1
	}
	// Guarantee a non-zero magnitude so cosine similarity is defined.
	vector[0] += 1
	return vector
}

func newTestIndexer(t *testing.T, store VectorStore, embedder EmbeddingModel, batchSize int) *Indexer {
	t.Helper()
	chunker, err := NewCharacterChunker(CharacterChunkerConfig{Size: 20})
	if err != nil {
		t.Fatalf("NewCharacterChunker error = %v", err)
	}
	indexer, err := NewIndexer(IndexerConfig{
		Chunker:    chunker,
		Embeddings: embedder,
		Store:      store,
		Index:      "docs",
		BatchSize:  batchSize,
	})
	if err != nil {
		t.Fatalf("NewIndexer error = %v", err)
	}
	return indexer
}

func TestNewIndexerValidation(t *testing.T) {
	chunker, err := NewCharacterChunker(CharacterChunkerConfig{Size: 10})
	if err != nil {
		t.Fatalf("NewCharacterChunker error = %v", err)
	}
	embedder := newStubEmbedder(8)
	store := NewMemoryVectorStore()

	tests := []struct {
		name    string
		config  IndexerConfig
		wantErr string
	}{
		{
			name:   "valid",
			config: IndexerConfig{Chunker: chunker, Embeddings: embedder, Store: store, Index: "docs"},
		},
		{
			name:    "missing chunker",
			config:  IndexerConfig{Embeddings: embedder, Store: store, Index: "docs"},
			wantErr: "chunker is required",
		},
		{
			name:    "missing embeddings",
			config:  IndexerConfig{Chunker: chunker, Store: store, Index: "docs"},
			wantErr: "embedding model is required",
		},
		{
			name:    "missing store",
			config:  IndexerConfig{Chunker: chunker, Embeddings: embedder, Index: "docs"},
			wantErr: "vector store is required",
		},
		{
			name:    "missing index",
			config:  IndexerConfig{Chunker: chunker, Embeddings: embedder, Store: store},
			wantErr: "index name is required",
		},
		{
			name:    "negative batch size",
			config:  IndexerConfig{Chunker: chunker, Embeddings: embedder, Store: store, Index: "docs", BatchSize: -1},
			wantErr: "batch size must not be negative",
		},
		{
			name:    "non-positive dimension",
			config:  IndexerConfig{Chunker: chunker, Embeddings: newStubEmbedder(0), Store: store, Index: "docs"},
			wantErr: "dimension must be positive",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewIndexer(test.config)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("NewIndexer error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("NewIndexer error = nil, want %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), test.wantErr)
			}
		})
	}
}

func TestIndexerEnsureIndexIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()
	indexer := newTestIndexer(t, store, newStubEmbedder(8), 0)

	if err := indexer.EnsureIndex(ctx); err != nil {
		t.Fatalf("first EnsureIndex error = %v", err)
	}
	// Calling it again on every boot must not fail.
	if err := indexer.EnsureIndex(ctx); err != nil {
		t.Fatalf("second EnsureIndex error = %v", err)
	}
	if got := indexer.Index(); got != "docs" {
		t.Fatalf("Index() = %q, want %q", got, "docs")
	}
}

func TestIndexerIngestRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()
	embedder := newStubEmbedder(8)
	indexer := newTestIndexer(t, store, embedder, 0)

	if err := indexer.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex error = %v", err)
	}

	result, err := indexer.Ingest(ctx, Document{
		ID:       "doc-1",
		Content:  strings.Repeat("a", 45),
		Source:   "handbook.md",
		Metadata: json.RawMessage(`{"team":"platform"}`),
	})
	if err != nil {
		t.Fatalf("Ingest error = %v", err)
	}

	// 45 runes at a window of 20 yields 3 chunks.
	if result.Chunks != 3 {
		t.Fatalf("Chunks = %d, want 3", result.Chunks)
	}
	if result.DocumentID != "doc-1" {
		t.Fatalf("DocumentID = %q, want %q", result.DocumentID, "doc-1")
	}
	wantIDs := []string{"doc-1#0", "doc-1#1", "doc-1#2"}
	if len(result.ChunkIDs) != len(wantIDs) {
		t.Fatalf("ChunkIDs = %v, want %v", result.ChunkIDs, wantIDs)
	}
	for i, want := range wantIDs {
		if result.ChunkIDs[i] != want {
			t.Fatalf("ChunkIDs[%d] = %q, want %q", i, result.ChunkIDs[i], want)
		}
	}

	// The records must be searchable and carry decoded provenance.
	hits, err := store.Search(ctx, SimilarityQuery{
		Vector: deriveVector(strings.Repeat("a", 20), 8),
		Index:  "docs",
		TopK:   10,
	})
	if err != nil {
		t.Fatalf("Search error = %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("len(hits) = %d, want 3", len(hits))
	}
	for _, hit := range hits {
		chunk, err := chunkFromMetadata(hit)
		if err != nil {
			t.Fatalf("chunkFromMetadata error = %v", err)
		}
		if chunk.DocumentID != "doc-1" {
			t.Fatalf("DocumentID = %q, want %q", chunk.DocumentID, "doc-1")
		}
		if chunk.Source != "handbook.md" {
			t.Fatalf("Source = %q, want %q", chunk.Source, "handbook.md")
		}
		if chunk.Content == "" {
			t.Fatal("Content is empty, want the chunk text stored on the record")
		}
	}
}

// TestIndexerIngestIsIdempotent is the re-ingestion guarantee: stable chunk IDs
// mean re-indexing replaces rather than duplicates.
func TestIndexerIngestIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()
	indexer := newTestIndexer(t, store, newStubEmbedder(8), 0)
	if err := indexer.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex error = %v", err)
	}

	document := Document{ID: "doc-1", Content: strings.Repeat("b", 45)}
	first, err := indexer.Ingest(ctx, document)
	if err != nil {
		t.Fatalf("first Ingest error = %v", err)
	}
	second, err := indexer.Ingest(ctx, document)
	if err != nil {
		t.Fatalf("second Ingest error = %v", err)
	}
	if first.Chunks != second.Chunks {
		t.Fatalf("chunk counts differ: %d then %d", first.Chunks, second.Chunks)
	}

	hits, err := store.Search(ctx, SimilarityQuery{Vector: deriveVector("b", 8), Index: "docs", TopK: 100})
	if err != nil {
		t.Fatalf("Search error = %v", err)
	}
	if len(hits) != first.Chunks {
		t.Fatalf("len(hits) = %d after re-ingestion, want %d", len(hits), first.Chunks)
	}
}

func TestIndexerBatchesEmbeddings(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()
	embedder := newStubEmbedder(8)
	indexer := newTestIndexer(t, store, embedder, 2)
	if err := indexer.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex error = %v", err)
	}

	// 100 runes at a window of 20 yields 5 chunks, batched 2 + 2 + 1.
	if _, err := indexer.Ingest(ctx, Document{ID: "doc-1", Content: strings.Repeat("c", 100)}); err != nil {
		t.Fatalf("Ingest error = %v", err)
	}

	batches := embedder.recordedBatches()
	wantSizes := []int{2, 2, 1}
	if len(batches) != len(wantSizes) {
		t.Fatalf("len(batches) = %d, want %d", len(batches), len(wantSizes))
	}
	for i, want := range wantSizes {
		if len(batches[i]) != want {
			t.Fatalf("batches[%d] size = %d, want %d", i, len(batches[i]), want)
		}
	}
}

func TestIndexerDefaultBatchSize(t *testing.T) {
	store := NewMemoryVectorStore()
	indexer := newTestIndexer(t, store, newStubEmbedder(8), 0)
	if indexer.batchSize != DefaultEmbeddingBatchSize {
		t.Fatalf("batchSize = %d, want %d", indexer.batchSize, DefaultEmbeddingBatchSize)
	}
}

func TestIndexerIngestRejectsInvalidDocument(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()
	indexer := newTestIndexer(t, store, newStubEmbedder(8), 0)
	if err := indexer.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex error = %v", err)
	}

	if _, err := indexer.Ingest(ctx, Document{Content: "no id"}); !errors.Is(err, ErrRAGInvalidDocument) {
		t.Fatalf("Ingest error = %v, want ErrRAGInvalidDocument", err)
	}
}

func TestIndexerIngestEmbeddingFailure(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()
	embedder := newStubEmbedder(8)
	providerErr := errors.New("provider unavailable")
	embedder.err = providerErr
	indexer := newTestIndexer(t, store, embedder, 0)
	if err := indexer.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex error = %v", err)
	}

	_, err := indexer.Ingest(ctx, Document{ID: "doc-1", Content: "hello"})
	if !errors.Is(err, ErrRAGEmbedding) {
		t.Fatalf("Ingest error = %v, want ErrRAGEmbedding", err)
	}
	// The provider's own error must stay reachable so callers can apply a retry
	// policy to it.
	if !errors.Is(err, providerErr) {
		t.Fatalf("Ingest error = %v, want the provider error preserved", err)
	}
}

func TestIndexerIngestRejectsWrongVectorCount(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()
	embedder := newStubEmbedder(8)
	// One vector for what will be two chunks.
	embedder.vectors = [][]float32{deriveVector("x", 8)}
	indexer := newTestIndexer(t, store, embedder, 0)
	if err := indexer.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex error = %v", err)
	}

	_, err := indexer.Ingest(ctx, Document{ID: "doc-1", Content: strings.Repeat("d", 30)})
	if !errors.Is(err, ErrRAGEmbedding) {
		t.Fatalf("Ingest error = %v, want ErrRAGEmbedding", err)
	}
	if !strings.Contains(err.Error(), "vectors for") {
		t.Fatalf("error = %q, want it to report the count mismatch", err.Error())
	}
}

func TestIndexerIngestRejectsWrongDimension(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()
	embedder := newStubEmbedder(8)
	// Right count, wrong width.
	embedder.vectors = [][]float32{{1, 2, 3}}
	indexer := newTestIndexer(t, store, embedder, 0)
	if err := indexer.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex error = %v", err)
	}

	_, err := indexer.Ingest(ctx, Document{ID: "doc-1", Content: "short"})
	if !errors.Is(err, ErrRAGEmbedding) {
		t.Fatalf("Ingest error = %v, want ErrRAGEmbedding", err)
	}
	if !strings.Contains(err.Error(), "dimension") {
		t.Fatalf("error = %q, want it to report the dimension mismatch", err.Error())
	}
}

func TestIndexerIngestIndexingFailure(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()
	indexer := newTestIndexer(t, store, newStubEmbedder(8), 0)

	// EnsureIndex deliberately not called, so the upsert hits a missing index.
	_, err := indexer.Ingest(ctx, Document{ID: "doc-1", Content: "hello"})
	if !errors.Is(err, ErrRAGIndexing) {
		t.Fatalf("Ingest error = %v, want ErrRAGIndexing", err)
	}
	if !errors.Is(err, ErrVectorNotFound) {
		t.Fatalf("Ingest error = %v, want the ErrVector sentinel preserved", err)
	}
}

func TestIndexerIngestCanceledContext(t *testing.T) {
	store := NewMemoryVectorStore()
	indexer := newTestIndexer(t, store, newStubEmbedder(8), 0)
	if err := indexer.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := indexer.Ingest(ctx, Document{ID: "doc-1", Content: "hello"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Ingest error = %v, want context.Canceled", err)
	}
}

func TestIndexerDelete(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()
	indexer := newTestIndexer(t, store, newStubEmbedder(8), 0)
	if err := indexer.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex error = %v", err)
	}

	result, err := indexer.Ingest(ctx, Document{ID: "doc-1", Content: strings.Repeat("e", 45)})
	if err != nil {
		t.Fatalf("Ingest error = %v", err)
	}

	if err := indexer.Delete(ctx, result.ChunkIDs); err != nil {
		t.Fatalf("Delete error = %v", err)
	}

	hits, err := store.Search(ctx, SimilarityQuery{Vector: deriveVector("e", 8), Index: "docs", TopK: 10})
	if err != nil {
		t.Fatalf("Search error = %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("len(hits) = %d after delete, want 0", len(hits))
	}

	// An empty list is a no-op, not an error.
	if err := indexer.Delete(ctx, nil); err != nil {
		t.Fatalf("Delete(nil) error = %v", err)
	}
}

func TestIndexerNilReceiver(t *testing.T) {
	var indexer *Indexer
	if _, err := indexer.Ingest(context.Background(), Document{ID: "d", Content: "c"}); err == nil {
		t.Fatal("Ingest on nil indexer error = nil, want an error")
	}
	if err := indexer.EnsureIndex(context.Background()); err == nil {
		t.Fatal("EnsureIndex on nil indexer error = nil, want an error")
	}
	if err := indexer.Delete(context.Background(), []string{"x"}); err == nil {
		t.Fatal("Delete on nil indexer error = nil, want an error")
	}
	if got := indexer.Index(); got != "" {
		t.Fatalf("Index() on nil indexer = %q, want empty", got)
	}
}

// TestIndexerRejectsChunkerReturningNoChunks covers a custom Chunker that
// reports success with an empty slice: silently indexing nothing must not look
// like a successful ingestion.
func TestIndexerRejectsChunkerReturningNoChunks(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()
	indexer, err := NewIndexer(IndexerConfig{
		Chunker:    emptyChunker{},
		Embeddings: newStubEmbedder(8),
		Store:      store,
		Index:      "docs",
	})
	if err != nil {
		t.Fatalf("NewIndexer error = %v", err)
	}
	if err := indexer.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex error = %v", err)
	}

	if _, err := indexer.Ingest(ctx, Document{ID: "doc-1", Content: "hello"}); !errors.Is(err, ErrRAGChunking) {
		t.Fatalf("Ingest error = %v, want ErrRAGChunking", err)
	}
}

type emptyChunker struct{}

func (emptyChunker) Chunk(context.Context, Document) ([]Chunk, error) { return nil, nil }

// TestIndexerRejectsInvalidChunks covers a custom Chunker that emits a
// structurally invalid chunk.
func TestIndexerRejectsInvalidChunks(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()
	indexer, err := NewIndexer(IndexerConfig{
		Chunker:    invalidChunker{},
		Embeddings: newStubEmbedder(8),
		Store:      store,
		Index:      "docs",
	})
	if err != nil {
		t.Fatalf("NewIndexer error = %v", err)
	}
	if err := indexer.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex error = %v", err)
	}

	if _, err := indexer.Ingest(ctx, Document{ID: "doc-1", Content: "hello"}); !errors.Is(err, ErrRAGChunking) {
		t.Fatalf("Ingest error = %v, want ErrRAGChunking", err)
	}
}

type invalidChunker struct{}

func (invalidChunker) Chunk(_ context.Context, document Document) ([]Chunk, error) {
	// Missing ID.
	return []Chunk{{DocumentID: document.ID, Content: "x"}}, nil
}

// TestIndexerNormalizesUntypedChunkerError covers a Chunker that fails with a
// plain error: the indexer must classify it as a chunking failure.
func TestIndexerNormalizesUntypedChunkerError(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()
	indexer, err := NewIndexer(IndexerConfig{
		Chunker:    failingChunker{err: errors.New("tokenizer exploded")},
		Embeddings: newStubEmbedder(8),
		Store:      store,
		Index:      "docs",
	})
	if err != nil {
		t.Fatalf("NewIndexer error = %v", err)
	}
	if err := indexer.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex error = %v", err)
	}

	_, err = indexer.Ingest(ctx, Document{ID: "doc-1", Content: "hello"})
	if !errors.Is(err, ErrRAGChunking) {
		t.Fatalf("Ingest error = %v, want ErrRAGChunking", err)
	}
	if !strings.Contains(err.Error(), "tokenizer exploded") {
		t.Fatalf("error = %q, want the chunker cause preserved", err.Error())
	}
}

type failingChunker struct{ err error }

func (c failingChunker) Chunk(context.Context, Document) ([]Chunk, error) { return nil, c.err }

// bufferReusingEmbedder returns the same backing slice on every call, refilled
// with the call number. Real providers are free to do this, and the indexer must
// not depend on the slice staying valid after Embed returns.
type bufferReusingEmbedder struct {
	dimension int
	buffer    []float32
	calls     int
}

func (e *bufferReusingEmbedder) Dimension() int { return e.dimension }

func (e *bufferReusingEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	e.calls++
	if e.buffer == nil {
		e.buffer = make([]float32, e.dimension)
	}
	for i := range e.buffer {
		e.buffer[i] = float32(e.calls)
	}
	vectors := make([][]float32, len(inputs))
	for i := range inputs {
		vectors[i] = e.buffer
	}
	return vectors, nil
}

// TestIndexerCopiesProviderVectors guards against aliasing across batches: the
// records accumulate until a single upsert at the end, so retaining the
// provider's slice would let a later batch rewrite earlier vectors.
func TestIndexerCopiesProviderVectors(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVectorStore()
	embedder := &bufferReusingEmbedder{dimension: 8}

	chunker, err := NewCharacterChunker(CharacterChunkerConfig{Size: 10})
	if err != nil {
		t.Fatalf("NewCharacterChunker error = %v", err)
	}
	// One chunk per batch, so each chunk sees a different buffer fill.
	indexer, err := NewIndexer(IndexerConfig{
		Chunker:    chunker,
		Embeddings: embedder,
		Store:      store,
		Index:      "docs",
		BatchSize:  1,
	})
	if err != nil {
		t.Fatalf("NewIndexer error = %v", err)
	}
	if err := indexer.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex error = %v", err)
	}

	// 30 runes at a window of 10 yields 3 chunks, hence 3 Embed calls.
	if _, err := indexer.Ingest(ctx, Document{ID: "doc-1", Content: strings.Repeat("a", 30)}); err != nil {
		t.Fatalf("Ingest error = %v", err)
	}
	if embedder.calls != 3 {
		t.Fatalf("Embed calls = %d, want 3", embedder.calls)
	}

	// Each chunk must retain the vector from its own batch. Without a copy all
	// three would hold the final call's values.
	for chunkIndex, want := range []float32{1, 2, 3} {
		id := ChunkID("doc-1", chunkIndex)
		hits, err := store.Search(ctx, SimilarityQuery{
			Vector: []float32{want, want, want, want, want, want, want, want},
			Index:  "docs",
			TopK:   10,
		})
		if err != nil {
			t.Fatalf("Search error = %v", err)
		}
		var found bool
		for _, hit := range hits {
			if hit.ID == id {
				found = true
				// A cosine score of 1 means the stored vector is parallel to the
				// batch fill it should have kept.
				if hit.Score < 0.999 {
					t.Fatalf("chunk %s score against its own batch vector = %f, want ~1", id, hit.Score)
				}
			}
		}
		if !found {
			t.Fatalf("chunk %s not found in the index", id)
		}
	}
}
