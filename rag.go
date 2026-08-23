package lebro

import "github.com/tesh254/lebro/internal/runtime"

func NewCharacterChunker(config CharacterChunkerConfig) (*CharacterChunker, error) {
	return runtime.NewCharacterChunker(config)
}

// NewRecursiveChunker creates a structural chunker that prefers configured
// separators before falling back to rune boundaries.
func NewRecursiveChunker(config RecursiveChunkerConfig) (*RecursiveChunker, error) {
	return runtime.NewRecursiveChunker(config)
}

// NewSlidingWindowChunker creates a fixed-width, optionally overlapping
// rune-window chunker.
func NewSlidingWindowChunker(config SlidingWindowChunkerConfig) (*SlidingWindowChunker, error) {
	return runtime.NewSlidingWindowChunker(config)
}

func NewIndexer(config IndexerConfig) (*Indexer, error) { return runtime.NewIndexer(config) }
func NewVectorRetriever(config VectorRetrieverConfig) (*VectorRetriever, error) {
	return runtime.NewVectorRetriever(config)
}

// NewScorerReranker adapts a provider-neutral relevance scorer for vector retrieval.
func NewScorerReranker(scorer RelevanceScorer) (*ScorerReranker, error) {
	return runtime.NewScorerReranker(scorer)
}
func NewThreadHistory(config ThreadHistoryConfig) (*ThreadHistory, error) {
	return runtime.NewThreadHistory(config)
}
func NewRetrievalTool(config RetrievalToolConfig) (*RetrievalTool, error) {
	return runtime.NewRetrievalTool(config)
}

// NewVectorQueryTool exposes scoped vector retrieval through ToolRegistry.
func NewVectorQueryTool(config VectorQueryToolConfig) (*VectorQueryTool, error) {
	return runtime.NewVectorQueryTool(config)
}

// NewDocumentChunkerTool exposes configured document chunking through ToolRegistry.
func NewDocumentChunkerTool(config DocumentChunkerToolConfig) (*DocumentChunkerTool, error) {
	return runtime.NewDocumentChunkerTool(config)
}

// NewGraphRetrievalTool exposes bounded configured graph retrieval through ToolRegistry.
func NewGraphRetrievalTool(config GraphRetrievalToolConfig) (*GraphRetrievalTool, error) {
	return runtime.NewGraphRetrievalTool(config)
}

const (
	// DefaultGraphTraversalDepth is the default maximum edge depth for graph tools.
	DefaultGraphTraversalDepth = runtime.DefaultGraphTraversalDepth
	// DefaultGraphTraversalResults is the default maximum node count for graph tools.
	DefaultGraphTraversalResults = runtime.DefaultGraphTraversalResults
)

func ChunkID(documentID string, index int) string { return runtime.ChunkID(documentID, index) }
