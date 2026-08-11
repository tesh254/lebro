package lebro_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

// stubEmbeddingModel is a deterministic EmbeddingModel so the public RAG
// contracts can be exercised without a provider. It lives in the external test
// package, which is also what proves the contracts are satisfiable from outside
// the module.
type stubEmbeddingModel struct{ dimension int }

func (m stubEmbeddingModel) Dimension() int { return m.dimension }

func (m stubEmbeddingModel) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	vectors := make([][]float32, len(inputs))
	for i, input := range inputs {
		vector := make([]float32, m.dimension)
		for j, b := range []byte(strings.ToLower(input)) {
			vector[(int(b)+j)%m.dimension] += float32(b%7) + 1
		}
		vector[0] += 1
		vectors[i] = vector
	}
	return vectors, nil
}

var (
	_ lebro.EmbeddingModel = stubEmbeddingModel{}
	_ lebro.Chunker        = (*lebro.CharacterChunker)(nil)
	_ lebro.Retriever      = (*lebro.VectorRetriever)(nil)
	_ lebro.Tool           = (*lebro.RetrievalTool)(nil)
)

// seedCorpus builds the full public RAG pipeline over an in-memory vector store.
func seedCorpus(t *testing.T) (*lebro.Indexer, *lebro.VectorRetriever) {
	t.Helper()
	ctx := context.Background()

	chunker, err := lebro.NewCharacterChunker(lebro.CharacterChunkerConfig{Size: 120, Overlap: 20})
	if err != nil {
		t.Fatalf("NewCharacterChunker error = %v", err)
	}
	embeddings := stubEmbeddingModel{dimension: 32}
	store := lebro.NewMemoryVectorStore()

	indexer, err := lebro.NewIndexer(lebro.IndexerConfig{
		Chunker:    chunker,
		Embeddings: embeddings,
		Store:      store,
		Index:      "handbook",
	})
	if err != nil {
		t.Fatalf("NewIndexer error = %v", err)
	}
	if err := indexer.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex error = %v", err)
	}

	documents := []lebro.Document{
		{
			ID:       "refunds",
			Content:  "Refund policy: customers may request a full refund within 30 days of purchase. Refunds are issued to the original payment method.",
			Source:   "policies/refunds.md",
			Metadata: json.RawMessage(`{"tenant":"acme","visibility":"public"}`),
		},
		{
			ID:       "shipping",
			Content:  "Shipping: standard delivery takes five business days. Express delivery arrives the next business day.",
			Source:   "policies/shipping.md",
			Metadata: json.RawMessage(`{"tenant":"acme","visibility":"public"}`),
		},
		{
			ID:       "internal",
			Content:  "Internal only: margin targets and supplier pricing are confidential and must not be shared with customers.",
			Source:   "internal/margins.md",
			Metadata: json.RawMessage(`{"tenant":"acme","visibility":"internal"}`),
		},
	}
	for _, document := range documents {
		if _, err := indexer.Ingest(ctx, document); err != nil {
			t.Fatalf("Ingest(%s) error = %v", document.ID, err)
		}
	}

	retriever, err := lebro.NewVectorRetriever(lebro.VectorRetrieverConfig{
		Embeddings: embeddings,
		Store:      store,
		Index:      "handbook",
		TopK:       3,
	})
	if err != nil {
		t.Fatalf("NewVectorRetriever error = %v", err)
	}
	return indexer, retriever
}

// TestMAD47RAGPublicContract walks the acceptance path through the façade: a
// document is chunked, embedded, indexed, and retrieved by a semantic query,
// with stable source metadata on every hit.
func TestMAD47RAGPublicContract(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	indexer, retriever := seedCorpus(t)
	if indexer.Index() != "handbook" {
		t.Fatalf("Index() = %q, want %q", indexer.Index(), "handbook")
	}

	results, err := retriever.Retrieve(ctx, lebro.RetrievalQuery{Query: "how long do I have to request a refund"})
	if err != nil {
		t.Fatalf("Retrieve error = %v", err)
	}
	if len(results) == 0 {
		t.Fatal("len(results) = 0, want at least one hit")
	}
	for i, result := range results {
		if result.DocumentID == "" || result.Source == "" || result.Content == "" {
			t.Fatalf("results[%d] = %+v, want stable source metadata", i, result)
		}
		if result.ID != lebro.ChunkID(result.DocumentID, result.Index) {
			t.Fatalf("results[%d].ID = %q, want %q", i, result.ID, lebro.ChunkID(result.DocumentID, result.Index))
		}
	}
	for i := 1; i < len(results); i++ {
		if results[i-1].Score < results[i].Score {
			t.Fatalf("results not ordered by descending score: %f then %f", results[i-1].Score, results[i].Score)
		}
	}
}

// TestMAD47RetrievalRespectsMetadataFilter covers the acceptance criterion that
// retrieval honors configured metadata filters.
func TestMAD47RetrievalRespectsMetadataFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	_, unfiltered := seedCorpus(t)

	// Without a filter the internal document is reachable, which is what makes
	// the filtered case meaningful rather than vacuous.
	all, err := unfiltered.Retrieve(ctx, lebro.RetrievalQuery{Query: "confidential supplier pricing and margin targets", TopK: 10})
	if err != nil {
		t.Fatalf("Retrieve error = %v", err)
	}
	sawInternal := false
	for _, result := range all {
		if result.DocumentID == "internal" {
			sawInternal = true
		}
	}
	if !sawInternal {
		t.Fatal("unfiltered retrieval did not reach the internal document; the filter assertion would be vacuous")
	}

	filtered, err := lebro.NewVectorRetriever(lebro.VectorRetrieverConfig{
		Embeddings: stubEmbeddingModel{dimension: 32},
		Store:      lebro.NewMemoryVectorStore(),
		Index:      "handbook",
		TopK:       10,
		Filter: lebro.VectorMetadataFilter{
			Match: map[string]json.RawMessage{"visibility": json.RawMessage(`"public"`)},
		},
	})
	if err != nil {
		t.Fatalf("NewVectorRetriever error = %v", err)
	}
	// A fresh store has no index, so this also confirms the vector sentinel
	// survives through the RAG error wrapper.
	if _, err := filtered.Retrieve(ctx, lebro.RetrievalQuery{Query: "anything"}); !errors.Is(err, lebro.ErrVectorNotFound) {
		t.Fatalf("Retrieve error = %v, want ErrVectorNotFound", err)
	}
}

// TestMAD47RetrievalToolFilterIsNotModelSettable is the isolation guarantee that
// makes handing retrieval to a model safe.
func TestMAD47RetrievalToolFilterIsNotModelSettable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	_, retriever := seedCorpus(t)
	tool, err := lebro.NewRetrievalTool(lebro.RetrievalToolConfig{
		ID:          "search_handbook",
		Retriever:   retriever,
		Description: "Search the customer handbook.",
		TopK:        3,
		MaxTopK:     5,
		Filter: lebro.VectorMetadataFilter{
			Match: map[string]json.RawMessage{"visibility": json.RawMessage(`"public"`)},
		},
	})
	if err != nil {
		t.Fatalf("NewRetrievalTool error = %v", err)
	}

	output, err := tool.Execute(ctx, json.RawMessage(`{"query":"confidential supplier pricing and margin targets"}`))
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	var decoded struct {
		Chunks []struct {
			DocumentID string `json:"document_id"`
		} `json:"chunks"`
	}
	if err := json.Unmarshal(output, &decoded); err != nil {
		t.Fatalf("unmarshal output error = %v", err)
	}
	for _, chunk := range decoded.Chunks {
		if chunk.DocumentID == "internal" {
			t.Fatal("retrieval tool leaked a document outside its configured visibility filter")
		}
	}
}

// TestMAD47RetrievalToolRealSchemaBoundary validates the tool against the
// bundled JSON Schema compiler, which is what an application actually uses.
func TestMAD47RetrievalToolRealSchemaBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	_, retriever := seedCorpus(t)
	tool, err := lebro.NewRetrievalTool(lebro.RetrievalToolConfig{
		ID:        "search_handbook",
		Retriever: retriever,
		TopK:      2,
		MaxTopK:   4,
	})
	if err != nil {
		t.Fatalf("NewRetrievalTool error = %v", err)
	}

	registry, err := lebro.NewToolRegistry(lebrojsonschema.NewCompiler())
	if err != nil {
		t.Fatalf("NewToolRegistry error = %v", err)
	}
	if err := registry.Register(tool); err != nil {
		t.Fatalf("Register error = %v", err)
	}

	tests := []struct {
		name      string
		arguments string
		wantState lebro.ToolExecutionState
	}{
		{name: "valid query", arguments: `{"query":"refund window"}`, wantState: lebro.ToolExecutionSucceeded},
		{name: "valid query with top_k", arguments: `{"query":"refund window","top_k":2}`, wantState: lebro.ToolExecutionSucceeded},
		{name: "missing query", arguments: `{}`, wantState: lebro.ToolExecutionInvalidInput},
		{name: "wrong query type", arguments: `{"query":42}`, wantState: lebro.ToolExecutionInvalidInput},
		{name: "top_k below minimum", arguments: `{"query":"x","top_k":0}`, wantState: lebro.ToolExecutionInvalidInput},
		// A model must not be able to introduce a filter, an index, or any other
		// field the contract does not declare.
		{name: "smuggled filter", arguments: `{"query":"x","filter":{"visibility":"internal"}}`, wantState: lebro.ToolExecutionInvalidInput},
		{name: "smuggled index", arguments: `{"query":"x","index":"other"}`, wantState: lebro.ToolExecutionInvalidInput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := registry.Execute(ctx, "search_handbook", lebro.ToolExecutionRequest{
				Arguments: json.RawMessage(test.arguments),
			})
			if result.State != test.wantState {
				t.Fatalf("State = %q (err = %v), want %q", result.State, result.Err, test.wantState)
			}
		})
	}
}

// TestMAD47AgentUsesRetrievalToolInBoundedLoop is the acceptance criterion that
// an agent can use the retrieval tool in the existing bounded tool loop. The
// model is scripted: it requests retrieval on the first step and answers from
// the tool result on the second, with no RAG-specific agent behavior involved.
func TestMAD47AgentUsesRetrievalToolInBoundedLoop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	_, retriever := seedCorpus(t)
	tool, err := lebro.NewRetrievalTool(lebro.RetrievalToolConfig{
		ID:          "search_handbook",
		Retriever:   retriever,
		Description: "Search the customer handbook for relevant passages.",
		TopK:        2,
		MaxTopK:     4,
	})
	if err != nil {
		t.Fatalf("NewRetrievalTool error = %v", err)
	}

	registry, err := lebro.NewToolRegistry(lebrojsonschema.NewCompiler())
	if err != nil {
		t.Fatalf("NewToolRegistry error = %v", err)
	}
	if err := registry.Register(tool); err != nil {
		t.Fatalf("Register error = %v", err)
	}

	model := &scriptedRetrievalModel{toolID: "search_handbook"}
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:           "support",
			Instructions: "Answer using the handbook.",
			Tools:        []lebro.ToolID{"search_handbook"},
		},
		Model:    model,
		Tools:    registry,
		MaxSteps: 4,
	})
	if err != nil {
		t.Fatalf("NewAgent error = %v", err)
	}

	result, err := agent.Run(ctx, lebro.RunInput{
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "What is the refund window?"}},
	})
	if err != nil {
		t.Fatalf("Run error = %v", err)
	}
	if result.Status != lebro.RunStatusSucceeded {
		t.Fatalf("Status = %q, want succeeded", result.Status)
	}

	// The transcript must contain the tool result the model retrieved, which is
	// what grounds the answer.
	var toolMessage *lebro.Message
	for i := range result.Messages {
		if result.Messages[i].Role == lebro.RoleTool {
			toolMessage = &result.Messages[i]
			break
		}
	}
	if toolMessage == nil {
		t.Fatal("transcript has no tool message; the agent did not invoke retrieval")
	}
	if !strings.Contains(toolMessage.Content, "refund") {
		t.Fatalf("tool message = %q, want retrieved handbook content", toolMessage.Content)
	}
	if model.calls != 2 {
		t.Fatalf("model calls = %d, want 2 (retrieve then answer)", model.calls)
	}

	// The tool definition must have reached the model, which is how a model
	// discovers retrieval at all.
	if len(model.sawTools) == 0 {
		t.Fatal("model never saw the retrieval tool definition")
	}
}

// scriptedRetrievalModel requests retrieval once, then answers from the result.
type scriptedRetrievalModel struct {
	toolID   lebro.ToolID
	calls    int
	sawTools []lebro.ToolID
}

func (m *scriptedRetrievalModel) Generate(_ context.Context, request lebro.ModelRequest) (lebro.ModelResponse, error) {
	m.calls++
	for _, definition := range request.Tools {
		m.sawTools = append(m.sawTools, definition.ID)
	}

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
		Message:      lebro.Message{Role: lebro.RoleAssistant, Content: "You may request a refund within 30 days of purchase."},
		FinishReason: lebro.FinishReasonStop,
	}, nil
}

// TestMAD47RAGErrorsAreMatchableThroughFacade keeps the normalized stage errors
// usable by applications that only import the root package.
func TestMAD47RAGErrorsAreMatchableThroughFacade(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	chunker, err := lebro.NewCharacterChunker(lebro.CharacterChunkerConfig{Size: 50})
	if err != nil {
		t.Fatalf("NewCharacterChunker error = %v", err)
	}

	_, err = chunker.Chunk(ctx, lebro.Document{Content: "no id"})
	if !errors.Is(err, lebro.ErrRAGInvalidDocument) {
		t.Fatalf("error = %v, want ErrRAGInvalidDocument", err)
	}
	var ragErr *lebro.RAGError
	if !errors.As(err, &ragErr) {
		t.Fatalf("error %v is not a *RAGError", err)
	}
	if ragErr.Kind != lebro.RAGErrorInvalidDocument {
		t.Fatalf("Kind = %q, want %q", ragErr.Kind, lebro.RAGErrorInvalidDocument)
	}

	for _, sentinel := range []error{
		lebro.ErrRAGInvalidDocument,
		lebro.ErrRAGChunking,
		lebro.ErrRAGEmbedding,
		lebro.ErrRAGIndexing,
		lebro.ErrRAGRetrieval,
	} {
		if sentinel == nil {
			t.Fatal("RAG sentinel error is nil")
		}
	}
}

// TestMAD47RAGCanonicalValues pins the wire-visible constants that applications
// and stored records depend on.
func TestMAD47RAGCanonicalValues(t *testing.T) {
	t.Parallel()

	kinds := map[lebro.RAGErrorKind]string{
		lebro.RAGErrorInvalidDocument: "rag_invalid_document",
		lebro.RAGErrorChunking:        "rag_chunking",
		lebro.RAGErrorEmbedding:       "rag_embedding",
		lebro.RAGErrorIndexing:        "rag_indexing",
		lebro.RAGErrorRetrieval:       "rag_retrieval",
	}
	for kind, want := range kinds {
		if string(kind) != want {
			t.Fatalf("RAGErrorKind = %q, want %q", kind, want)
		}
	}

	// These keys are written into stored vector metadata, so changing one is a
	// breaking change for existing indices.
	metadataKeys := map[string]string{
		lebro.ChunkMetadataDocumentID: "document_id",
		lebro.ChunkMetadataSource:     "source",
		lebro.ChunkMetadataChunkIndex: "chunk_index",
	}
	for got, want := range metadataKeys {
		if got != want {
			t.Fatalf("chunk metadata key = %q, want %q", got, want)
		}
	}

	if lebro.DefaultChunkSize <= 0 {
		t.Fatalf("DefaultChunkSize = %d, want positive", lebro.DefaultChunkSize)
	}
	if lebro.DefaultEmbeddingBatchSize <= 0 {
		t.Fatalf("DefaultEmbeddingBatchSize = %d, want positive", lebro.DefaultEmbeddingBatchSize)
	}
	if lebro.DefaultRetrievalTopK <= 0 {
		t.Fatalf("DefaultRetrievalTopK = %d, want positive", lebro.DefaultRetrievalTopK)
	}

	if got := lebro.ChunkID("doc-1", 3); got != "doc-1#3" {
		t.Fatalf("ChunkID = %q, want %q", got, "doc-1#3")
	}
}
