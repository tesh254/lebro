package lebro_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

type toolFixtureRetriever struct{}

func (toolFixtureRetriever) Retrieve(_ context.Context, query lebro.RetrievalQuery) ([]lebro.RetrievedChunk, error) {
	return []lebro.RetrievedChunk{{Chunk: lebro.Chunk{ID: "doc#0", DocumentID: "doc", Content: query.Query, Index: 0}, Score: 1}}, nil
}

type toolFixtureGraphRetriever struct{}

func (toolFixtureGraphRetriever) RetrieveGraph(_ context.Context, query lebro.GraphRetrievalQuery) ([]lebro.GraphNode, error) {
	return []lebro.GraphNode{{ID: "node-1", Content: query.Query}}, nil
}

func TestMAD67RAGToolsRegistrySchemas(t *testing.T) {
	chunker, err := lebro.NewCharacterChunker(lebro.CharacterChunkerConfig{Size: 16})
	if err != nil {
		t.Fatalf("NewCharacterChunker() error = %v", err)
	}
	vector, err := lebro.NewVectorQueryTool(lebro.VectorQueryToolConfig{ID: "vector", Retriever: toolFixtureRetriever{}})
	if err != nil {
		t.Fatalf("NewVectorQueryTool() error = %v", err)
	}
	chunk, err := lebro.NewDocumentChunkerTool(lebro.DocumentChunkerToolConfig{ID: "chunk", Chunker: chunker, Document: lebro.Document{ID: "doc", Content: "fixture"}})
	if err != nil {
		t.Fatalf("NewDocumentChunkerTool() error = %v", err)
	}
	graph, err := lebro.NewGraphRetrievalTool(lebro.GraphRetrievalToolConfig{ID: "graph", Retriever: toolFixtureGraphRetriever{}, MaxDepth: 2, MaxResults: 3})
	if err != nil {
		t.Fatalf("NewGraphRetrievalTool() error = %v", err)
	}
	tools := []lebro.Tool{vector, chunk, graph}
	registry, err := lebro.NewToolRegistry(lebrojsonschema.NewCompiler())
	if err != nil {
		t.Fatalf("NewToolRegistry() error = %v", err)
	}
	for _, tool := range tools {
		if err := registry.Register(tool); err != nil {
			t.Fatalf("Register(%q) error = %v", tool.Definition().ID, err)
		}
	}

	for _, invocation := range []struct {
		id        lebro.ToolID
		arguments string
		state     lebro.ToolExecutionState
	}{
		{id: "vector", arguments: `{"query":"refund"}`, state: lebro.ToolExecutionSucceeded},
		{id: "vector", arguments: `{"query":"refund","filter":{}}`, state: lebro.ToolExecutionInvalidInput},
		{id: "chunk", arguments: `{}`, state: lebro.ToolExecutionSucceeded},
		{id: "chunk", arguments: `{"document":"other"}`, state: lebro.ToolExecutionInvalidInput},
		{id: "graph", arguments: `{"query":"refund","max_depth":2,"max_results":3}`, state: lebro.ToolExecutionSucceeded},
		{id: "graph", arguments: `{"query":"refund","max_depth":0}`, state: lebro.ToolExecutionInvalidInput},
	} {
		result := registry.Execute(context.Background(), invocation.id, lebro.ToolExecutionRequest{Arguments: json.RawMessage(invocation.arguments)})
		if result.State != invocation.state {
			t.Fatalf("Execute(%q, %s) state = %q, want %q (%v)", invocation.id, invocation.arguments, result.State, invocation.state, result.Err)
		}
	}
}
