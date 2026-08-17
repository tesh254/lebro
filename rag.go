package lebro

import "github.com/tesh254/lebro/internal/runtime"

func NewCharacterChunker(config CharacterChunkerConfig) (*CharacterChunker, error) {
	return runtime.NewCharacterChunker(config)
}

func NewIndexer(config IndexerConfig) (*Indexer, error) { return runtime.NewIndexer(config) }
func NewVectorRetriever(config VectorRetrieverConfig) (*VectorRetriever, error) {
	return runtime.NewVectorRetriever(config)
}
func NewThreadHistory(config ThreadHistoryConfig) (*ThreadHistory, error) {
	return runtime.NewThreadHistory(config)
}
func NewRetrievalTool(config RetrievalToolConfig) (*RetrievalTool, error) {
	return runtime.NewRetrievalTool(config)
}
func ChunkID(documentID string, index int) string { return runtime.ChunkID(documentID, index) }
