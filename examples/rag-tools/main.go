// Command rag-tools registers bounded vector, document, and graph RAG tools.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

type vectorFixture struct{}

func (vectorFixture) Retrieve(_ context.Context, query lebro.RetrievalQuery) ([]lebro.RetrievedChunk, error) {
	return []lebro.RetrievedChunk{{
		Chunk: lebro.Chunk{ID: "handbook#0", DocumentID: "handbook", Content: query.Query, Index: 0},
		Score: 1,
	}}, nil
}

type graphFixture struct{}

func (graphFixture) RetrieveGraph(_ context.Context, query lebro.GraphRetrievalQuery) ([]lebro.GraphNode, error) {
	return []lebro.GraphNode{{ID: "policy", Content: fmt.Sprintf("depth %d: %s", query.MaxDepth, query.Query)}}, nil
}

func main() {
	registry, err := lebro.NewToolRegistry(lebrojsonschema.NewCompiler())
	if err != nil {
		log.Fatal(err)
	}
	chunker, err := lebro.NewCharacterChunker(lebro.CharacterChunkerConfig{Size: 200})
	if err != nil {
		log.Fatal(err)
	}
	tools := []lebro.Tool{
		must(lebro.NewVectorQueryTool(lebro.VectorQueryToolConfig{ID: "search_handbook", Retriever: vectorFixture{}})),
		must(lebro.NewDocumentChunkerTool(lebro.DocumentChunkerToolConfig{ID: "chunk_handbook", Chunker: chunker, Document: lebro.Document{ID: "handbook", Content: "Refunds are available for 30 days."}})),
		must(lebro.NewGraphRetrievalTool(lebro.GraphRetrievalToolConfig{ID: "search_policy_graph", Retriever: graphFixture{}, MaxDepth: 2, MaxResults: 5})),
	}
	for _, tool := range tools {
		if err := registry.Register(tool); err != nil {
			log.Fatal(err)
		}
	}

	result := registry.Execute(context.Background(), "search_policy_graph", lebro.ToolExecutionRequest{Arguments: json.RawMessage(`{"query":"refund policy","max_depth":9}`)})
	if result.Err != nil {
		log.Fatal(result.Err)
	}
	fmt.Println(string(result.Output)) // max_depth is clamped to 2.
}

func must(tool lebro.Tool, err error) lebro.Tool {
	if err != nil {
		log.Fatal(err)
	}
	return tool
}
