package runtime

import (
	"context"
	"fmt"
	"sync"
)

// MemoryVectorStore is a concurrency-safe, in-process VectorStore intended
// for tests and local development. It performs brute-force cosine similarity
// search over all records in an index, which is suitable for small datasets
// but not production workloads.
type MemoryVectorStore struct {
	mu      sync.RWMutex
	indices map[string]*memoryVectorIndex
}

type memoryVectorIndex struct {
	dimension int
	records   map[string]EmbeddingRecord
	order     []string
}

// NewMemoryVectorStore creates an empty in-memory vector store.
func NewMemoryVectorStore() *MemoryVectorStore {
	return &MemoryVectorStore{
		indices: map[string]*memoryVectorIndex{},
	}
}

func (s *MemoryVectorStore) CreateIndex(ctx context.Context, index string, dimension int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if index == "" {
		return fmt.Errorf("%w: empty index name", ErrVectorInvalidInput)
	}
	if dimension <= 0 {
		return fmt.Errorf("%w: dimension must be positive", ErrVectorInvalidInput)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.indices[index]; exists {
		return fmt.Errorf("%w: index %q", ErrVectorAlreadyExists, index)
	}
	s.indices[index] = &memoryVectorIndex{
		dimension: dimension,
		records:   map[string]EmbeddingRecord{},
	}
	return nil
}

func (s *MemoryVectorStore) DeleteIndex(ctx context.Context, index string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if index == "" {
		return fmt.Errorf("%w: empty index name", ErrVectorInvalidInput)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.indices[index]; !exists {
		return fmt.Errorf("%w: index %q", ErrVectorNotFound, index)
	}
	delete(s.indices, index)
	return nil
}

func (s *MemoryVectorStore) Upsert(ctx context.Context, records []EmbeddingRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, record := range records {
		if err := validateVectorRecord(record); err != nil {
			return err
		}
		idx, exists := s.indices[record.Index]
		if !exists {
			return fmt.Errorf("%w: index %q", ErrVectorNotFound, record.Index)
		}
		if len(record.Vector) != idx.dimension {
			return fmt.Errorf("%w: record %q has dimension %d, index %q expects %d", ErrVectorInvalidDimension, record.ID, len(record.Vector), record.Index, idx.dimension)
		}
		cloned := EmbeddingRecord{
			ID:        record.ID,
			Index:     record.Index,
			Vector:    append([]float32(nil), record.Vector...),
			Dimension: len(record.Vector),
			Metadata:  cloneJSON(record.Metadata),
			Content:   record.Content,
			CreatedAt: record.CreatedAt,
		}
		if _, exists := idx.records[record.ID]; !exists {
			idx.order = append(idx.order, record.ID)
		}
		idx.records[record.ID] = cloned
	}
	return nil
}

func (s *MemoryVectorStore) Delete(ctx context.Context, index string, ids []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if index == "" {
		return fmt.Errorf("%w: empty index name", ErrVectorInvalidInput)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, exists := s.indices[index]
	if !exists {
		return fmt.Errorf("%w: index %q", ErrVectorNotFound, index)
	}
	for _, id := range ids {
		if _, exists := idx.records[id]; exists {
			delete(idx.records, id)
			for i, oid := range idx.order {
				if oid == id {
					idx.order = append(idx.order[:i], idx.order[i+1:]...)
					break
				}
			}
		}
	}
	return nil
}

func (s *MemoryVectorStore) Search(ctx context.Context, query SimilarityQuery) ([]SimilarityResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateSimilarityQuery(query); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	idx, exists := s.indices[query.Index]
	if !exists {
		return nil, fmt.Errorf("%w: index %q", ErrVectorNotFound, query.Index)
	}
	if len(query.Vector) != idx.dimension {
		return nil, fmt.Errorf("%w: query vector has dimension %d, index %q expects %d", ErrVectorInvalidDimension, len(query.Vector), query.Index, idx.dimension)
	}
	results := make([]SimilarityResult, 0, len(idx.order))
	for _, id := range idx.order {
		record := idx.records[id]
		if !metadataMatches(record.Metadata, query.Filter) {
			continue
		}
		score := CosineSimilarity(query.Vector, record.Vector)
		results = append(results, SimilarityResult{
			ID:       record.ID,
			Score:    score,
			Metadata: cloneJSON(record.Metadata),
			Content:  record.Content,
		})
	}
	return rankResults(results, query.TopK, query.MinScore), nil
}
