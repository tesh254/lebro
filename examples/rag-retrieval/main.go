// rag-retrieval demonstrates the retrieval-augmented generation contracts:
// documents are chunked, embedded, and indexed, then exposed to an agent as an
// ordinary schema-backed tool that the agent selects inside the existing bounded
// tool loop.
//
// The embedding model here is a deterministic local stand-in so the example runs
// with no API key. Swap it for openai.NewEmbedder to use a real provider; the
// rest of the pipeline is unchanged, because nothing below depends on which
// EmbeddingModel or VectorStore is in use.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

func main() {
	ctx := context.Background()

	// 1. Build the ingestion pipeline: chunk, embed, persist.
	chunker := mustValue(lebro.NewCharacterChunker(lebro.CharacterChunkerConfig{
		Size:    160,
		Overlap: 32,
	}))
	embeddings := localEmbedder{dimension: 64}
	store := lebro.NewMemoryVectorStore()

	indexer := mustValue(lebro.NewIndexer(lebro.IndexerConfig{
		Chunker:    chunker,
		Embeddings: embeddings,
		Store:      store,
		Index:      "handbook",
	}))
	must(indexer.EnsureIndex(ctx))

	// 2. Ingest documents. Metadata is filterable at retrieval time; the
	// visibility key below is what scopes the agent's reach in step 4.
	documents := []lebro.Document{
		{
			ID:       "refunds",
			Content:  "Refund policy: customers may request a full refund within 30 days of purchase. Refunds are issued to the original payment method and appear within five business days.",
			Source:   "policies/refunds.md",
			Metadata: json.RawMessage(`{"visibility":"public"}`),
		},
		{
			ID:       "shipping",
			Content:  "Shipping policy: standard delivery takes five business days. Express delivery arrives the next business day for orders placed before 2pm.",
			Source:   "policies/shipping.md",
			Metadata: json.RawMessage(`{"visibility":"public"}`),
		},
		{
			ID:       "margins",
			Content:  "Internal only: supplier pricing and margin targets are confidential and must never be shared with customers.",
			Source:   "internal/margins.md",
			Metadata: json.RawMessage(`{"visibility":"internal"}`),
		},
	}
	for _, document := range documents {
		result := mustValue(indexer.Ingest(ctx, document))
		fmt.Printf("indexed %s: %d chunk(s)\n", result.DocumentID, result.Chunks)
	}

	// 3. Retrieve directly. The retriever embeds the query itself, so callers
	// never handle vectors.
	retriever := mustValue(lebro.NewVectorRetriever(lebro.VectorRetrieverConfig{
		Embeddings: embeddings,
		Store:      store,
		Index:      "handbook",
		TopK:       2,
		Filter: lebro.VectorMetadataFilter{
			Match: map[string]json.RawMessage{"visibility": json.RawMessage(`"public"`)},
		},
	}))

	hits := mustValue(retriever.Retrieve(ctx, lebro.RetrievalQuery{
		Query: "how long do I have to request a refund",
	}))
	fmt.Printf("\nretrieved %d chunk(s):\n", len(hits))
	for _, hit := range hits {
		fmt.Printf("  %s (score=%.4f, source=%s)\n", hit.ID, hit.Score, hit.Source)
	}

	// 4. Expose retrieval as an ordinary tool and let an agent use it. The
	// configured filter is fixed here, so the agent cannot reach the internal
	// document no matter what it asks for.
	tool := mustValue(lebro.NewRetrievalTool(lebro.RetrievalToolConfig{
		ID:          "search_handbook",
		Retriever:   retriever,
		Description: "Search the customer handbook for passages relevant to a question.",
		TopK:        2,
		MaxTopK:     4,
	}))

	registry := mustValue(lebro.NewToolRegistry(lebrojsonschema.NewCompiler()))
	must(registry.Register(tool))

	agent := mustValue(lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:           "support",
			Instructions: "Answer customer questions using the handbook.",
			Tools:        []lebro.ToolID{"search_handbook"},
		},
		Model:    &scriptedModel{toolID: "search_handbook"},
		Tools:    registry,
		MaxSteps: 4,
	}))

	result := mustValue(agent.Run(ctx, lebro.RunInput{
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "What is the refund window?"}},
	}))

	fmt.Printf("\nagent run %s: %s\n", result.ID, result.Status)
	for _, message := range result.Messages {
		content := message.Content
		if content == "" && !message.ToolCalls.IsZero() {
			content = "(requested retrieval)"
		}
		if len(content) > 96 {
			content = content[:96] + "..."
		}
		fmt.Printf("  %-9s %s\n", message.Role, content)
	}
}

// localEmbedder is a deterministic EmbeddingModel used so the example needs no
// API key. It hashes text into a fixed-width vector: identical text yields
// identical vectors and overlapping text yields similar ones, which is enough to
// demonstrate retrieval. It is not a substitute for a real embedding model.
type localEmbedder struct{ dimension int }

func (e localEmbedder) Dimension() int { return e.dimension }

func (e localEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	vectors := make([][]float32, len(inputs))
	for i, input := range inputs {
		vector := make([]float32, e.dimension)
		// Accumulate per-word buckets so documents sharing vocabulary score
		// higher against a query than unrelated ones.
		for _, word := range strings.Fields(strings.ToLower(input)) {
			bucket := 0
			for _, b := range []byte(word) {
				bucket = (bucket*31 + int(b)) % e.dimension
			}
			vector[bucket] += 1
		}
		// Guarantee a non-zero magnitude so cosine similarity is defined.
		vector[0] += 0.01
		vectors[i] = vector
	}
	return vectors, nil
}

// scriptedModel stands in for a provider: it requests retrieval on the first
// turn, then answers from the tool result. A real model makes the same two
// calls through the same bounded loop.
type scriptedModel struct {
	toolID lebro.ToolID
	calls  int
}

func (m *scriptedModel) Generate(_ context.Context, _ lebro.ModelRequest) (lebro.ModelResponse, error) {
	m.calls++
	if m.calls == 1 {
		calls, err := lebro.NewModelToolCalls(lebro.ModelToolCall{
			ID:        "call-1",
			ToolID:    m.toolID,
			Arguments: json.RawMessage(`{"query":"refund window"}`),
		})
		if err != nil {
			return lebro.ModelResponse{}, err
		}
		return lebro.ModelResponse{
			Message:      lebro.Message{Role: lebro.RoleAssistant, ToolCalls: calls},
			FinishReason: lebro.FinishReasonToolCalls,
		}, nil
	}
	return lebro.ModelResponse{
		Message:      lebro.Message{Role: lebro.RoleAssistant, Content: "You may request a full refund within 30 days of purchase."},
		FinishReason: lebro.FinishReasonStop,
	}, nil
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func mustValue[T any](value T, err error) T {
	must(err)
	return value
}
