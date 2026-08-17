package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// VectorQueryTool and VectorQueryToolConfig name the vector-query factory in
// the public RAG vocabulary while preserving RetrievalTool compatibility.
type VectorQueryTool = RetrievalTool
type VectorQueryToolConfig = RetrievalToolConfig

// NewVectorQueryTool creates a schema-backed vector retrieval tool. It has the
// same fixed-scope and result-cap guarantees as NewRetrievalTool.
func NewVectorQueryTool(config VectorQueryToolConfig) (*VectorQueryTool, error) {
	return NewRetrievalTool(config)
}

const documentChunkerToolInputSchema = `{
	"type":"object",
	"additionalProperties":false
}`

const documentChunkerToolOutputSchema = `{
	"type":"object",
	"required":["chunks"],
	"properties":{"chunks":{"type":"array","items":{"type":"object","required":["id","document_id","content","chunk_index"],"properties":{"id":{"type":"string"},"document_id":{"type":"string"},"content":{"type":"string"},"source":{"type":"string"},"chunk_index":{"type":"integer"}},"additionalProperties":false}}},
	"additionalProperties":false
}`

// DocumentChunkerToolConfig describes a fixed document chunking capability.
// Document and Chunker are construction-time configuration so a model cannot
// choose arbitrary data or alter chunking resource behavior.
type DocumentChunkerToolConfig struct {
	ID           ToolID
	Chunker      Chunker
	Document     Document
	Description  string
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
}

// DocumentChunkerTool exposes one configured document and chunker through the
// ordinary ToolRegistry schema boundary.
type DocumentChunkerTool struct {
	chunker    Chunker
	document   Document
	definition ToolDefinition
}

var _ Tool = (*DocumentChunkerTool)(nil)

// NewDocumentChunkerTool validates a fixed document chunking tool.
func NewDocumentChunkerTool(config DocumentChunkerToolConfig) (*DocumentChunkerTool, error) {
	if config.ID == "" {
		return nil, errors.New("lebro: document chunker tool ID is required")
	}
	if config.Chunker == nil || isNilInterface(config.Chunker) {
		return nil, errors.New("lebro: document chunker tool chunker is required")
	}
	if err := config.Document.Validate(); err != nil {
		return nil, fmt.Errorf("lebro: document chunker tool document: %w", err)
	}
	inputSchema, err := toolSchema(config.InputSchema, documentChunkerToolInputSchema, "document chunker tool input")
	if err != nil {
		return nil, err
	}
	outputSchema, err := toolSchema(config.OutputSchema, documentChunkerToolOutputSchema, "document chunker tool output")
	if err != nil {
		return nil, err
	}
	description := config.Description
	if description == "" {
		description = "Split the configured document into stable, provenance-preserving chunks."
	}
	return &DocumentChunkerTool{chunker: config.Chunker, document: cloneDocument(config.Document), definition: ToolDefinition{ID: config.ID, Description: description, InputSchema: inputSchema, OutputSchema: outputSchema}}, nil
}

func (t *DocumentChunkerTool) Definition() ToolDefinition {
	if t == nil {
		return ToolDefinition{}
	}
	return cloneToolDefinition(t.definition)
}

func (t *DocumentChunkerTool) Execute(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	if t == nil || t.chunker == nil || isNilInterface(t.chunker) {
		return nil, errors.New("lebro: document chunker tool is nil")
	}
	if ctx == nil {
		return nil, errors.New("lebro: document chunker tool context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	chunks, err := t.chunker.Chunk(ctx, cloneDocument(t.document))
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Chunks []documentChunkerToolChunk `json:"chunks"`
	}{Chunks: documentToolChunks(chunks)})
}

type documentChunkerToolChunk struct {
	ID         string `json:"id"`
	DocumentID string `json:"document_id"`
	Content    string `json:"content"`
	Source     string `json:"source,omitempty"`
	ChunkIndex int    `json:"chunk_index"`
}

func documentToolChunks(chunks []Chunk) []documentChunkerToolChunk {
	output := make([]documentChunkerToolChunk, 0, len(chunks))
	for _, chunk := range chunks {
		output = append(output, documentChunkerToolChunk{ID: chunk.ID, DocumentID: chunk.DocumentID, Content: chunk.Content, Source: chunk.Source, ChunkIndex: chunk.Index})
	}
	return output
}

const graphRetrievalToolInputSchema = `{
	"type":"object",
	"required":["query"],
	"properties":{"query":{"type":"string"},"max_depth":{"type":"integer","minimum":1},"max_results":{"type":"integer","minimum":1}},
	"additionalProperties":false
}`

const graphRetrievalToolOutputSchema = `{
	"type":"object",
	"required":["nodes"],
	"properties":{"nodes":{"type":"array","items":{"type":"object","required":["id","content"],"properties":{"id":{"type":"string"},"content":{"type":"string"}},"additionalProperties":false}}},
	"additionalProperties":false
}`

// GraphRetrievalToolConfig describes a graph-retrieval capability. Caps are
// construction-time policy; requests can narrow them but never increase them.
type GraphRetrievalToolConfig struct {
	ID           ToolID
	Retriever    GraphRetriever
	Description  string
	MaxDepth     int
	MaxResults   int
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
}

// GraphRetrievalTool exposes bounded graph retrieval through ToolRegistry.
type GraphRetrievalTool struct {
	retriever  GraphRetriever
	maxDepth   int
	maxResults int
	definition ToolDefinition
}

var _ Tool = (*GraphRetrievalTool)(nil)

// NewGraphRetrievalTool validates a bounded graph retrieval tool.
func NewGraphRetrievalTool(config GraphRetrievalToolConfig) (*GraphRetrievalTool, error) {
	if config.ID == "" {
		return nil, errors.New("lebro: graph retrieval tool ID is required")
	}
	if config.Retriever == nil || isNilInterface(config.Retriever) {
		return nil, errors.New("lebro: graph retrieval tool retriever is required")
	}
	maxDepth := config.MaxDepth
	if maxDepth == 0 {
		maxDepth = DefaultGraphTraversalDepth
	}
	if maxDepth < 1 {
		return nil, errors.New("lebro: graph retrieval tool max depth must be at least 1")
	}
	maxResults := config.MaxResults
	if maxResults == 0 {
		maxResults = DefaultGraphTraversalResults
	}
	if maxResults < 1 {
		return nil, errors.New("lebro: graph retrieval tool max results must be at least 1")
	}
	inputSchema, err := toolSchema(config.InputSchema, graphRetrievalToolInputSchema, "graph retrieval tool input")
	if err != nil {
		return nil, err
	}
	outputSchema, err := toolSchema(config.OutputSchema, graphRetrievalToolOutputSchema, "graph retrieval tool output")
	if err != nil {
		return nil, err
	}
	description := config.Description
	if description == "" {
		description = "Search configured graph context with bounded traversal depth and result count."
	}
	return &GraphRetrievalTool{retriever: config.Retriever, maxDepth: maxDepth, maxResults: maxResults, definition: ToolDefinition{ID: config.ID, Description: description, InputSchema: inputSchema, OutputSchema: outputSchema}}, nil
}

func (t *GraphRetrievalTool) Definition() ToolDefinition {
	if t == nil {
		return ToolDefinition{}
	}
	return cloneToolDefinition(t.definition)
}

func (t *GraphRetrievalTool) Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	if t == nil || t.retriever == nil || isNilInterface(t.retriever) {
		return nil, errors.New("lebro: graph retrieval tool is nil")
	}
	if ctx == nil {
		return nil, errors.New("lebro: graph retrieval tool context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var request struct {
		Query      string `json:"query"`
		MaxDepth   int    `json:"max_depth,omitempty"`
		MaxResults int    `json:"max_results,omitempty"`
	}
	if err := json.Unmarshal(input, &request); err != nil {
		return nil, &RAGError{Kind: RAGErrorGraphTraversal, Err: fmt.Errorf("lebro: decode graph retrieval input: %w", err)}
	}
	query := GraphRetrievalQuery{Query: request.Query, MaxDepth: clampPositive(request.MaxDepth, t.maxDepth), MaxResults: clampPositive(request.MaxResults, t.maxResults)}
	if err := query.Validate(); err != nil {
		return nil, &RAGError{Kind: RAGErrorGraphTraversal, Err: err}
	}
	nodes, err := t.retriever.RetrieveGraph(ctx, query)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, &RAGError{Kind: RAGErrorGraphTraversal, Err: err}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(nodes) > query.MaxResults {
		nodes = nodes[:query.MaxResults]
	}
	output := make([]graphRetrievalToolNode, 0, len(nodes))
	for _, node := range nodes {
		if err := node.Validate(); err != nil {
			return nil, &RAGError{Kind: RAGErrorGraphTraversal, Err: err}
		}
		output = append(output, graphRetrievalToolNode{ID: node.ID, Content: node.Content})
	}
	return json.Marshal(struct {
		Nodes []graphRetrievalToolNode `json:"nodes"`
	}{Nodes: output})
}

type graphRetrievalToolNode struct {
	ID      string `json:"id"`
	Content string `json:"content"`
}

// clampPositive resolves omitted or over-cap model values to the configured
// maximum; valid positive values below the maximum are preserved.
func clampPositive(requested, maximum int) int {
	if requested < 1 || requested > maximum {
		return maximum
	}
	return requested
}

func toolSchema(schema json.RawMessage, fallback, name string) (json.RawMessage, error) {
	if len(schema) == 0 {
		return json.RawMessage(fallback), nil
	}
	if !json.Valid(schema) {
		return nil, fmt.Errorf("lebro: %s schema must be valid JSON", name)
	}
	return cloneRawMessage(schema), nil
}

func cloneDocument(document Document) Document {
	document.Metadata = cloneRawMessage(document.Metadata)
	return document
}
