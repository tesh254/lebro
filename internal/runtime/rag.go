package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Reserved chunk metadata keys. An Indexer writes these onto every embedding
// record's metadata so a retrieval hit carries stable provenance without a
// second lookup, and a VectorRetriever reads them back to reconstruct a Chunk.
//
// They are reserved: a document whose own metadata uses one of these keys is
// rejected by Document.Validate rather than silently overwritten, so the
// provenance a retrieval result reports is always the provenance the indexer
// recorded.
const (
	ChunkMetadataDocumentID = "document_id"
	ChunkMetadataSource     = "source"
	ChunkMetadataChunkIndex = "chunk_index"
)

// reservedChunkMetadataKeys lists the keys an application may not set on a
// document. Kept in one place so Document.Validate and the indexer cannot
// disagree about what is reserved.
var reservedChunkMetadataKeys = []string{
	ChunkMetadataDocumentID,
	ChunkMetadataSource,
	ChunkMetadataChunkIndex,
}

// RAGErrorKind identifies the normalized category of a retrieval-pipeline
// failure. The stages are distinguished because they fail for different
// reasons and callers react differently: a chunking failure is a defect in the
// document or the chunker configuration, while an embedding failure is usually
// a provider problem worth retrying.
type RAGErrorKind string

const (
	// RAGErrorInvalidDocument means a document did not satisfy the ingestion
	// contract — empty ID or content, invalid metadata JSON, or metadata that
	// collides with a reserved chunk metadata key.
	RAGErrorInvalidDocument RAGErrorKind = "rag_invalid_document"
	// RAGErrorChunking means a Chunker rejected a document or returned a
	// structurally invalid chunk.
	RAGErrorChunking RAGErrorKind = "rag_chunking"
	// RAGErrorEmbedding means an EmbeddingModel failed, or returned a vector
	// count or dimension that does not match what was requested. The wrapped
	// error preserves the adapter's *ModelError where there was one.
	RAGErrorEmbedding RAGErrorKind = "rag_embedding"
	// RAGErrorIndexing means the vector store rejected the embedding records.
	// The wrapped error preserves the ErrVector* sentinel.
	RAGErrorIndexing RAGErrorKind = "rag_indexing"
	// RAGErrorRetrieval means a retrieval query failed — an invalid query, a
	// failed search, or a hit whose metadata could not be decoded.
	RAGErrorRetrieval RAGErrorKind = "rag_retrieval"
	// RAGErrorGraphTraversal means a graph store rejected or failed a bounded
	// traversal request.
	RAGErrorGraphTraversal RAGErrorKind = "rag_graph_traversal"
	// RAGErrorReranking means a reranker or its relevance-scoring adapter
	// failed after vector retrieval returned its bounded candidate pool.
	RAGErrorReranking RAGErrorKind = "rag_reranking"
)

// Retrieval-pipeline errors. Adapters and pipeline stages return these via
// errors.Is so callers can branch on the failing stage without parsing text.
var (
	// ErrRAGInvalidDocument matches documents that fail the ingestion contract.
	ErrRAGInvalidDocument = errors.New("lebro: invalid RAG document")
	// ErrRAGChunking matches chunking failures.
	ErrRAGChunking = errors.New("lebro: RAG chunking failed")
	// ErrRAGEmbedding matches embedding failures.
	ErrRAGEmbedding = errors.New("lebro: RAG embedding failed")
	// ErrRAGIndexing matches vector-store failures during indexing.
	ErrRAGIndexing = errors.New("lebro: RAG indexing failed")
	// ErrRAGRetrieval matches retrieval failures.
	ErrRAGRetrieval = errors.New("lebro: RAG retrieval failed")
	// ErrRAGGraphTraversal matches graph traversal failures.
	ErrRAGGraphTraversal = errors.New("lebro: RAG graph traversal failed")
	// ErrRAGReranking matches reranker and relevance-scoring failures.
	ErrRAGReranking = errors.New("lebro: RAG reranking failed")
)

// RAGError preserves the failing stage and cause of a retrieval-pipeline
// failure. DocumentID is set when the failure is attributable to one document.
type RAGError struct {
	Kind       RAGErrorKind
	DocumentID string
	Err        error
}

func (e *RAGError) Error() string {
	if e == nil {
		return "lebro: RAG failure"
	}
	kind := e.Kind
	if kind == "" {
		kind = RAGErrorRetrieval
	}
	subject := ""
	if e.DocumentID != "" {
		subject = fmt.Sprintf(" for document %q", e.DocumentID)
	}
	if e.Err == nil {
		return fmt.Sprintf("lebro: %s%s", kind, subject)
	}
	return fmt.Sprintf("lebro: %s%s: %s", kind, subject, e.Err.Error())
}

// Unwrap exposes the underlying chunker, provider, or store error so callers
// can inspect a *ModelError or an ErrVector* sentinel behind a stage failure.
func (e *RAGError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Is supports errors.Is checks against the normalized ErrRAG sentinels while
// Unwrap continues to preserve the original cause.
func (e *RAGError) Is(target error) bool {
	if e == nil {
		return false
	}
	return target == ragErrorSentinel(e.Kind)
}

func ragErrorSentinel(kind RAGErrorKind) error {
	switch kind {
	case RAGErrorInvalidDocument:
		return ErrRAGInvalidDocument
	case RAGErrorChunking:
		return ErrRAGChunking
	case RAGErrorEmbedding:
		return ErrRAGEmbedding
	case RAGErrorIndexing:
		return ErrRAGIndexing
	case RAGErrorRetrieval:
		return ErrRAGRetrieval
	case RAGErrorGraphTraversal:
		return ErrRAGGraphTraversal
	case RAGErrorReranking:
		return ErrRAGReranking
	default:
		return ErrRAGRetrieval
	}
}

// Document is a unit of application content submitted for ingestion. ID must
// be stable across re-ingestion of the same document: chunk IDs derive from it,
// so re-indexing a changed document replaces its chunks by ID rather than
// accumulating duplicates.
//
// Source is free-form provenance — a file path, URL, or record key — and is
// copied onto every chunk so a retrieval result can cite where it came from.
// Metadata is raw JSON so applications can evolve their payloads without a
// contract change; it is merged into each chunk's vector metadata and is
// therefore filterable at retrieval time.
type Document struct {
	ID       string          `json:"id"`
	Content  string          `json:"content"`
	Source   string          `json:"source,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// Validate checks the ingestion contract. It rejects metadata that is not a
// JSON object, because chunk metadata is built by merging keys into it, and
// metadata that uses a reserved chunk metadata key, because the indexer would
// otherwise have to choose between clobbering the caller's value and corrupting
// provenance.
func (d Document) Validate() error {
	if d.ID == "" {
		return errors.New("lebro: document ID is required")
	}
	if d.Content == "" {
		return errors.New("lebro: document content is required")
	}
	if len(d.Metadata) == 0 {
		return nil
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(d.Metadata, &decoded); err != nil {
		return fmt.Errorf("lebro: document metadata must be a JSON object: %w", err)
	}
	// JSON null unmarshals into a nil map without error, so it must be rejected
	// explicitly: the indexer merges provenance keys into this map, and writing
	// to a nil map panics.
	if decoded == nil {
		return errors.New("lebro: document metadata must be a JSON object, got null")
	}
	for _, reserved := range reservedChunkMetadataKeys {
		if _, exists := decoded[reserved]; exists {
			return fmt.Errorf("lebro: document metadata must not set reserved key %q", reserved)
		}
	}
	return nil
}

// Chunk is one retrievable span of a document. Index is the chunk's ordinal
// position within its document, so a caller can restore reading order after a
// similarity search returns hits out of order.
//
// ID is assigned by the chunker as "<DocumentID>#<Index>", which makes it
// stable across re-ingestion: the same document chunked with the same strategy
// produces the same IDs, so an upsert replaces rather than duplicates.
type Chunk struct {
	ID         string          `json:"id"`
	DocumentID string          `json:"document_id"`
	Content    string          `json:"content"`
	Source     string          `json:"source,omitempty"`
	Index      int             `json:"index"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
}

// Validate checks that a chunk produced by a Chunker is structurally usable.
func (c Chunk) Validate() error {
	if c.ID == "" {
		return errors.New("lebro: chunk ID is required")
	}
	if c.DocumentID == "" {
		return errors.New("lebro: chunk document ID is required")
	}
	if c.Content == "" {
		return errors.New("lebro: chunk content is required")
	}
	if c.Index < 0 {
		return errors.New("lebro: chunk index must not be negative")
	}
	if err := validateJSON(c.Metadata); err != nil {
		return fmt.Errorf("lebro: chunk metadata %s", err)
	}
	return nil
}

// ChunkID renders the stable identifier for a chunk at position index within
// documentID. Chunkers use it so chunk identity is a property of the contract
// rather than of a particular strategy.
func ChunkID(documentID string, index int) string {
	return documentID + "#" + strconv.Itoa(index)
}

// Chunker splits a document into retrievable spans. Implementations must be
// safe for concurrent use, must assign each chunk a stable ID via ChunkID, and
// must propagate the document's Source and Metadata onto every chunk so
// provenance survives ingestion.
//
// A chunker returns an error rather than an empty slice for a document it
// cannot split, so an ingestion that silently indexed nothing is not mistaken
// for success.
type Chunker interface {
	Chunk(ctx context.Context, document Document) ([]Chunk, error)
}

// EmbeddingModel converts text into vectors. It is deliberately separate from
// Model: an embedding provider is not a chat provider, and the agent runtime
// must stay usable with neither.
//
// Implementations must be safe for concurrent use and must return exactly one
// vector per input, in input order, each of length Dimension. Callers batch
// their own inputs; an implementation may split a batch internally but must
// still preserve order.
type EmbeddingModel interface {
	// Embed returns one vector per input string, in the same order.
	//
	// Callers must not assume they own the returned slices, and must copy any
	// vector they retain beyond the call: an implementation is permitted to
	// reuse its buffers between calls.
	Embed(ctx context.Context, inputs []string) ([][]float32, error)

	// Dimension reports the fixed length of every vector this model produces.
	// It is used to create the vector index, so it must not vary per call.
	Dimension() int
}

// RetrievalQuery is a semantic search over indexed chunks. Query is natural
// language; the Retriever owns turning it into a vector, so callers — including
// a model calling a retrieval tool — never handle embeddings.
//
// TopK bounds the result count. MinScore is an optional cosine-similarity
// threshold in [0,1]. Filter narrows results by chunk metadata, using the same
// semantics as VectorMetadataFilter.
type RetrievalQuery struct {
	Query    string               `json:"query"`
	TopK     int                  `json:"top_k,omitempty"`
	MinScore float32              `json:"min_score,omitempty"`
	Filter   VectorMetadataFilter `json:"filter,omitempty"`
}

// Validate checks that a retrieval query is structurally usable.
func (q RetrievalQuery) Validate() error {
	if strings.TrimSpace(q.Query) == "" {
		return errors.New("lebro: retrieval query must not be empty")
	}
	if q.TopK < 0 {
		return errors.New("lebro: retrieval TopK must not be negative")
	}
	if q.MinScore < 0 || q.MinScore > 1 || isNaN(q.MinScore) {
		return errors.New("lebro: retrieval MinScore must be in [0, 1]")
	}
	return nil
}

// RetrievedChunk is a chunk that matched a retrieval query, with its cosine
// similarity to the query vector.
type RetrievedChunk struct {
	Chunk
	// Score is cosine similarity when no Reranker is configured; otherwise it
	// is the rerank score used to order this response.
	Score float32 `json:"score"`
	// VectorScore preserves the candidate's cosine similarity after reranking.
	// It is zero when no reranker is configured.
	VectorScore float32 `json:"vector_score,omitempty"`
	// ScoreExplanation describes the rerank score when a reranker provides one.
	ScoreExplanation string `json:"score_explanation,omitempty"`
}

// Retriever answers a semantic query with the chunks most relevant to it.
// Implementations must be safe for concurrent use and must return results
// ordered by descending score.
type Retriever interface {
	Retrieve(ctx context.Context, query RetrievalQuery) ([]RetrievedChunk, error)
}

// chunkMetadata builds the vector-record metadata for a chunk by merging the
// chunk's application metadata with the reserved provenance keys. The reserved
// keys are written last, but Document.Validate has already rejected documents
// that set them, so no caller value is lost here.
func chunkMetadata(chunk Chunk) (json.RawMessage, error) {
	merged := map[string]json.RawMessage{}
	if len(chunk.Metadata) > 0 {
		if err := json.Unmarshal(chunk.Metadata, &merged); err != nil {
			return nil, fmt.Errorf("lebro: chunk metadata must be a JSON object: %w", err)
		}
		// Decoding JSON null replaces the initialized map with a nil one, so it
		// must be re-established before the provenance keys are written. A
		// custom Chunker can emit metadata that never passed Document.Validate,
		// so this guard is not redundant with it.
		if merged == nil {
			merged = map[string]json.RawMessage{}
		}
	}

	documentID, err := json.Marshal(chunk.DocumentID)
	if err != nil {
		return nil, fmt.Errorf("lebro: encode chunk document ID: %w", err)
	}
	merged[ChunkMetadataDocumentID] = documentID

	index, err := json.Marshal(chunk.Index)
	if err != nil {
		return nil, fmt.Errorf("lebro: encode chunk index: %w", err)
	}
	merged[ChunkMetadataChunkIndex] = index

	// Source is optional, so it is recorded only when present. Writing an
	// empty string would make a metadata filter on source match documents that
	// never declared one.
	if chunk.Source != "" {
		source, err := json.Marshal(chunk.Source)
		if err != nil {
			return nil, fmt.Errorf("lebro: encode chunk source: %w", err)
		}
		merged[ChunkMetadataSource] = source
	}

	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("lebro: encode chunk metadata: %w", err)
	}
	return encoded, nil
}

// chunkFromMetadata reconstructs a chunk from a similarity hit. Reserved keys
// are consumed into their typed fields and removed from the application
// metadata, so a caller sees the metadata it supplied rather than the storage
// representation.
//
// Missing reserved keys are tolerated: a record written by something other than
// an Indexer still yields a usable chunk with its content and score, rather
// than failing an otherwise good retrieval.
//
// A reserved key that is present but not usable is a different matter and is
// rejected. Decoding JSON null into a string or int is a silent no-op, so
// tolerating it would surface an empty document ID or a negative chunk index as
// though it were real provenance — exactly the guarantee retrieval results are
// supposed to carry.
func chunkFromMetadata(result SimilarityResult) (Chunk, error) {
	chunk := Chunk{ID: result.ID, Content: result.Content}
	if len(result.Metadata) == 0 {
		return chunk, nil
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(result.Metadata, &decoded); err != nil {
		return Chunk{}, fmt.Errorf("lebro: decode chunk metadata: %w", err)
	}

	if raw, ok := decoded[ChunkMetadataDocumentID]; ok {
		if err := json.Unmarshal(raw, &chunk.DocumentID); err != nil {
			return Chunk{}, fmt.Errorf("lebro: decode chunk document ID: %w", err)
		}
		if chunk.DocumentID == "" {
			return Chunk{}, fmt.Errorf("lebro: chunk metadata %q must be a non-empty string", ChunkMetadataDocumentID)
		}
		delete(decoded, ChunkMetadataDocumentID)
	}
	if raw, ok := decoded[ChunkMetadataSource]; ok {
		if err := json.Unmarshal(raw, &chunk.Source); err != nil {
			return Chunk{}, fmt.Errorf("lebro: decode chunk source: %w", err)
		}
		// An indexer omits an empty source rather than writing "", so a present
		// but empty source is malformed provenance, not an absent one.
		if chunk.Source == "" {
			return Chunk{}, fmt.Errorf("lebro: chunk metadata %q must be a non-empty string", ChunkMetadataSource)
		}
		delete(decoded, ChunkMetadataSource)
	}
	if raw, ok := decoded[ChunkMetadataChunkIndex]; ok {
		// A null index decodes as a silent no-op, leaving 0 — indistinguishable
		// from a legitimate first chunk — so it is rejected explicitly rather
		// than through the range check below.
		if isJSONNull(raw) {
			return Chunk{}, fmt.Errorf("lebro: chunk metadata %q must not be null", ChunkMetadataChunkIndex)
		}
		if err := json.Unmarshal(raw, &chunk.Index); err != nil {
			return Chunk{}, fmt.Errorf("lebro: decode chunk index: %w", err)
		}
		if chunk.Index < 0 {
			return Chunk{}, fmt.Errorf("lebro: chunk metadata %q must not be negative, got %d", ChunkMetadataChunkIndex, chunk.Index)
		}
		delete(decoded, ChunkMetadataChunkIndex)
	}

	if len(decoded) > 0 {
		encoded, err := json.Marshal(decoded)
		if err != nil {
			return Chunk{}, fmt.Errorf("lebro: re-encode chunk metadata: %w", err)
		}
		chunk.Metadata = encoded
	}
	return chunk, nil
}

// isJSONNull reports whether raw is the JSON null literal. It exists because
// decoding null into a Go string or int succeeds without changing the target,
// so a null is otherwise indistinguishable from an absent key.
func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}
