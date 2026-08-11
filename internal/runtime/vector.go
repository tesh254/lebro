package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
)

// Vector-store errors. Adapters must return these via errors.Is so callers
// can branch on known failure modes without parsing adapter-specific text.
var (
	// ErrVectorNotFound is returned when an index or record does not exist.
	ErrVectorNotFound = errors.New("lebro: vector index or record not found")

	// ErrVectorAlreadyExists is returned when CreateIndex is called for an
	// index that already exists.
	ErrVectorAlreadyExists = errors.New("lebro: vector index already exists")

	// ErrVectorInvalidDimension is returned when an upsert record's vector
	// dimension does not match the index's declared dimension.
	ErrVectorInvalidDimension = errors.New("lebro: vector dimension mismatch")

	// ErrVectorInvalidInput is returned when a call receives structurally
	// invalid input — empty index name, zero-dimension index, empty vector,
	// invalid metadata JSON, or a non-positive TopK.
	ErrVectorInvalidInput = errors.New("lebro: invalid vector input")
)

// EmbeddingRecord is the durable representation of a single embedding stored
// in a vector index. ID is scoped per index so the same ID may appear in
// different indices without collision. Dimension must match the index's
// declared dimension on upsert. Metadata is raw JSON so applications can
// evolve their payload schemas without a storage-adapter change; it is
// filterable via VectorMetadataFilter on similarity queries.
type EmbeddingRecord struct {
	ID        string          `json:"id"`
	Index     string          `json:"index"`
	Vector    []float32       `json:"vector"`
	Dimension int             `json:"dimension,omitempty"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	Content   string          `json:"content,omitempty"`
	CreatedAt string          `json:"created_at,omitempty"`
}

// VectorMetadataFilter narrows a similarity search to records whose metadata
// matches every key/value pair in Match. A nil or zero-value filter returns
// all records. Values are raw JSON so callers can match strings, numbers,
// booleans, or nested objects. A key that is absent from a record's metadata
// excludes that record from the result set.
type VectorMetadataFilter struct {
	Match map[string]json.RawMessage `json:"match,omitempty"`
}

// SimilarityQuery specifies a cosine-similarity search against a vector index.
// Vector must have the same dimension as the index. TopK bounds the result
// count and must be positive. MinScore is an optional threshold in [0,1];
// results with a score below MinScore are excluded. A zero MinScore returns
// all results regardless of score.
type SimilarityQuery struct {
	Vector   []float32            `json:"vector"`
	Index    string               `json:"index"`
	Filter   VectorMetadataFilter `json:"filter,omitempty"`
	TopK     int                  `json:"top_k"`
	MinScore float32              `json:"min_score,omitempty"`
}

// SimilarityResult is a single hit from a similarity search. Score is the
// cosine similarity between the query vector and the stored vector, in the
// range [-1, 1] (typically [0, 1] for normalized embeddings).
type SimilarityResult struct {
	ID       string          `json:"id"`
	Score    float32         `json:"score"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
	Content  string          `json:"content,omitempty"`
}

// VectorStore is a provider-neutral vector storage interface. It is
// intentionally separate from Store: agent and workflow packages must remain
// usable with no vector dependency. Adapters own their schema migrations
// (Migrate is not part of this interface; each adapter exposes its own
// Migrate method or constructor that handles schema setup).
type VectorStore interface {
	// CreateIndex creates a new vector index with the given name and fixed
	// dimension. Returns ErrVectorAlreadyExists if the index exists.
	CreateIndex(ctx context.Context, index string, dimension int) error

	// DeleteIndex removes an index and all its records. Returns
	// ErrVectorNotFound if the index does not exist.
	DeleteIndex(ctx context.Context, index string) error

	// Upsert inserts or replaces embedding records. Each record's Dimension
	// must match the index's declared dimension. Records are upserted by ID:
	// an existing ID is replaced. Returns ErrVectorNotFound if the index
	// does not exist.
	Upsert(ctx context.Context, records []EmbeddingRecord) error

	// Delete removes records by ID from the given index. Missing IDs are
	// silently ignored (idempotent). Returns ErrVectorNotFound if the index
	// does not exist.
	Delete(ctx context.Context, index string, ids []string) error

	// Search returns the TopK most similar records to the query vector,
	// ordered by descending cosine similarity. If Filter is non-empty, only
	// records whose metadata matches every key/value pair are considered.
	// Results with a score below MinScore are excluded when MinScore > 0.
	// Returns ErrVectorNotFound if the index does not exist.
	Search(ctx context.Context, query SimilarityQuery) ([]SimilarityResult, error)
}

// CosineSimilarity computes the cosine similarity between two float32 vectors.
// It returns 0 when either vector has zero magnitude. Both vectors must have
// the same length; the caller is responsible for dimension validation.
func CosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		af, bf := float64(a[i]), float64(b[i])
		dot += af * bf
		na += af * af
		nb += bf * bf
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}

// validateVectorRecord checks that a record is structurally valid before it
// reaches an adapter. It does not check dimension against an index — that is
// the adapter's responsibility because the index dimension is adapter state.
func validateVectorRecord(record EmbeddingRecord) error {
	if record.ID == "" {
		return fmt.Errorf("%w: empty record ID", ErrVectorInvalidInput)
	}
	if record.Index == "" {
		return fmt.Errorf("%w: empty index name", ErrVectorInvalidInput)
	}
	if len(record.Vector) == 0 {
		return fmt.Errorf("%w: empty vector", ErrVectorInvalidInput)
	}
	if record.Dimension != 0 && record.Dimension != len(record.Vector) {
		return fmt.Errorf("%w: declared dimension %d does not match vector length %d", ErrVectorInvalidDimension, record.Dimension, len(record.Vector))
	}
	if err := validateJSON(record.Metadata); err != nil {
		return fmt.Errorf("%w: metadata %s", ErrVectorInvalidInput, err)
	}
	return nil
}

// validateSimilarityQuery checks that a query is structurally valid before it
// reaches an adapter.
func validateSimilarityQuery(query SimilarityQuery) error {
	if query.Index == "" {
		return fmt.Errorf("%w: empty index name", ErrVectorInvalidInput)
	}
	if len(query.Vector) == 0 {
		return fmt.Errorf("%w: empty query vector", ErrVectorInvalidInput)
	}
	if query.TopK <= 0 {
		return fmt.Errorf("%w: TopK must be positive", ErrVectorInvalidInput)
	}
	if query.MinScore < 0 || query.MinScore > 1 || isNaN(query.MinScore) {
		return fmt.Errorf("%w: MinScore must be in [0, 1]", ErrVectorInvalidInput)
	}
	return nil
}

// metadataMatches checks whether a record's metadata contains every key/value
// pair in the filter. A nil or empty filter matches everything.
func metadataMatches(recordMetadata json.RawMessage, filter VectorMetadataFilter) bool {
	if len(filter.Match) == 0 {
		return true
	}
	if len(recordMetadata) == 0 {
		return false
	}
	var recordMap map[string]json.RawMessage
	if err := json.Unmarshal(recordMetadata, &recordMap); err != nil {
		return false
	}
	for key, want := range filter.Match {
		got, ok := recordMap[key]
		if !ok || !jsonEqual(got, want) {
			return false
		}
	}
	return true
}

// jsonEqual compares two RawMessage values by parsing with UseNumber so large
// integers retain exact precision, then re-marshaling so key ordering and
// whitespace differences do not cause false negatives.
func jsonEqual(a, b json.RawMessage) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	va, err := decodeJSONNumber(a)
	if err != nil {
		return false
	}
	vb, err := decodeJSONNumber(b)
	if err != nil {
		return false
	}
	na, _ := json.Marshal(va)
	nb, _ := json.Marshal(vb)
	return string(na) == string(nb)
}

// decodeJSONNumber unmarshals raw JSON into an any using UseNumber so large
// integers are preserved as json.Number instead of losing precision through
// float64.
func decodeJSONNumber(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// isNaN reports whether f is a NaN value. math.IsNaN operates on float64, so
// this helper bridges the float32 domain.
func isNaN(f float32) bool { return f != f }

// rankResults sorts similarity results by descending score and trims to TopK,
// excluding results with a score below MinScore when MinScore > 0.
func rankResults(results []SimilarityResult, topK int, minScore float32) []SimilarityResult {
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})
	if minScore > 0 {
		cutoff := results
		for i, r := range results {
			if r.Score < minScore {
				cutoff = results[:i]
				break
			}
		}
		results = cutoff
	}
	if topK > 0 && topK < len(results) {
		results = results[:topK]
	}
	return results
}
