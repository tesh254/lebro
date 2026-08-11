package runtime

import (
	"context"
	"errors"
	"fmt"
)

// DefaultEmbeddingBatchSize is the number of chunks embedded per provider call
// when an IndexerConfig leaves BatchSize zero.
const DefaultEmbeddingBatchSize = 64

// IndexerConfig describes an ingestion pipeline. Chunker, Embeddings, Store,
// and Index are required.
type IndexerConfig struct {
	// Chunker splits documents into retrievable spans.
	Chunker Chunker
	// Embeddings converts chunk text into vectors. Its Dimension determines the
	// dimension of the index EnsureIndex creates.
	Embeddings EmbeddingModel
	// Store persists the embeddings. Any VectorStore adapter works; the indexer
	// only uses the interface, so the choice of backend is not an ingestion
	// concern.
	Store VectorStore
	// Index is the vector index name that receives this pipeline's records.
	Index string
	// BatchSize bounds how many chunks are embedded per provider call. A zero
	// value uses DefaultEmbeddingBatchSize.
	BatchSize int
}

// IndexResult reports what an ingestion produced.
type IndexResult struct {
	// DocumentID identifies the ingested document.
	DocumentID string
	// Chunks is the number of chunks indexed.
	Chunks int
	// ChunkIDs are the stable IDs of the indexed chunks, in document order.
	// Because chunk IDs are stable across re-ingestion, these are also the IDs
	// to delete when removing the document from the index.
	ChunkIDs []string
}

// Indexer runs the ingestion pipeline: chunk, embed, upsert. It owns no state
// beyond its configuration and is safe for concurrent use if its collaborators
// are.
//
// Ingestion is idempotent for an unchanged document: chunk IDs are derived from
// the document ID and ordinal position, so re-indexing replaces records by ID.
// A document that shrinks leaves its surplus trailing chunks behind, so a
// caller that re-ingests changed documents should delete the previous
// IndexResult.ChunkIDs that no longer appear.
//
// The zero value is not usable; construct one with NewIndexer.
type Indexer struct {
	chunker    Chunker
	embeddings EmbeddingModel
	store      VectorStore
	index      string
	batchSize  int
}

// NewIndexer validates the configuration and returns an ingestion pipeline.
func NewIndexer(config IndexerConfig) (*Indexer, error) {
	if config.Chunker == nil || isNilInterface(config.Chunker) {
		return nil, errors.New("lebro: indexer chunker is required")
	}
	if config.Embeddings == nil || isNilInterface(config.Embeddings) {
		return nil, errors.New("lebro: indexer embedding model is required")
	}
	if config.Store == nil || isNilInterface(config.Store) {
		return nil, errors.New("lebro: indexer vector store is required")
	}
	if config.Index == "" {
		return nil, errors.New("lebro: indexer index name is required")
	}
	if config.BatchSize < 0 {
		return nil, errors.New("lebro: indexer batch size must not be negative")
	}
	if dimension := config.Embeddings.Dimension(); dimension <= 0 {
		return nil, fmt.Errorf("lebro: embedding model dimension must be positive, got %d", dimension)
	}

	batchSize := config.BatchSize
	if batchSize == 0 {
		batchSize = DefaultEmbeddingBatchSize
	}
	return &Indexer{
		chunker:    config.Chunker,
		embeddings: config.Embeddings,
		store:      config.Store,
		index:      config.Index,
		batchSize:  batchSize,
	}, nil
}

// Index returns the vector index name this pipeline writes to.
func (i *Indexer) Index() string {
	if i == nil {
		return ""
	}
	return i.index
}

// EnsureIndex creates the pipeline's vector index at the embedding model's
// dimension. An index that already exists is left alone, so calling this at
// startup is safe on every boot.
//
// It does not verify that an existing index's dimension matches the embedding
// model: adapters report a mismatch on upsert via ErrVectorInvalidDimension,
// and VectorStore exposes no dimension read.
func (i *Indexer) EnsureIndex(ctx context.Context) error {
	if i == nil {
		return errors.New("lebro: indexer is nil")
	}
	if ctx == nil {
		return errors.New("lebro: indexer context is nil")
	}
	err := i.store.CreateIndex(ctx, i.index, i.embeddings.Dimension())
	if err == nil || errors.Is(err, ErrVectorAlreadyExists) {
		return nil
	}
	return &RAGError{Kind: RAGErrorIndexing, Err: err}
}

// Ingest chunks, embeds, and upserts a document, returning what was indexed.
//
// Failures are returned as *RAGError naming the stage that failed, with the
// underlying chunker, provider, or store error preserved for errors.As. The
// upsert is a single call per document, so adapters that write atomically leave
// no partial document behind on failure.
func (i *Indexer) Ingest(ctx context.Context, document Document) (IndexResult, error) {
	if i == nil {
		return IndexResult{}, errors.New("lebro: indexer is nil")
	}
	if ctx == nil {
		return IndexResult{}, errors.New("lebro: indexer context is nil")
	}
	if err := ctx.Err(); err != nil {
		return IndexResult{}, err
	}
	if err := document.Validate(); err != nil {
		return IndexResult{}, &RAGError{Kind: RAGErrorInvalidDocument, DocumentID: document.ID, Err: err}
	}

	chunks, err := i.chunker.Chunk(ctx, document)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return IndexResult{}, ctxErr
		}
		// A chunker that already reported a typed stage failure keeps its own
		// classification; anything else is normalized as a chunking failure.
		var ragErr *RAGError
		if errors.As(err, &ragErr) {
			return IndexResult{}, err
		}
		return IndexResult{}, &RAGError{Kind: RAGErrorChunking, DocumentID: document.ID, Err: err}
	}
	if len(chunks) == 0 {
		return IndexResult{}, &RAGError{
			Kind:       RAGErrorChunking,
			DocumentID: document.ID,
			Err:        errors.New("lebro: chunker returned no chunks"),
		}
	}
	for _, chunk := range chunks {
		if err := chunk.Validate(); err != nil {
			return IndexResult{}, &RAGError{Kind: RAGErrorChunking, DocumentID: document.ID, Err: err}
		}
	}

	records := make([]EmbeddingRecord, 0, len(chunks))
	chunkIDs := make([]string, 0, len(chunks))
	dimension := i.embeddings.Dimension()

	for start := 0; start < len(chunks); start += i.batchSize {
		end := start + i.batchSize
		if end > len(chunks) {
			end = len(chunks)
		}
		batch := chunks[start:end]

		inputs := make([]string, len(batch))
		for offset, chunk := range batch {
			inputs[offset] = chunk.Content
		}

		vectors, err := i.embeddings.Embed(ctx, inputs)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return IndexResult{}, ctxErr
			}
			return IndexResult{}, &RAGError{Kind: RAGErrorEmbedding, DocumentID: document.ID, Err: err}
		}
		if len(vectors) != len(inputs) {
			return IndexResult{}, &RAGError{
				Kind:       RAGErrorEmbedding,
				DocumentID: document.ID,
				Err:        fmt.Errorf("lebro: embedding model returned %d vectors for %d inputs", len(vectors), len(inputs)),
			}
		}

		for offset, chunk := range batch {
			vector := vectors[offset]
			if len(vector) != dimension {
				return IndexResult{}, &RAGError{
					Kind:       RAGErrorEmbedding,
					DocumentID: document.ID,
					Err:        fmt.Errorf("lebro: embedding for chunk %q has dimension %d, want %d", chunk.ID, len(vector), dimension),
				}
			}
			metadata, err := chunkMetadata(chunk)
			if err != nil {
				return IndexResult{}, &RAGError{Kind: RAGErrorInvalidDocument, DocumentID: document.ID, Err: err}
			}
			records = append(records, EmbeddingRecord{
				ID:    chunk.ID,
				Index: i.index,
				// Copy the provider's slice: records accumulate across batches
				// and are upserted once at the end, so an EmbeddingModel that
				// reuses its buffer between calls would otherwise rewrite the
				// vectors of every chunk collected so far.
				Vector:    append([]float32(nil), vector...),
				Dimension: dimension,
				Metadata:  metadata,
				Content:   chunk.Content,
			})
			chunkIDs = append(chunkIDs, chunk.ID)
		}
	}

	if err := i.store.Upsert(ctx, records); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return IndexResult{}, ctxErr
		}
		return IndexResult{}, &RAGError{Kind: RAGErrorIndexing, DocumentID: document.ID, Err: err}
	}

	return IndexResult{DocumentID: document.ID, Chunks: len(records), ChunkIDs: chunkIDs}, nil
}

// Delete removes chunk records from the pipeline's index by chunk ID. Pass the
// ChunkIDs from a prior IndexResult to remove a document. Missing IDs are
// ignored, matching VectorStore.Delete.
func (i *Indexer) Delete(ctx context.Context, chunkIDs []string) error {
	if i == nil {
		return errors.New("lebro: indexer is nil")
	}
	if ctx == nil {
		return errors.New("lebro: indexer context is nil")
	}
	if len(chunkIDs) == 0 {
		return nil
	}
	if err := i.store.Delete(ctx, i.index, chunkIDs); err != nil {
		return &RAGError{Kind: RAGErrorIndexing, Err: err}
	}
	return nil
}
