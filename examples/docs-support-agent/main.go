// docs-support-agent composes the support-bot build out of lebro primitives:
// a handbook indexed through RAG, a fixed-scope retrieval tool that can only
// ever see public documents, an agent bound to durable threads so each
// customer keeps one persisted conversation, and answers that are functions of
// what retrieval actually returned.
//
// The embedder, scorer, and model here are deterministic local stand-ins so
// the example runs with no API key. Swap them for provider adapters; nothing
// below depends on which implementation is in use.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(output io.Writer) error {
	ctx := context.Background()
	store := lebro.NewMemoryStore()

	// 1. Ingest the handbook. The visibility key scopes what the support agent
	// can ever retrieve; the internal document is indexed but unreachable.
	chunker := mustValue(lebro.NewCharacterChunker(lebro.CharacterChunkerConfig{Size: 160, Overlap: 32}))
	embeddings := localEmbedder{dimension: 64}
	vectorStore := lebro.NewMemoryVectorStore()

	indexer := mustValue(lebro.NewIndexer(lebro.IndexerConfig{
		Chunker:    chunker,
		Embeddings: embeddings,
		Store:      vectorStore,
		Index:      "handbook",
	}))
	must(indexer.EnsureIndex(ctx))

	documents := []lebro.Document{
		{
			ID:       "refunds",
			Content:  "Refund policy: customers may request a full refund within 30 days of purchase. Refunds go back to the original payment method within five business days.",
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
		writef(output, "indexed %s (%d chunk(s))\n", result.DocumentID, result.Chunks)
	}

	// 2. Retrieval is exposed as one ordinary tool whose metadata filter is
	// fixed in code. No model input can widen the corpus past public documents,
	// which is what keeps the agent on-corpus.
	retriever := mustValue(lebro.NewVectorRetriever(lebro.VectorRetrieverConfig{
		Embeddings:    embeddings,
		Store:         vectorStore,
		Index:         "handbook",
		TopK:          2,
		CandidateTopK: 4,
		Reranker:      mustValue(lebro.NewScorerReranker(keywordScorer{})),
		Filter: lebro.VectorMetadataFilter{
			Match: map[string]json.RawMessage{"visibility": json.RawMessage(`"public"`)},
		},
	}))
	tool := mustValue(lebro.NewRetrievalTool(lebro.RetrievalToolConfig{
		ID:          "search_handbook",
		Retriever:   retriever,
		Description: "Search the customer handbook for passages relevant to a question.",
		TopK:        2,
		MaxTopK:     4,
	}))
	registry := mustValue(lebro.NewToolRegistry(lebrojsonschema.NewCompiler()))
	must(registry.Register(tool))

	// 3. One shared agent; per-customer state lives in the store under the
	// caller's thread ID, not in the agent.
	model := &scriptedModel{}
	agent := mustValue(lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:           "docs-support",
			Name:         "Docs Support",
			Instructions: "Answer customer questions using only the handbook. If the handbook has nothing, say so.",
			Tools:        []lebro.ToolID{"search_handbook"},
		},
		Model:    model,
		Tools:    registry,
		Store:    store,
		MaxSteps: 4,
	}))

	converse(output, agent, "customer-acme-1", "What is the refund window?")
	converse(output, agent, "customer-acme-1", "How long does standard delivery take?")
	converse(output, agent, "customer-globex-9", "What are your supplier margin targets?")

	// 4. Each customer's exchange persisted to its own durable thread; the two
	// transcripts never mix.
	for _, threadID := range []string{"customer-acme-1", "customer-globex-9"} {
		page, err := store.Messages().ListMessages(ctx, lebro.ThreadID(threadID), lebro.PageRequest{})
		if err != nil {
			return err
		}
		writef(output, "%s persisted messages: %d\n", threadID, len(page.Records))
	}
	return nil
}

// converse runs one customer turn on the given thread and prints the reply.
func converse(output io.Writer, agent *lebro.Agent, threadID, question string) {
	result, err := agent.Run(context.Background(), lebro.RunInput{
		ThreadID: lebro.ThreadID(threadID),
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: question}},
	})
	if err != nil {
		panic(err)
	}
	reply := result.Messages[len(result.Messages)-1].Content
	writef(output, "\n[%s] %s\n[%s] %s\n", threadID, question, threadID, truncate(reply, 110))
}

func writef(output io.Writer, format string, args ...any) {
	if _, err := fmt.Fprintf(output, format, args...); err != nil {
		panic(err)
	}
}

func truncate(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	const ellipsis = "..."
	if limit <= len(ellipsis) {
		return string(runes[:limit])
	}
	return string(runes[:limit-len(ellipsis)]) + ellipsis
}

// localEmbedder is a deterministic stand-in embedding model; identical text
// yields identical vectors and overlapping vocabulary yields similar ones.
type localEmbedder struct{ dimension int }

func (e localEmbedder) Dimension() int { return e.dimension }

func (e localEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	vectors := make([][]float32, len(inputs))
	for i, input := range inputs {
		vector := make([]float32, e.dimension)
		for _, word := range strings.Fields(strings.ToLower(input)) {
			bucket := 0
			for _, b := range []byte(word) {
				bucket = (bucket*31 + int(b)) % e.dimension
			}
			vector[bucket] += 1
		}
		vector[0] += 0.01
		vectors[i] = vector
	}
	return vectors, nil
}

// keywordScorer reranks candidates by query keyword overlap.
type keywordScorer struct{}

func (keywordScorer) Score(ctx context.Context, query string, candidate lebro.Chunk) (float32, string, error) {
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	var matches float32
	content := strings.ToLower(candidate.Content)
	for _, word := range strings.Fields(strings.ToLower(query)) {
		if strings.Contains(content, word) {
			matches++
		}
	}
	return matches, fmt.Sprintf("%d query keyword matches", int(matches)), nil
}

// scriptedModel plays the provider: it always requests handbook retrieval
// first, then answers strictly from what came back. An empty retrieval result
// produces a refusal instead of a guess, which is the on-corpus guarantee this
// build demonstrates.
type scriptedModel struct {
	calls int
}

func (m *scriptedModel) Generate(_ context.Context, request lebro.ModelRequest) (lebro.ModelResponse, error) {
	m.calls++
	if m.calls%2 == 1 {
		// Retrieve with the customer's own words; a real model rewrites the
		// question into a search query, but the corpus reach is identical.
		query := lastUserContent(request.Messages)
		args, err := json.Marshal(map[string]string{"query": query})
		if err != nil {
			return lebro.ModelResponse{}, err
		}
		calls, err := lebro.NewModelToolCalls(lebro.ModelToolCall{
			ID:        fmt.Sprintf("call-%d", m.calls),
			ToolID:    "search_handbook",
			Arguments: args,
		})
		if err != nil {
			return lebro.ModelResponse{}, err
		}
		return lebro.ModelResponse{
			Message:      lebro.Message{Role: lebro.RoleAssistant, ToolCalls: calls},
			FinishReason: lebro.FinishReasonToolCalls,
		}, nil
	}

	answer := answerFromRetrieval(request.Messages)
	return lebro.ModelResponse{
		Message:      lebro.Message{Role: lebro.RoleAssistant, Content: answer},
		FinishReason: lebro.FinishReasonStop,
	}, nil
}

func lastUserContent(messages []lebro.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == lebro.RoleUser {
			return messages[i].Content
		}
	}
	return ""
}

func answerFromRetrieval(messages []lebro.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != lebro.RoleTool {
			continue
		}
		var payload struct {
			Chunks []struct {
				Content string  `json:"content"`
				Source  string  `json:"source"`
				Score   float32 `json:"score"`
			} `json:"chunks"`
		}
		if err := json.Unmarshal([]byte(messages[i].Content), &payload); err != nil {
			return "I could not read the handbook result."
		}
		if len(payload.Chunks) == 0 || payload.Chunks[0].Score == 0 {
			return "The public handbook says nothing about that, so I cannot help here."
		}
		top := payload.Chunks[0]
		return fmt.Sprintf("According to %s: %s", top.Source, top.Content)
	}
	return "I did not retrieve anything."
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
