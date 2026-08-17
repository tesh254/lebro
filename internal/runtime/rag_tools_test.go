package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type graphRetrieverSpy struct {
	query GraphRetrievalQuery
	nodes []GraphNode
	err   error
}

func (s *graphRetrieverSpy) RetrieveGraph(_ context.Context, query GraphRetrievalQuery) ([]GraphNode, error) {
	s.query = query
	return s.nodes, s.err
}

func TestDocumentChunkerToolUsesFixedDocument(t *testing.T) {
	chunker, err := NewCharacterChunker(CharacterChunkerConfig{Size: 3})
	if err != nil {
		t.Fatalf("NewCharacterChunker() error = %v", err)
	}
	tool, err := NewDocumentChunkerTool(DocumentChunkerToolConfig{ID: "chunk", Chunker: chunker, Document: Document{ID: "doc", Content: "abcdef", Source: "fixture"}})
	if err != nil {
		t.Fatalf("NewDocumentChunkerTool() error = %v", err)
	}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var got struct {
		Chunks []documentChunkerToolChunk `json:"chunks"`
	}
	if err := json.Unmarshal(output, &got); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if len(got.Chunks) != 2 || got.Chunks[0].ID != "doc#0" || got.Chunks[1].Content != "def" {
		t.Fatalf("chunks = %+v, want fixed document chunks", got.Chunks)
	}
}

func TestGraphRetrievalToolClampsAndTruncates(t *testing.T) {
	spy := &graphRetrieverSpy{nodes: []GraphNode{{ID: "a", Content: "A"}, {ID: "b", Content: "B"}, {ID: "c", Content: "C"}}}
	tool, err := NewGraphRetrievalTool(GraphRetrievalToolConfig{ID: "graph", Retriever: spy, MaxDepth: 2, MaxResults: 2})
	if err != nil {
		t.Fatalf("NewGraphRetrievalTool() error = %v", err)
	}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"find","max_depth":99,"max_results":99}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if spy.query.MaxDepth != 2 || spy.query.MaxResults != 2 {
		t.Fatalf("retriever query = %+v, want capped values", spy.query)
	}
	var got struct {
		Nodes []GraphNode `json:"nodes"`
	}
	if err := json.Unmarshal(output, &got); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if len(got.Nodes) != 2 || got.Nodes[0].ID != "a" || got.Nodes[1].ID != "b" {
		t.Fatalf("output nodes = %+v, want first two bounded nodes", got.Nodes)
	}
}

func TestGraphRetrievalToolRejectsUnsafeConfiguration(t *testing.T) {
	spy := &graphRetrieverSpy{}
	for _, config := range []GraphRetrievalToolConfig{
		{ID: "graph", Retriever: spy, MaxDepth: -1},
		{ID: "graph", Retriever: spy, MaxResults: -1},
		{ID: "graph", Retriever: spy, InputSchema: json.RawMessage(`not-json`)},
		{ID: "graph", Retriever: spy, OutputSchema: json.RawMessage(`not-json`)},
	} {
		if _, err := NewGraphRetrievalTool(config); err == nil {
			t.Fatalf("NewGraphRetrievalTool(%+v) error = nil", config)
		}
	}
}

func TestRAGToolsPreserveCancellationAndWrapGraphFailures(t *testing.T) {
	chunker, err := NewCharacterChunker(CharacterChunkerConfig{Size: 3})
	if err != nil {
		t.Fatalf("NewCharacterChunker() error = %v", err)
	}
	chunkTool, err := NewDocumentChunkerTool(DocumentChunkerToolConfig{ID: "chunk", Chunker: chunker, Document: Document{ID: "doc", Content: "abc"}})
	if err != nil {
		t.Fatalf("NewDocumentChunkerTool() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := chunkTool.Execute(ctx, json.RawMessage(`{}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("document chunker cancellation error = %v", err)
	}

	cause := errors.New("store unavailable")
	graphTool, err := NewGraphRetrievalTool(GraphRetrievalToolConfig{ID: "graph", Retriever: &graphRetrieverSpy{err: cause}})
	if err != nil {
		t.Fatalf("NewGraphRetrievalTool() error = %v", err)
	}
	if _, err := graphTool.Execute(context.Background(), json.RawMessage(`{"query":"find"}`)); !errors.Is(err, ErrRAGGraphTraversal) || !errors.Is(err, cause) {
		t.Fatalf("graph failure error = %v, want graph sentinel and cause", err)
	}
	if _, err := graphTool.Execute(ctx, json.RawMessage(`{"query":"find"}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("graph cancellation error = %v", err)
	}
}
