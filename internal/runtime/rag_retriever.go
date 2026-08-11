package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// DefaultRetrievalTopK is the result count used when neither a
// VectorRetrieverConfig nor a query specifies one.
const DefaultRetrievalTopK = 5

// VectorRetrieverConfig describes a semantic retriever over a vector index.
// Embeddings, Store, and Index are required.
type VectorRetrieverConfig struct {
	// Embeddings embeds the query text. It must be the same model used to index
	// the chunks: vectors from different models are not comparable, and a
	// mismatch yields meaningless scores rather than an error.
	Embeddings EmbeddingModel
	// Store holds the indexed chunks.
	Store VectorStore
	// Index is the vector index searched by this retriever.
	Index string
	// TopK is the default result count for queries that do not set one. A zero
	// value uses DefaultRetrievalTopK.
	TopK int
	// MinScore is the default cosine-similarity floor for queries that do not
	// set one. It must be in [0, 1]; zero admits every result.
	MinScore float32
	// Filter is applied to every query in addition to the query's own filter.
	// Use it to scope a retriever to a tenant, collection, or document set.
	Filter VectorMetadataFilter
}

// VectorRetriever answers semantic queries by embedding the query text and
// searching a vector index. It is safe for concurrent use if its collaborators
// are.
//
// The zero value is not usable; construct one with NewVectorRetriever.
type VectorRetriever struct {
	embeddings EmbeddingModel
	store      VectorStore
	index      string
	topK       int
	minScore   float32
	filter     VectorMetadataFilter
}

var _ Retriever = (*VectorRetriever)(nil)

// NewVectorRetriever validates the configuration and returns a retriever.
func NewVectorRetriever(config VectorRetrieverConfig) (*VectorRetriever, error) {
	if config.Embeddings == nil || isNilInterface(config.Embeddings) {
		return nil, errors.New("lebro: retriever embedding model is required")
	}
	if config.Store == nil || isNilInterface(config.Store) {
		return nil, errors.New("lebro: retriever vector store is required")
	}
	if config.Index == "" {
		return nil, errors.New("lebro: retriever index name is required")
	}
	if config.TopK < 0 {
		return nil, errors.New("lebro: retriever TopK must not be negative")
	}
	if config.MinScore < 0 || config.MinScore > 1 || isNaN(config.MinScore) {
		return nil, errors.New("lebro: retriever MinScore must be in [0, 1]")
	}

	topK := config.TopK
	if topK == 0 {
		topK = DefaultRetrievalTopK
	}
	return &VectorRetriever{
		embeddings: config.Embeddings,
		store:      config.Store,
		index:      config.Index,
		topK:       topK,
		minScore:   config.MinScore,
		filter:     cloneVectorMetadataFilter(config.Filter),
	}, nil
}

// Retrieve embeds the query and returns the most similar chunks, ordered by
// descending score.
//
// The retriever's configured filter is merged with the query's filter. On a key
// collision the retriever's value wins, so a caller cannot escape a
// configured scope by naming the same key — this is what lets a retrieval tool
// be handed to a model safely.
func (r *VectorRetriever) Retrieve(ctx context.Context, query RetrievalQuery) ([]RetrievedChunk, error) {
	if r == nil {
		return nil, errors.New("lebro: vector retriever is nil")
	}
	if ctx == nil {
		return nil, errors.New("lebro: retriever context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := query.Validate(); err != nil {
		return nil, &RAGError{Kind: RAGErrorRetrieval, Err: err}
	}

	topK := query.TopK
	if topK == 0 {
		topK = r.topK
	}
	minScore := query.MinScore
	if minScore == 0 {
		minScore = r.minScore
	}

	vectors, err := r.embeddings.Embed(ctx, []string{query.Query})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, &RAGError{Kind: RAGErrorEmbedding, Err: err}
	}
	if len(vectors) != 1 {
		return nil, &RAGError{
			Kind: RAGErrorEmbedding,
			Err:  fmt.Errorf("lebro: embedding model returned %d vectors for 1 input", len(vectors)),
		}
	}
	// Check the width here rather than letting the store reject it, so a
	// provider defect is reported as an embedding failure instead of surfacing
	// as a retrieval failure. This matches what the indexer already does.
	if dimension := r.embeddings.Dimension(); len(vectors[0]) != dimension {
		return nil, &RAGError{
			Kind: RAGErrorEmbedding,
			Err:  fmt.Errorf("lebro: query embedding has dimension %d, want %d", len(vectors[0]), dimension),
		}
	}

	results, err := r.store.Search(ctx, SimilarityQuery{
		Vector:   vectors[0],
		Index:    r.index,
		Filter:   mergeVectorMetadataFilters(query.Filter, r.filter),
		TopK:     topK,
		MinScore: minScore,
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, &RAGError{Kind: RAGErrorRetrieval, Err: err}
	}

	retrieved := make([]RetrievedChunk, 0, len(results))
	for _, result := range results {
		chunk, err := chunkFromMetadata(result)
		if err != nil {
			return nil, &RAGError{Kind: RAGErrorRetrieval, Err: err}
		}
		retrieved = append(retrieved, RetrievedChunk{Chunk: chunk, Score: result.Score})
	}
	return retrieved, nil
}

// mergeVectorMetadataFilters combines a caller filter with an enforced filter.
// The enforced filter is applied last so its keys cannot be overridden.
func mergeVectorMetadataFilters(caller, enforced VectorMetadataFilter) VectorMetadataFilter {
	if len(caller.Match) == 0 && len(enforced.Match) == 0 {
		return VectorMetadataFilter{}
	}
	merged := make(map[string]json.RawMessage, len(caller.Match)+len(enforced.Match))
	for key, value := range caller.Match {
		merged[key] = cloneRawMessage(value)
	}
	for key, value := range enforced.Match {
		merged[key] = cloneRawMessage(value)
	}
	return VectorMetadataFilter{Match: merged}
}

// cloneVectorMetadataFilter deep-copies a filter so a caller cannot mutate a
// retriever's or tool's enforced scope after construction.
func cloneVectorMetadataFilter(filter VectorMetadataFilter) VectorMetadataFilter {
	if len(filter.Match) == 0 {
		return VectorMetadataFilter{}
	}
	cloned := make(map[string]json.RawMessage, len(filter.Match))
	for key, value := range filter.Match {
		cloned[key] = cloneRawMessage(value)
	}
	return VectorMetadataFilter{Match: cloned}
}

// retrievalToolInputSchema is the default retrieval contract. A model states a
// natural-language query and may narrow the result count.
//
// Notably absent: any filter or index field. Retrieval scope is configuration,
// not something a model chooses, so a model cannot widen what it is allowed to
// read by changing what it sends.
const retrievalToolInputSchema = `{
	"type":"object",
	"required":["query"],
	"properties":{
		"query":{"type":"string","description":"The natural-language question to search for."},
		"top_k":{"type":"integer","minimum":1,"description":"Optional maximum number of chunks to return."}
	},
	"additionalProperties":false
}`

// retrievalToolOutputSchema is the default retrieval result. Every hit reports
// its provenance alongside its text so a model can cite what it used.
const retrievalToolOutputSchema = `{
	"type":"object",
	"required":["chunks"],
	"properties":{
		"chunks":{
			"type":"array",
			"items":{
				"type":"object",
				"required":["content","document_id","chunk_index","score"],
				"properties":{
					"content":{"type":"string"},
					"document_id":{"type":"string"},
					"source":{"type":"string"},
					"chunk_index":{"type":"integer"},
					"score":{"type":"number"}
				},
				"additionalProperties":false
			}
		}
	},
	"additionalProperties":false
}`

// retrievalToolInput is the decoded retrieval contract.
type retrievalToolInput struct {
	Query string `json:"query"`
	TopK  int    `json:"top_k,omitempty"`
}

// retrievalToolOutput is the retrieval result returned to the model.
type retrievalToolOutput struct {
	Chunks []retrievalToolChunk `json:"chunks"`
}

// retrievalToolChunk is one hit as the model sees it. Application metadata is
// deliberately not included: it is filterable at configuration time but is not
// necessarily meant for the model's context.
type retrievalToolChunk struct {
	Content    string  `json:"content"`
	DocumentID string  `json:"document_id"`
	Source     string  `json:"source,omitempty"`
	ChunkIndex int     `json:"chunk_index"`
	Score      float32 `json:"score"`
}

// RetrievalToolConfig describes a retrieval capability exposed to a model. ID
// and Retriever are required.
type RetrievalToolConfig struct {
	// ID is the stable tool ID the model uses to select retrieval.
	ID ToolID
	// Retriever answers the queries. Any Retriever implementation works.
	Retriever Retriever
	// Description tells the model when to retrieve. It defaults to a generic
	// description; a specific one — naming the corpus — materially improves
	// selection, so applications should set it.
	Description string
	// TopK is the result count used when the model does not request one. A zero
	// value defers to the retriever's own default.
	TopK int
	// MaxTopK caps a model-supplied top_k, so a model cannot enlarge its own
	// context window beyond what the application budgeted. A zero value falls
	// back to TopK, or to DefaultRetrievalTopK when TopK is also unset, so the
	// cap holds even for a tool configured with neither. Requests above the cap
	// are clamped rather than rejected: a clamped retrieval is more useful to a
	// model than a validation error.
	MaxTopK int
	// MinScore is the cosine-similarity floor applied to every query. It must be
	// in [0, 1].
	MinScore float32
	// Filter scopes every retrieval this tool performs. It is fixed at
	// construction and is not model-settable.
	Filter VectorMetadataFilter
	// InputSchema overrides the default retrieval contract. When set, the
	// payload must still decode into a query and optional top_k.
	InputSchema json.RawMessage
	// OutputSchema overrides the default result schema. When set, it must accept
	// the chunks envelope this adapter produces.
	OutputSchema json.RawMessage
}

// RetrievalTool exposes semantic retrieval as an ordinary schema-backed Tool.
//
// It implements Tool and nothing more: registering one in a ToolRegistry gives
// retrieval the same execution boundary as any other tool, and an agent selects
// it through ordinary model tool-calling inside the existing bounded loop.
// There is no implicit context injection, no automatic pre-run retrieval, and
// no hidden transcript rewriting — if a model does not call the tool, no
// retrieval happens.
//
// Retrieval scope is configuration: the metadata filter and the result-count
// cap are fixed at construction, so a model can choose what to search for but
// not what it is allowed to see.
//
// The zero value is not usable; construct one with NewRetrievalTool.
type RetrievalTool struct {
	id         ToolID
	retriever  Retriever
	definition ToolDefinition
	topK       int
	maxTopK    int
	minScore   float32
	filter     VectorMetadataFilter
}

var _ Tool = (*RetrievalTool)(nil)

// NewRetrievalTool validates the configuration and returns a retrieval
// capability safe for concurrent use.
func NewRetrievalTool(config RetrievalToolConfig) (*RetrievalTool, error) {
	if config.ID == "" {
		return nil, errors.New("lebro: retrieval tool ID is required")
	}
	if config.Retriever == nil || isNilInterface(config.Retriever) {
		return nil, errors.New("lebro: retrieval tool retriever is required")
	}
	if config.TopK < 0 {
		return nil, errors.New("lebro: retrieval tool TopK must not be negative")
	}
	if config.MaxTopK < 0 {
		return nil, errors.New("lebro: retrieval tool MaxTopK must not be negative")
	}
	if config.MaxTopK > 0 && config.TopK > config.MaxTopK {
		return nil, fmt.Errorf("lebro: retrieval tool TopK %d must not exceed MaxTopK %d", config.TopK, config.MaxTopK)
	}
	if config.MinScore < 0 || config.MinScore > 1 || isNaN(config.MinScore) {
		return nil, errors.New("lebro: retrieval tool MinScore must be in [0, 1]")
	}

	inputSchema := config.InputSchema
	if len(inputSchema) == 0 {
		inputSchema = json.RawMessage(retrievalToolInputSchema)
	} else if !json.Valid(inputSchema) {
		return nil, errors.New("lebro: retrieval tool input schema must be valid JSON")
	}
	outputSchema := config.OutputSchema
	if len(outputSchema) == 0 {
		outputSchema = json.RawMessage(retrievalToolOutputSchema)
	} else if !json.Valid(outputSchema) {
		return nil, errors.New("lebro: retrieval tool output schema must be valid JSON")
	}

	description := config.Description
	if description == "" {
		description = "Search the indexed document corpus for passages relevant to a natural-language query."
	}

	// An unset cap falls back to the configured default, and then to the package
	// default, so a model-supplied top_k can never exceed the application's own
	// budget. Leaving it at zero would make resolveTopK pass any requested count
	// straight through, which is the opposite of what MaxTopK promises.
	maxTopK := config.MaxTopK
	if maxTopK == 0 {
		maxTopK = config.TopK
	}
	if maxTopK == 0 {
		maxTopK = DefaultRetrievalTopK
	}

	return &RetrievalTool{
		id:        config.ID,
		retriever: config.Retriever,
		definition: ToolDefinition{
			ID:           config.ID,
			Description:  description,
			InputSchema:  cloneRawMessage(inputSchema),
			OutputSchema: cloneRawMessage(outputSchema),
		},
		topK:     config.TopK,
		maxTopK:  maxTopK,
		minScore: config.MinScore,
		filter:   cloneVectorMetadataFilter(config.Filter),
	}, nil
}

// Definition returns a caller-owned copy of the retrieval tool's definition.
func (t *RetrievalTool) Definition() ToolDefinition {
	if t == nil {
		return ToolDefinition{}
	}
	return cloneToolDefinition(t.definition)
}

// Execute runs one retrieval and returns the matching chunks.
//
// Cancellation is returned as the bare context error so the registry boundary
// classifies it as ToolExecutionCancelled rather than a handler failure. An
// empty result set is a success with an empty chunks array, not an error: "no
// relevant passages" is a useful answer for a model to reason about.
func (t *RetrievalTool) Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	if t == nil || t.retriever == nil || isNilInterface(t.retriever) {
		return nil, errors.New("lebro: retrieval tool is nil")
	}
	if ctx == nil {
		return nil, errors.New("lebro: retrieval tool context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var decoded retrievalToolInput
	if err := json.Unmarshal(input, &decoded); err != nil {
		return nil, &RAGError{Kind: RAGErrorRetrieval, Err: fmt.Errorf("lebro: decode retrieval input: %w", err)}
	}
	if decoded.Query == "" {
		return nil, &RAGError{Kind: RAGErrorRetrieval, Err: errors.New("lebro: retrieval query must not be empty")}
	}
	if decoded.TopK < 0 {
		return nil, &RAGError{Kind: RAGErrorRetrieval, Err: errors.New("lebro: retrieval top_k must not be negative")}
	}

	results, err := t.retriever.Retrieve(ctx, RetrievalQuery{
		Query:    decoded.Query,
		TopK:     t.resolveTopK(decoded.TopK),
		MinScore: t.minScore,
		Filter:   cloneVectorMetadataFilter(t.filter),
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, err
	}

	output := retrievalToolOutput{Chunks: make([]retrievalToolChunk, 0, len(results))}
	for _, result := range results {
		output.Chunks = append(output.Chunks, retrievalToolChunk{
			Content:    result.Content,
			DocumentID: result.DocumentID,
			Source:     result.Source,
			ChunkIndex: result.Index,
			Score:      result.Score,
		})
	}

	encoded, err := json.Marshal(output)
	if err != nil {
		return nil, &RAGError{Kind: RAGErrorRetrieval, Err: fmt.Errorf("lebro: encode retrieval result: %w", err)}
	}
	return encoded, nil
}

// resolveTopK picks the effective result count for one call, clamping a
// model-supplied value to the configured cap.
func (t *RetrievalTool) resolveTopK(requested int) int {
	if requested <= 0 {
		return t.topK
	}
	if t.maxTopK > 0 && requested > t.maxTopK {
		return t.maxTopK
	}
	return requested
}
