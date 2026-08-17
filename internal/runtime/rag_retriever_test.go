package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

type fixtureRelevanceScorer struct {
	scores       map[string]float32
	explanations map[string]string
	err          error
	calls        *int
}

type fixtureReranker struct{ results []RerankResult }

func (r fixtureReranker) Rerank(_ context.Context, _ string, _ []RetrievedChunk) ([]RerankResult, error) {
	return r.results, nil
}

func (s fixtureRelevanceScorer) Score(ctx context.Context, _ string, candidate Chunk) (float32, string, error) {
	if s.calls != nil {
		*s.calls++
	}
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	if s.err != nil {
		return 0, "", s.err
	}
	return s.scores[candidate.ID], s.explanations[candidate.ID], nil
}

// seedRetrievalIndex ingests a small corpus and returns a retriever over it.
func seedRetrievalIndex(t *testing.T, config VectorRetrieverConfig) (*VectorRetriever, *stubEmbedder, VectorStore) {
	t.Helper()
	ctx := context.Background()
	store := NewMemoryVectorStore()
	embedder := newStubEmbedder(16)

	chunker, err := NewCharacterChunker(CharacterChunkerConfig{Size: 200})
	if err != nil {
		t.Fatalf("NewCharacterChunker error = %v", err)
	}
	indexer, err := NewIndexer(IndexerConfig{
		Chunker:    chunker,
		Embeddings: embedder,
		Store:      store,
		Index:      "docs",
	})
	if err != nil {
		t.Fatalf("NewIndexer error = %v", err)
	}
	if err := indexer.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex error = %v", err)
	}

	documents := []Document{
		{ID: "billing", Content: "Invoices are issued monthly and payment is due in 30 days.", Source: "billing.md", Metadata: json.RawMessage(`{"tenant":"acme"}`)},
		{ID: "onboarding", Content: "New engineers receive a laptop and repository access on day one.", Source: "onboarding.md", Metadata: json.RawMessage(`{"tenant":"acme"}`)},
		{ID: "secret", Content: "Internal compensation bands are confidential.", Source: "secret.md", Metadata: json.RawMessage(`{"tenant":"other"}`)},
	}
	for _, document := range documents {
		if _, err := indexer.Ingest(ctx, document); err != nil {
			t.Fatalf("Ingest(%s) error = %v", document.ID, err)
		}
	}

	config.Embeddings = embedder
	config.Store = store
	if config.Index == "" {
		config.Index = "docs"
	}
	retriever, err := NewVectorRetriever(config)
	if err != nil {
		t.Fatalf("NewVectorRetriever error = %v", err)
	}
	return retriever, embedder, store
}

func TestNewVectorRetrieverValidation(t *testing.T) {
	embedder := newStubEmbedder(8)
	store := NewMemoryVectorStore()

	tests := []struct {
		name    string
		config  VectorRetrieverConfig
		wantErr string
	}{
		{name: "valid", config: VectorRetrieverConfig{Embeddings: embedder, Store: store, Index: "docs"}},
		{name: "missing embeddings", config: VectorRetrieverConfig{Store: store, Index: "docs"}, wantErr: "embedding model is required"},
		{name: "missing store", config: VectorRetrieverConfig{Embeddings: embedder, Index: "docs"}, wantErr: "vector store is required"},
		{name: "missing index", config: VectorRetrieverConfig{Embeddings: embedder, Store: store}, wantErr: "index name is required"},
		{name: "negative top k", config: VectorRetrieverConfig{Embeddings: embedder, Store: store, Index: "docs", TopK: -1}, wantErr: "TopK must not be negative"},
		{name: "min score above one", config: VectorRetrieverConfig{Embeddings: embedder, Store: store, Index: "docs", MinScore: 2}, wantErr: "MinScore must be in [0, 1]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewVectorRetriever(test.config)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("NewVectorRetriever error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("NewVectorRetriever error = nil, want %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), test.wantErr)
			}
		})
	}
}

func TestVectorRetrieverDefaultTopK(t *testing.T) {
	retriever, _, _ := seedRetrievalIndex(t, VectorRetrieverConfig{})
	if retriever.topK != DefaultRetrievalTopK {
		t.Fatalf("topK = %d, want %d", retriever.topK, DefaultRetrievalTopK)
	}
}

func TestVectorRetrieverRetrievesWithProvenance(t *testing.T) {
	retriever, _, _ := seedRetrievalIndex(t, VectorRetrieverConfig{TopK: 3})

	results, err := retriever.Retrieve(context.Background(), RetrievalQuery{Query: "invoices and payment"})
	if err != nil {
		t.Fatalf("Retrieve error = %v", err)
	}
	if len(results) == 0 {
		t.Fatal("len(results) = 0, want at least one")
	}

	// Every hit must be attributable, which is what makes a retrieval answer
	// citable.
	for i, result := range results {
		if result.DocumentID == "" {
			t.Fatalf("results[%d].DocumentID is empty", i)
		}
		if result.Source == "" {
			t.Fatalf("results[%d].Source is empty", i)
		}
		if result.Content == "" {
			t.Fatalf("results[%d].Content is empty", i)
		}
	}
	// Results must be ordered by descending score.
	for i := 1; i < len(results); i++ {
		if results[i-1].Score < results[i].Score {
			t.Fatalf("results not ordered by descending score: %f then %f", results[i-1].Score, results[i].Score)
		}
	}
}

func TestVectorRetrieverRespectsTopK(t *testing.T) {
	retriever, _, _ := seedRetrievalIndex(t, VectorRetrieverConfig{TopK: 3})

	results, err := retriever.Retrieve(context.Background(), RetrievalQuery{Query: "laptop access", TopK: 1})
	if err != nil {
		t.Fatalf("Retrieve error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
}

func TestVectorRetrieverReranksBoundedCandidates(t *testing.T) {
	reranker, err := NewScorerReranker(fixtureRelevanceScorer{
		scores:       map[string]float32{"billing#0": 1, "onboarding#0": 2, "secret#0": 3},
		explanations: map[string]string{"secret#0": "highest relevance"},
	})
	if err != nil {
		t.Fatalf("NewScorerReranker error = %v", err)
	}
	retriever, _, _ := seedRetrievalIndex(t, VectorRetrieverConfig{
		TopK:          1,
		Reranker:      reranker,
		CandidateTopK: 3,
	})

	results, err := retriever.Retrieve(context.Background(), RetrievalQuery{Query: "anything"})
	if err != nil {
		t.Fatalf("Retrieve error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].ID != "secret#0" {
		t.Fatalf("results[0].ID = %q, want secret#0", results[0].ID)
	}
	if results[0].Score != 3 || results[0].VectorScore == 0 {
		t.Fatalf("result scores = rerank %.2f/vector %.2f, want rerank 3 and preserved vector score", results[0].Score, results[0].VectorScore)
	}
	if results[0].ScoreExplanation != "highest relevance" {
		t.Fatalf("ScoreExplanation = %q, want scorer explanation", results[0].ScoreExplanation)
	}
}

func TestNewVectorRetrieverRerankerValidation(t *testing.T) {
	embedder := newStubEmbedder(8)
	store := NewMemoryVectorStore()
	_, err := NewVectorRetriever(VectorRetrieverConfig{Embeddings: embedder, Store: store, Index: "docs", CandidateTopK: 2})
	if err == nil || !strings.Contains(err.Error(), "requires a reranker") {
		t.Fatalf("NewVectorRetriever error = %v, want CandidateTopK reranker validation", err)
	}
	_, err = NewVectorRetriever(VectorRetrieverConfig{Embeddings: embedder, Store: store, Index: "docs", CandidateTopK: -1})
	if err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("NewVectorRetriever error = %v, want negative CandidateTopK validation", err)
	}
}

func TestScorerRerankerDeterministicTiesAndErrors(t *testing.T) {
	providerErr := errors.New("scorer unavailable")
	_, err := NewScorerReranker(nil)
	if err == nil {
		t.Fatal("NewScorerReranker(nil) error = nil, want error")
	}

	reranker, err := NewScorerReranker(fixtureRelevanceScorer{scores: map[string]float32{"b": 1, "a": 1}})
	if err != nil {
		t.Fatalf("NewScorerReranker error = %v", err)
	}
	results, err := reranker.Rerank(context.Background(), "query", []RetrievedChunk{
		{Chunk: Chunk{ID: "b"}, Score: 0.5},
		{Chunk: Chunk{ID: "a"}, Score: 0.5},
	})
	if err != nil {
		t.Fatalf("Rerank error = %v", err)
	}
	if results[0].Candidate.ID != "a" || results[1].Candidate.ID != "b" {
		t.Fatalf("tie order = %q, %q; want a, b", results[0].Candidate.ID, results[1].Candidate.ID)
	}

	failing, err := NewScorerReranker(fixtureRelevanceScorer{err: providerErr})
	if err != nil {
		t.Fatalf("NewScorerReranker error = %v", err)
	}
	_, err = failing.Rerank(context.Background(), "query", []RetrievedChunk{{Chunk: Chunk{ID: "a"}}})
	if !errors.Is(err, providerErr) {
		t.Fatalf("Rerank error = %v, want provider error preserved", err)
	}

	nan, err := NewScorerReranker(fixtureRelevanceScorer{scores: map[string]float32{"a": float32(math.NaN())}})
	if err != nil {
		t.Fatalf("NewScorerReranker error = %v", err)
	}
	_, err = nan.Rerank(context.Background(), "query", []RetrievedChunk{{Chunk: Chunk{ID: "a"}}})
	if err == nil || !strings.Contains(err.Error(), "must not be NaN") {
		t.Fatalf("Rerank error = %v, want NaN validation", err)
	}
}

func TestVectorRetrieverWrapsRerankerError(t *testing.T) {
	providerErr := errors.New("provider unavailable")
	reranker, err := NewScorerReranker(fixtureRelevanceScorer{err: providerErr})
	if err != nil {
		t.Fatalf("NewScorerReranker error = %v", err)
	}
	retriever, _, _ := seedRetrievalIndex(t, VectorRetrieverConfig{Reranker: reranker, CandidateTopK: 2})
	_, err = retriever.Retrieve(context.Background(), RetrievalQuery{Query: "anything"})
	if !errors.Is(err, ErrRAGReranking) || !errors.Is(err, providerErr) {
		t.Fatalf("Retrieve error = %v, want reranking and provider errors", err)
	}
}

func TestVectorRetrieverSkipsRerankerForEmptyCandidatePool(t *testing.T) {
	calls := 0
	reranker, err := NewScorerReranker(fixtureRelevanceScorer{calls: &calls})
	if err != nil {
		t.Fatalf("NewScorerReranker error = %v", err)
	}
	retriever, err := NewVectorRetriever(VectorRetrieverConfig{
		Embeddings:    newStubEmbedder(8),
		Store:         NewMemoryVectorStore(),
		Index:         "empty",
		Reranker:      reranker,
		CandidateTopK: 2,
	})
	if err != nil {
		t.Fatalf("NewVectorRetriever error = %v", err)
	}
	if err := retriever.store.CreateIndex(context.Background(), "empty", 8); err != nil {
		t.Fatalf("CreateIndex error = %v", err)
	}
	results, err := retriever.Retrieve(context.Background(), RetrievalQuery{Query: "anything"})
	if err != nil {
		t.Fatalf("Retrieve error = %v", err)
	}
	if len(results) != 0 || calls != 0 {
		t.Fatalf("results/calls = %d/%d, want 0/0", len(results), calls)
	}
}

func TestVectorRetrieverRejectsInvalidRerankerResults(t *testing.T) {
	retriever, _, _ := seedRetrievalIndex(t, VectorRetrieverConfig{
		TopK:          1,
		CandidateTopK: 2,
		Reranker: fixtureReranker{results: []RerankResult{
			{Candidate: RetrievedChunk{Chunk: Chunk{ID: "missing"}}, Score: 1},
			{Candidate: RetrievedChunk{Chunk: Chunk{ID: "missing"}}, Score: 0},
		}},
	})
	_, err := retriever.Retrieve(context.Background(), RetrievalQuery{Query: "anything"})
	if !errors.Is(err, ErrRAGReranking) || !strings.Contains(err.Error(), "unknown chunk") {
		t.Fatalf("Retrieve error = %v, want validated reranker result error", err)
	}
}

// TestVectorRetrieverEnforcesConfiguredFilter is the isolation guarantee: a
// configured scope must hold for every query.
func TestVectorRetrieverEnforcesConfiguredFilter(t *testing.T) {
	retriever, _, _ := seedRetrievalIndex(t, VectorRetrieverConfig{
		TopK:   10,
		Filter: VectorMetadataFilter{Match: map[string]json.RawMessage{"tenant": json.RawMessage(`"acme"`)}},
	})

	results, err := retriever.Retrieve(context.Background(), RetrievalQuery{Query: "confidential compensation bands"})
	if err != nil {
		t.Fatalf("Retrieve error = %v", err)
	}
	for _, result := range results {
		if result.DocumentID == "secret" {
			t.Fatal("retrieved a document outside the configured tenant filter")
		}
	}
}

// TestVectorRetrieverCallerCannotEscapeFilter asserts the precedence rule: a
// caller naming the enforced key does not widen its own scope.
func TestVectorRetrieverCallerCannotEscapeFilter(t *testing.T) {
	retriever, _, _ := seedRetrievalIndex(t, VectorRetrieverConfig{
		TopK:   10,
		Filter: VectorMetadataFilter{Match: map[string]json.RawMessage{"tenant": json.RawMessage(`"acme"`)}},
	})

	results, err := retriever.Retrieve(context.Background(), RetrievalQuery{
		Query:  "confidential compensation bands",
		Filter: VectorMetadataFilter{Match: map[string]json.RawMessage{"tenant": json.RawMessage(`"other"`)}},
	})
	if err != nil {
		t.Fatalf("Retrieve error = %v", err)
	}
	for _, result := range results {
		if result.DocumentID == "secret" {
			t.Fatal("caller filter overrode the configured tenant scope")
		}
	}
}

func TestVectorRetrieverAppliesMinScore(t *testing.T) {
	// A threshold of 1 admits only a vector identical to the query.
	retriever, _, _ := seedRetrievalIndex(t, VectorRetrieverConfig{TopK: 10, MinScore: 1})

	results, err := retriever.Retrieve(context.Background(), RetrievalQuery{Query: "nothing like the corpus at all"})
	if err != nil {
		t.Fatalf("Retrieve error = %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("len(results) = %d, want 0 under a MinScore of 1", len(results))
	}
}

func TestVectorRetrieverRejectsInvalidQuery(t *testing.T) {
	retriever, _, _ := seedRetrievalIndex(t, VectorRetrieverConfig{})

	if _, err := retriever.Retrieve(context.Background(), RetrievalQuery{}); !errors.Is(err, ErrRAGRetrieval) {
		t.Fatalf("Retrieve error = %v, want ErrRAGRetrieval", err)
	}
}

func TestVectorRetrieverEmbeddingFailure(t *testing.T) {
	retriever, embedder, _ := seedRetrievalIndex(t, VectorRetrieverConfig{})
	providerErr := errors.New("provider down")
	embedder.err = providerErr

	_, err := retriever.Retrieve(context.Background(), RetrievalQuery{Query: "anything"})
	if !errors.Is(err, ErrRAGEmbedding) {
		t.Fatalf("Retrieve error = %v, want ErrRAGEmbedding", err)
	}
	if !errors.Is(err, providerErr) {
		t.Fatalf("Retrieve error = %v, want the provider error preserved", err)
	}
}

func TestVectorRetrieverMissingIndex(t *testing.T) {
	embedder := newStubEmbedder(8)
	retriever, err := NewVectorRetriever(VectorRetrieverConfig{
		Embeddings: embedder,
		Store:      NewMemoryVectorStore(),
		Index:      "absent",
	})
	if err != nil {
		t.Fatalf("NewVectorRetriever error = %v", err)
	}

	_, err = retriever.Retrieve(context.Background(), RetrievalQuery{Query: "anything"})
	if !errors.Is(err, ErrRAGRetrieval) {
		t.Fatalf("Retrieve error = %v, want ErrRAGRetrieval", err)
	}
	if !errors.Is(err, ErrVectorNotFound) {
		t.Fatalf("Retrieve error = %v, want ErrVectorNotFound preserved", err)
	}
}

func TestVectorRetrieverCanceledContext(t *testing.T) {
	retriever, _, _ := seedRetrievalIndex(t, VectorRetrieverConfig{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := retriever.Retrieve(ctx, RetrievalQuery{Query: "anything"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Retrieve error = %v, want context.Canceled", err)
	}
}

func TestVectorRetrieverNilReceiverAndContext(t *testing.T) {
	var retriever *VectorRetriever
	if _, err := retriever.Retrieve(context.Background(), RetrievalQuery{Query: "x"}); err == nil {
		t.Fatal("Retrieve on nil retriever error = nil, want an error")
	}

	real, _, _ := seedRetrievalIndex(t, VectorRetrieverConfig{})
	//nolint:staticcheck // deliberately passing a nil context to assert the guard
	if _, err := real.Retrieve(nil, RetrievalQuery{Query: "x"}); err == nil {
		t.Fatal("Retrieve with nil context error = nil, want an error")
	}
}

func TestNewRetrievalToolValidation(t *testing.T) {
	retriever, _, _ := seedRetrievalIndex(t, VectorRetrieverConfig{})

	tests := []struct {
		name    string
		config  RetrievalToolConfig
		wantErr string
	}{
		{name: "valid", config: RetrievalToolConfig{ID: "search", Retriever: retriever}},
		{name: "missing ID", config: RetrievalToolConfig{Retriever: retriever}, wantErr: "tool ID is required"},
		{name: "missing retriever", config: RetrievalToolConfig{ID: "search"}, wantErr: "retriever is required"},
		{name: "negative top k", config: RetrievalToolConfig{ID: "search", Retriever: retriever, TopK: -1}, wantErr: "TopK must not be negative"},
		{name: "negative max top k", config: RetrievalToolConfig{ID: "search", Retriever: retriever, MaxTopK: -1}, wantErr: "MaxTopK must not be negative"},
		{name: "top k above max", config: RetrievalToolConfig{ID: "search", Retriever: retriever, TopK: 10, MaxTopK: 5}, wantErr: "must not exceed MaxTopK"},
		{name: "min score above one", config: RetrievalToolConfig{ID: "search", Retriever: retriever, MinScore: 1.2}, wantErr: "MinScore must be in [0, 1]"},
		{name: "invalid input schema", config: RetrievalToolConfig{ID: "search", Retriever: retriever, InputSchema: json.RawMessage(`{`)}, wantErr: "input schema must be valid JSON"},
		{name: "invalid output schema", config: RetrievalToolConfig{ID: "search", Retriever: retriever, OutputSchema: json.RawMessage(`{`)}, wantErr: "output schema must be valid JSON"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewRetrievalTool(test.config)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("NewRetrievalTool error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("NewRetrievalTool error = nil, want %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), test.wantErr)
			}
		})
	}
}

func TestRetrievalToolDefinition(t *testing.T) {
	retriever, _, _ := seedRetrievalIndex(t, VectorRetrieverConfig{})
	tool, err := NewRetrievalTool(RetrievalToolConfig{ID: "search", Retriever: retriever})
	if err != nil {
		t.Fatalf("NewRetrievalTool error = %v", err)
	}

	definition := tool.Definition()
	if definition.ID != "search" {
		t.Fatalf("ID = %q, want %q", definition.ID, "search")
	}
	if definition.Description == "" {
		t.Fatal("Description is empty, want a default")
	}
	if !json.Valid(definition.InputSchema) || !json.Valid(definition.OutputSchema) {
		t.Fatal("schemas are not valid JSON")
	}

	// The input schema must not offer a filter or index: retrieval scope is
	// configuration, not a model's choice.
	var schema struct {
		Properties           map[string]json.RawMessage `json:"properties"`
		AdditionalProperties *bool                      `json:"additionalProperties"`
	}
	if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
		t.Fatalf("unmarshal input schema error = %v", err)
	}
	for _, forbidden := range []string{"filter", "index", "metadata", "min_score"} {
		if _, exists := schema.Properties[forbidden]; exists {
			t.Fatalf("input schema exposes %q to the model", forbidden)
		}
	}
	if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
		t.Fatal("input schema must set additionalProperties: false")
	}

	// Mutating the returned definition must not affect the tool.
	definition.InputSchema[0] = 'X'
	if tool.Definition().InputSchema[0] == 'X' {
		t.Fatal("Definition() did not return a defensive copy")
	}
}

func TestRetrievalToolExecute(t *testing.T) {
	retriever, _, _ := seedRetrievalIndex(t, VectorRetrieverConfig{TopK: 5})
	tool, err := NewRetrievalTool(RetrievalToolConfig{ID: "search", Retriever: retriever, TopK: 2})
	if err != nil {
		t.Fatalf("NewRetrievalTool error = %v", err)
	}

	output, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"invoices and payment terms"}`))
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}

	var decoded retrievalToolOutput
	if err := json.Unmarshal(output, &decoded); err != nil {
		t.Fatalf("unmarshal output error = %v", err)
	}
	if len(decoded.Chunks) == 0 {
		t.Fatal("len(chunks) = 0, want at least one")
	}
	if len(decoded.Chunks) > 2 {
		t.Fatalf("len(chunks) = %d, want at most the configured TopK of 2", len(decoded.Chunks))
	}
	for i, chunk := range decoded.Chunks {
		if chunk.Content == "" {
			t.Fatalf("chunks[%d].Content is empty", i)
		}
		if chunk.DocumentID == "" {
			t.Fatalf("chunks[%d].DocumentID is empty", i)
		}
	}
}

// TestRetrievalToolClampsTopK is the context-budget guarantee: a model cannot
// enlarge its own retrieval beyond what the application allowed.
func TestRetrievalToolClampsTopK(t *testing.T) {
	retriever, _, _ := seedRetrievalIndex(t, VectorRetrieverConfig{TopK: 10})
	tool, err := NewRetrievalTool(RetrievalToolConfig{ID: "search", Retriever: retriever, TopK: 1, MaxTopK: 2})
	if err != nil {
		t.Fatalf("NewRetrievalTool error = %v", err)
	}

	output, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"anything at all","top_k":100}`))
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	var decoded retrievalToolOutput
	if err := json.Unmarshal(output, &decoded); err != nil {
		t.Fatalf("unmarshal output error = %v", err)
	}
	if len(decoded.Chunks) > 2 {
		t.Fatalf("len(chunks) = %d, want at most MaxTopK of 2", len(decoded.Chunks))
	}
}

// TestRetrievalToolMaxTopKDefaultsToTopK asserts an unset cap still bounds a
// model-supplied value, rather than leaving it unbounded.
func TestRetrievalToolMaxTopKDefaultsToTopK(t *testing.T) {
	retriever, _, _ := seedRetrievalIndex(t, VectorRetrieverConfig{TopK: 10})
	tool, err := NewRetrievalTool(RetrievalToolConfig{ID: "search", Retriever: retriever, TopK: 1})
	if err != nil {
		t.Fatalf("NewRetrievalTool error = %v", err)
	}
	if tool.maxTopK != 1 {
		t.Fatalf("maxTopK = %d, want it to default to TopK of 1", tool.maxTopK)
	}

	output, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"anything","top_k":50}`))
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	var decoded retrievalToolOutput
	if err := json.Unmarshal(output, &decoded); err != nil {
		t.Fatalf("unmarshal output error = %v", err)
	}
	if len(decoded.Chunks) > 1 {
		t.Fatalf("len(chunks) = %d, want at most 1", len(decoded.Chunks))
	}
}

func TestRetrievalToolEnforcesFilter(t *testing.T) {
	retriever, _, _ := seedRetrievalIndex(t, VectorRetrieverConfig{TopK: 10})
	tool, err := NewRetrievalTool(RetrievalToolConfig{
		ID:        "search",
		Retriever: retriever,
		TopK:      10,
		Filter:    VectorMetadataFilter{Match: map[string]json.RawMessage{"tenant": json.RawMessage(`"acme"`)}},
	})
	if err != nil {
		t.Fatalf("NewRetrievalTool error = %v", err)
	}

	output, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"confidential compensation bands"}`))
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	var decoded retrievalToolOutput
	if err := json.Unmarshal(output, &decoded); err != nil {
		t.Fatalf("unmarshal output error = %v", err)
	}
	for _, chunk := range decoded.Chunks {
		if chunk.DocumentID == "secret" {
			t.Fatal("retrieval tool returned a document outside its configured filter")
		}
	}
}

func TestRetrievalToolEmptyResultIsSuccess(t *testing.T) {
	retriever, _, _ := seedRetrievalIndex(t, VectorRetrieverConfig{TopK: 5, MinScore: 1})
	tool, err := NewRetrievalTool(RetrievalToolConfig{ID: "search", Retriever: retriever, MinScore: 1})
	if err != nil {
		t.Fatalf("NewRetrievalTool error = %v", err)
	}

	output, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"utterly unrelated text"}`))
	if err != nil {
		t.Fatalf("Execute error = %v, want an empty success", err)
	}
	// The envelope must always carry a chunks array so a model never branches
	// on shape.
	if !strings.Contains(string(output), `"chunks":[]`) {
		t.Fatalf("output = %s, want an empty chunks array", output)
	}
}

func TestRetrievalToolRejectsBadInput(t *testing.T) {
	retriever, _, _ := seedRetrievalIndex(t, VectorRetrieverConfig{})
	tool, err := NewRetrievalTool(RetrievalToolConfig{ID: "search", Retriever: retriever})
	if err != nil {
		t.Fatalf("NewRetrievalTool error = %v", err)
	}

	tests := []struct {
		name  string
		input string
	}{
		{name: "malformed JSON", input: `{`},
		{name: "empty query", input: `{"query":""}`},
		{name: "missing query", input: `{}`},
		{name: "negative top k", input: `{"query":"x","top_k":-1}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := tool.Execute(context.Background(), json.RawMessage(test.input)); !errors.Is(err, ErrRAGRetrieval) {
				t.Fatalf("Execute error = %v, want ErrRAGRetrieval", err)
			}
		})
	}
}

func TestRetrievalToolCanceledContext(t *testing.T) {
	retriever, _, _ := seedRetrievalIndex(t, VectorRetrieverConfig{})
	tool, err := NewRetrievalTool(RetrievalToolConfig{ID: "search", Retriever: retriever})
	if err != nil {
		t.Fatalf("NewRetrievalTool error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// The bare context error is what makes the registry classify this as
	// ToolExecutionCancelled rather than a handler failure.
	if _, err := tool.Execute(ctx, json.RawMessage(`{"query":"x"}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute error = %v, want context.Canceled", err)
	}
}

func TestRetrievalToolNilReceiverAndContext(t *testing.T) {
	var tool *RetrievalTool
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"x"}`)); err == nil {
		t.Fatal("Execute on nil tool error = nil, want an error")
	}
	if got := tool.Definition(); got.ID != "" {
		t.Fatalf("Definition() on nil tool = %+v, want zero value", got)
	}

	retriever, _, _ := seedRetrievalIndex(t, VectorRetrieverConfig{})
	real, err := NewRetrievalTool(RetrievalToolConfig{ID: "search", Retriever: retriever})
	if err != nil {
		t.Fatalf("NewRetrievalTool error = %v", err)
	}
	//nolint:staticcheck // deliberately passing a nil context to assert the guard
	if _, err := real.Execute(nil, json.RawMessage(`{"query":"x"}`)); err == nil {
		t.Fatal("Execute with nil context error = nil, want an error")
	}
}

// TestRetrievalToolSatisfiesToolContract keeps the tool usable by the registry
// boundary. Validation against the real JSON Schema compiler lives in the root
// package's contract tests, which can import the jsonschema adapter without a
// dependency cycle.
func TestRetrievalToolSatisfiesToolContract(t *testing.T) {
	retriever, _, _ := seedRetrievalIndex(t, VectorRetrieverConfig{TopK: 5})
	tool, err := NewRetrievalTool(RetrievalToolConfig{ID: "search", Retriever: retriever, TopK: 3})
	if err != nil {
		t.Fatalf("NewRetrievalTool error = %v", err)
	}

	registry, err := NewToolRegistry(stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		return stubCompiledSchema{}, nil
	}})
	if err != nil {
		t.Fatalf("NewToolRegistry error = %v", err)
	}
	if err := registry.Register(tool); err != nil {
		t.Fatalf("Register error = %v", err)
	}

	result := registry.Execute(context.Background(), "search", ToolExecutionRequest{
		Arguments: json.RawMessage(`{"query":"invoices and payment"}`),
	})
	if result.State != ToolExecutionSucceeded {
		t.Fatalf("State = %q (err = %v), want %q", result.State, result.Err, ToolExecutionSucceeded)
	}
	if !strings.Contains(string(result.Output), `"chunks"`) {
		t.Fatalf("Output = %s, want a chunks envelope", result.Output)
	}
}

// TestVectorRetrieverRejectsWrongQueryDimension keeps a provider defect classified
// as an embedding failure rather than surfacing as a retrieval failure once the
// store rejects the width. The indexer already does this on the write path.
func TestVectorRetrieverRejectsWrongQueryDimension(t *testing.T) {
	retriever, embedder, _ := seedRetrievalIndex(t, VectorRetrieverConfig{})
	// Right count, wrong width for the 16-dimension index.
	embedder.vectors = [][]float32{{1, 2, 3}}

	_, err := retriever.Retrieve(context.Background(), RetrievalQuery{Query: "anything"})
	if !errors.Is(err, ErrRAGEmbedding) {
		t.Fatalf("Retrieve error = %v, want ErrRAGEmbedding", err)
	}
	if errors.Is(err, ErrRAGRetrieval) {
		t.Fatalf("Retrieve error = %v, want it not to classify as ErrRAGRetrieval", err)
	}
	if !strings.Contains(err.Error(), "dimension") {
		t.Fatalf("error = %q, want it to report the dimension mismatch", err.Error())
	}
}

// spyRetriever records the query it was handed so a test can assert what the
// tool actually forwarded after clamping.
type spyRetriever struct{ last RetrievalQuery }

func (s *spyRetriever) Retrieve(_ context.Context, query RetrievalQuery) ([]RetrievedChunk, error) {
	s.last = query
	return nil, nil
}

// TestRetrievalToolClampsTopKWhenUnconfigured covers the zero-config case: with
// neither TopK nor MaxTopK set, an unbounded model-supplied top_k would otherwise
// pass straight through, defeating the cap MaxTopK documents.
func TestRetrievalToolClampsTopKWhenUnconfigured(t *testing.T) {
	spy := &spyRetriever{}
	tool, err := NewRetrievalTool(RetrievalToolConfig{ID: "search", Retriever: spy})
	if err != nil {
		t.Fatalf("NewRetrievalTool error = %v", err)
	}

	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"x","top_k":1000}`)); err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if spy.last.TopK != DefaultRetrievalTopK {
		t.Fatalf("forwarded TopK = %d, want DefaultRetrievalTopK of %d", spy.last.TopK, DefaultRetrievalTopK)
	}
}

// TestRetrievalToolTopKResolution pins the interaction between the configured
// default, the cap, and a model-supplied value across the whole matrix.
func TestRetrievalToolTopKResolution(t *testing.T) {
	tests := []struct {
		name      string
		topK      int
		maxTopK   int
		requested int
		want      int
	}{
		{name: "unconfigured clamps to package default", requested: 1000, want: DefaultRetrievalTopK},
		{name: "unconfigured honors a small request", requested: 2, want: 2},
		{name: "configured TopK caps the request", topK: 3, requested: 1000, want: 3},
		{name: "explicit MaxTopK caps the request", maxTopK: 7, requested: 1000, want: 7},
		{name: "MaxTopK wins over TopK for a request", topK: 3, maxTopK: 7, requested: 1000, want: 7},
		{name: "no request uses configured TopK", topK: 3, maxTopK: 7, want: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spy := &spyRetriever{}
			tool, err := NewRetrievalTool(RetrievalToolConfig{
				ID:        "search",
				Retriever: spy,
				TopK:      test.topK,
				MaxTopK:   test.maxTopK,
			})
			if err != nil {
				t.Fatalf("NewRetrievalTool error = %v", err)
			}

			arguments := `{"query":"x"}`
			if test.requested > 0 {
				arguments = fmt.Sprintf(`{"query":"x","top_k":%d}`, test.requested)
			}
			if _, err := tool.Execute(context.Background(), json.RawMessage(arguments)); err != nil {
				t.Fatalf("Execute error = %v", err)
			}
			if spy.last.TopK != test.want {
				t.Fatalf("forwarded TopK = %d, want %d", spy.last.TopK, test.want)
			}
		})
	}
}
