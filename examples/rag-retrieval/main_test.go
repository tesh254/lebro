package main

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

func TestExample(t *testing.T) {
	main()
}

// buildPipeline mirrors the example's ingestion so the assertions below exercise
// the same configuration the example demonstrates.
func buildPipeline(t *testing.T, filter lebro.VectorMetadataFilter) *lebro.VectorRetriever {
	t.Helper()
	ctx := t.Context()

	chunker, err := lebro.NewCharacterChunker(lebro.CharacterChunkerConfig{Size: 160, Overlap: 32})
	if err != nil {
		t.Fatal(err)
	}
	embeddings := localEmbedder{dimension: 64}
	store := lebro.NewMemoryVectorStore()

	indexer, err := lebro.NewIndexer(lebro.IndexerConfig{
		Chunker:    chunker,
		Embeddings: embeddings,
		Store:      store,
		Index:      "handbook",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := indexer.EnsureIndex(ctx); err != nil {
		t.Fatal(err)
	}

	documents := []lebro.Document{
		{
			ID:       "refunds",
			Content:  "Refund policy: customers may request a full refund within 30 days of purchase.",
			Source:   "policies/refunds.md",
			Metadata: json.RawMessage(`{"visibility":"public"}`),
		},
		{
			ID:       "margins",
			Content:  "Internal only: supplier pricing and margin targets are confidential.",
			Source:   "internal/margins.md",
			Metadata: json.RawMessage(`{"visibility":"internal"}`),
		},
	}
	for _, document := range documents {
		if _, err := indexer.Ingest(ctx, document); err != nil {
			t.Fatal(err)
		}
	}

	retriever, err := lebro.NewVectorRetriever(lebro.VectorRetrieverConfig{
		Embeddings: embeddings,
		Store:      store,
		Index:      "handbook",
		TopK:       3,
		Filter:     filter,
	})
	if err != nil {
		t.Fatal(err)
	}
	return retriever
}

func TestRetrievalReturnsSourceMetadata(t *testing.T) {
	retriever := buildPipeline(t, lebro.VectorMetadataFilter{})

	hits, err := retriever.Retrieve(t.Context(), lebro.RetrievalQuery{Query: "refund within 30 days"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("hits = 0, want at least one")
	}
	if hits[0].DocumentID != "refunds" {
		t.Fatalf("top hit = %q, want %q", hits[0].DocumentID, "refunds")
	}
	if hits[0].Source != "policies/refunds.md" {
		t.Fatalf("source = %q, want %q", hits[0].Source, "policies/refunds.md")
	}
}

func TestRetrievalRespectsVisibilityFilter(t *testing.T) {
	retriever := buildPipeline(t, lebro.VectorMetadataFilter{
		Match: map[string]json.RawMessage{"visibility": json.RawMessage(`"public"`)},
	})

	hits, err := retriever.Retrieve(t.Context(), lebro.RetrievalQuery{
		Query: "supplier pricing and margin targets",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range hits {
		if hit.DocumentID == "margins" {
			t.Fatal("retrieved an internal document through a public-only retriever")
		}
	}
}

func TestAgentGroundsAnswerInRetrieval(t *testing.T) {
	retriever := buildPipeline(t, lebro.VectorMetadataFilter{
		Match: map[string]json.RawMessage{"visibility": json.RawMessage(`"public"`)},
	})

	tool, err := lebro.NewRetrievalTool(lebro.RetrievalToolConfig{
		ID:        "search_handbook",
		Retriever: retriever,
		TopK:      2,
		MaxTopK:   4,
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := lebro.NewToolRegistry(lebrojsonschema.NewCompiler())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}

	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:    "support",
			Tools: []lebro.ToolID{"search_handbook"},
		},
		Model:    &scriptedModel{toolID: "search_handbook"},
		Tools:    registry,
		MaxSteps: 4,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := agent.Run(t.Context(), lebro.RunInput{
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "What is the refund window?"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != lebro.RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}

	var sawToolResult bool
	for _, message := range result.Messages {
		if message.Role == lebro.RoleTool && strings.Contains(message.Content, "refund") {
			sawToolResult = true
		}
	}
	if !sawToolResult {
		t.Fatal("transcript has no retrieved handbook content")
	}
}

func TestTruncateBoundsRuneWidth(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		limit int
		want  string
	}{
		{name: "shorter than limit", text: "short", limit: 10, want: "short"},
		{name: "exactly the limit", text: "abcde", limit: 5, want: "abcde"},
		{name: "longer than limit", text: "abcdefghij", limit: 6, want: "abc..."},
		{name: "multi-byte stays intact", text: "日本語のテキスト", limit: 6, want: "日本語..."},
		{name: "limit at ellipsis width", text: "abcdef", limit: 3, want: "abc"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := truncate(test.text, test.limit)
			if got != test.want {
				t.Fatalf("truncate(%q, %d) = %q, want %q", test.text, test.limit, got, test.want)
			}
			// The advertised bound is on runes, ellipsis included.
			if n := len([]rune(got)); n > test.limit {
				t.Fatalf("truncate(%q, %d) returned %d runes, want at most %d", test.text, test.limit, n, test.limit)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("truncate(%q, %d) = %q is not valid UTF-8", test.text, test.limit, got)
			}
		})
	}
}
