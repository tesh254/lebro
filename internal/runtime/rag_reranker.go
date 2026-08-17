package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// DefaultRerankCandidateTopK is the bounded vector candidate pool used when a
// VectorRetriever has a Reranker but does not configure CandidateTopK.
const DefaultRerankCandidateTopK = 20

// RerankResult preserves the source candidate while reporting the score and
// optional explanation produced by a reranker.
type RerankResult struct {
	Candidate   RetrievedChunk
	Score       float32
	Explanation string
}

// Reranker reorders vector-search candidates for one natural-language query.
// Implementations must honor context cancellation and return every supplied
// candidate exactly once. VectorRetriever validates this contract and applies
// deterministic final tie-breaking.
type Reranker interface {
	Rerank(ctx context.Context, query string, candidates []RetrievedChunk) ([]RerankResult, error)
}

// RelevanceScorer scores one query/chunk pair. Explanation is surfaced to
// callers with the reranked result, allowing provider-specific reasoning to
// remain useful without exposing any provider type in the retrieval contract.
type RelevanceScorer interface {
	Score(ctx context.Context, query string, candidate Chunk) (score float32, explanation string, err error)
}

// ScorerReranker adapts a RelevanceScorer into a deterministic Reranker.
type ScorerReranker struct {
	scorer RelevanceScorer
}

var _ Reranker = (*ScorerReranker)(nil)

// NewScorerReranker validates scorer and returns a concurrent-safe adapter when
// its scorer is concurrent-safe.
func NewScorerReranker(scorer RelevanceScorer) (*ScorerReranker, error) {
	if scorer == nil || isNilInterface(scorer) {
		return nil, errors.New("lebro: relevance scorer is required")
	}
	return &ScorerReranker{scorer: scorer}, nil
}

// Rerank scores all candidates, then orders descending score. Equal scores use
// original vector score, chunk ID, and original position as deterministic ties.
func (r *ScorerReranker) Rerank(ctx context.Context, query string, candidates []RetrievedChunk) ([]RerankResult, error) {
	if r == nil || r.scorer == nil || isNilInterface(r.scorer) {
		return nil, errors.New("lebro: scorer reranker is nil")
	}
	if ctx == nil {
		return nil, errors.New("lebro: reranker context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("lebro: reranker query must not be empty")
	}

	results := make([]RerankResult, 0, len(candidates))
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		score, explanation, err := r.scorer.Score(ctx, query, candidate.Chunk)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, fmt.Errorf("score chunk %q: %w", candidate.ID, err)
		}
		if isNaN(score) {
			return nil, fmt.Errorf("score chunk %q: lebro: relevance score must not be NaN", candidate.ID)
		}
		results = append(results, RerankResult{Candidate: candidate, Score: score, Explanation: explanation})
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		if results[i].Candidate.Score != results[j].Candidate.Score {
			return results[i].Candidate.Score > results[j].Candidate.Score
		}
		return results[i].Candidate.ID < results[j].Candidate.ID
	})
	return results, nil
}
