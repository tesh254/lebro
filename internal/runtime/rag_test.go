package runtime

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDocumentValidate(t *testing.T) {
	tests := []struct {
		name     string
		document Document
		wantErr  string
	}{
		{
			name:     "valid without metadata",
			document: Document{ID: "doc-1", Content: "hello"},
		},
		{
			name:     "valid with metadata",
			document: Document{ID: "doc-1", Content: "hello", Metadata: json.RawMessage(`{"team":"platform"}`)},
		},
		{
			name:     "missing ID",
			document: Document{Content: "hello"},
			wantErr:  "document ID is required",
		},
		{
			name:     "missing content",
			document: Document{ID: "doc-1"},
			wantErr:  "document content is required",
		},
		{
			name:     "metadata not an object",
			document: Document{ID: "doc-1", Content: "hello", Metadata: json.RawMessage(`["a"]`)},
			wantErr:  "must be a JSON object",
		},
		{
			name:     "metadata invalid JSON",
			document: Document{ID: "doc-1", Content: "hello", Metadata: json.RawMessage(`{`)},
			wantErr:  "must be a JSON object",
		},
		{
			name:     "reserved document_id key",
			document: Document{ID: "doc-1", Content: "hello", Metadata: json.RawMessage(`{"document_id":"other"}`)},
			wantErr:  `reserved key "document_id"`,
		},
		{
			name:     "reserved source key",
			document: Document{ID: "doc-1", Content: "hello", Metadata: json.RawMessage(`{"source":"spoof"}`)},
			wantErr:  `reserved key "source"`,
		},
		{
			name:     "reserved chunk_index key",
			document: Document{ID: "doc-1", Content: "hello", Metadata: json.RawMessage(`{"chunk_index":9}`)},
			wantErr:  `reserved key "chunk_index"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.document.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() error = nil, want %q", test.wantErr)
			}
			if !contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() error = %q, want it to contain %q", err.Error(), test.wantErr)
			}
		})
	}
}

func TestChunkValidate(t *testing.T) {
	valid := Chunk{ID: "doc-1#0", DocumentID: "doc-1", Content: "hello", Index: 0}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}

	tests := []struct {
		name    string
		chunk   Chunk
		wantErr string
	}{
		{name: "missing ID", chunk: Chunk{DocumentID: "doc-1", Content: "x"}, wantErr: "chunk ID is required"},
		{name: "missing document ID", chunk: Chunk{ID: "doc-1#0", Content: "x"}, wantErr: "document ID is required"},
		{name: "missing content", chunk: Chunk{ID: "doc-1#0", DocumentID: "doc-1"}, wantErr: "content is required"},
		{name: "negative index", chunk: Chunk{ID: "doc-1#0", DocumentID: "doc-1", Content: "x", Index: -1}, wantErr: "must not be negative"},
		{name: "invalid metadata", chunk: Chunk{ID: "doc-1#0", DocumentID: "doc-1", Content: "x", Metadata: json.RawMessage(`{`)}, wantErr: "metadata"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.chunk.Validate()
			if err == nil {
				t.Fatalf("Validate() error = nil, want %q", test.wantErr)
			}
			if !contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() error = %q, want it to contain %q", err.Error(), test.wantErr)
			}
		})
	}
}

func TestChunkID(t *testing.T) {
	if got := ChunkID("doc-1", 0); got != "doc-1#0" {
		t.Fatalf("ChunkID = %q, want %q", got, "doc-1#0")
	}
	if got := ChunkID("doc-1", 42); got != "doc-1#42" {
		t.Fatalf("ChunkID = %q, want %q", got, "doc-1#42")
	}
}

func TestRetrievalQueryValidate(t *testing.T) {
	tests := []struct {
		name    string
		query   RetrievalQuery
		wantErr string
	}{
		{name: "valid", query: RetrievalQuery{Query: "what is lebro"}},
		{name: "empty", query: RetrievalQuery{}, wantErr: "must not be empty"},
		{name: "whitespace only", query: RetrievalQuery{Query: "   \t\n"}, wantErr: "must not be empty"},
		{name: "negative top k", query: RetrievalQuery{Query: "x", TopK: -1}, wantErr: "TopK must not be negative"},
		{name: "min score above one", query: RetrievalQuery{Query: "x", MinScore: 1.5}, wantErr: "MinScore must be in [0, 1]"},
		{name: "min score negative", query: RetrievalQuery{Query: "x", MinScore: -0.1}, wantErr: "MinScore must be in [0, 1]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.query.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() error = nil, want %q", test.wantErr)
			}
			if !contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() error = %q, want it to contain %q", err.Error(), test.wantErr)
			}
		})
	}
}

func TestRAGErrorSentinels(t *testing.T) {
	tests := []struct {
		kind RAGErrorKind
		want error
	}{
		{RAGErrorInvalidDocument, ErrRAGInvalidDocument},
		{RAGErrorChunking, ErrRAGChunking},
		{RAGErrorEmbedding, ErrRAGEmbedding},
		{RAGErrorIndexing, ErrRAGIndexing},
		{RAGErrorRetrieval, ErrRAGRetrieval},
		{RAGErrorKind("unknown"), ErrRAGRetrieval},
	}
	for _, test := range tests {
		t.Run(string(test.kind), func(t *testing.T) {
			err := &RAGError{Kind: test.kind, Err: errors.New("boom")}
			if !errors.Is(err, test.want) {
				t.Fatalf("errors.Is(%v, %v) = false, want true", err, test.want)
			}
		})
	}
}

func TestRAGErrorUnwrapPreservesCause(t *testing.T) {
	cause := errors.New("provider exploded")
	err := &RAGError{Kind: RAGErrorEmbedding, DocumentID: "doc-1", Err: cause}

	if !errors.Is(err, cause) {
		t.Fatal("errors.Is(err, cause) = false, want true")
	}
	if !errors.Is(err, ErrRAGEmbedding) {
		t.Fatal("errors.Is(err, ErrRAGEmbedding) = false, want true")
	}
	if errors.Is(err, ErrRAGChunking) {
		t.Fatal("errors.Is(err, ErrRAGChunking) = true, want false")
	}
	if !contains(err.Error(), `document "doc-1"`) {
		t.Fatalf("Error() = %q, want it to name the document", err.Error())
	}
}

func TestRAGErrorNilSafe(t *testing.T) {
	var err *RAGError
	if got := err.Error(); got != "lebro: RAG failure" {
		t.Fatalf("Error() = %q, want %q", got, "lebro: RAG failure")
	}
	if err.Unwrap() != nil {
		t.Fatal("Unwrap() != nil, want nil")
	}
	if err.Is(ErrRAGRetrieval) {
		t.Fatal("Is() = true on nil error, want false")
	}
}

// TestChunkMetadataRoundTrip is the provenance guarantee the retrieval contract
// rests on: what an indexer writes is what a retriever reads back, and the
// application metadata a caller supplied survives unchanged.
func TestChunkMetadataRoundTrip(t *testing.T) {
	chunk := Chunk{
		ID:         "doc-1#2",
		DocumentID: "doc-1",
		Content:    "the answer",
		Source:     "handbook.md",
		Index:      2,
		Metadata:   json.RawMessage(`{"team":"platform","public":true}`),
	}

	encoded, err := chunkMetadata(chunk)
	if err != nil {
		t.Fatalf("chunkMetadata error = %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal metadata error = %v", err)
	}
	if string(decoded[ChunkMetadataDocumentID]) != `"doc-1"` {
		t.Fatalf("document_id = %s, want %q", decoded[ChunkMetadataDocumentID], "doc-1")
	}
	if string(decoded[ChunkMetadataSource]) != `"handbook.md"` {
		t.Fatalf("source = %s, want %q", decoded[ChunkMetadataSource], "handbook.md")
	}
	if string(decoded[ChunkMetadataChunkIndex]) != "2" {
		t.Fatalf("chunk_index = %s, want 2", decoded[ChunkMetadataChunkIndex])
	}

	restored, err := chunkFromMetadata(SimilarityResult{
		ID:       chunk.ID,
		Score:    0.9,
		Metadata: encoded,
		Content:  chunk.Content,
	})
	if err != nil {
		t.Fatalf("chunkFromMetadata error = %v", err)
	}
	if restored.DocumentID != chunk.DocumentID {
		t.Fatalf("DocumentID = %q, want %q", restored.DocumentID, chunk.DocumentID)
	}
	if restored.Source != chunk.Source {
		t.Fatalf("Source = %q, want %q", restored.Source, chunk.Source)
	}
	if restored.Index != chunk.Index {
		t.Fatalf("Index = %d, want %d", restored.Index, chunk.Index)
	}
	if restored.Content != chunk.Content {
		t.Fatalf("Content = %q, want %q", restored.Content, chunk.Content)
	}

	// Reserved keys must be stripped so the caller sees the metadata it
	// supplied, not the storage representation.
	var restoredMetadata map[string]json.RawMessage
	if err := json.Unmarshal(restored.Metadata, &restoredMetadata); err != nil {
		t.Fatalf("unmarshal restored metadata error = %v", err)
	}
	for _, reserved := range reservedChunkMetadataKeys {
		if _, exists := restoredMetadata[reserved]; exists {
			t.Fatalf("restored metadata still contains reserved key %q", reserved)
		}
	}
	if string(restoredMetadata["team"]) != `"platform"` {
		t.Fatalf("team = %s, want %q", restoredMetadata["team"], "platform")
	}
	if string(restoredMetadata["public"]) != "true" {
		t.Fatalf("public = %s, want true", restoredMetadata["public"])
	}
}

func TestChunkMetadataOmitsEmptySource(t *testing.T) {
	encoded, err := chunkMetadata(Chunk{ID: "doc-1#0", DocumentID: "doc-1", Content: "x", Index: 0})
	if err != nil {
		t.Fatalf("chunkMetadata error = %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal error = %v", err)
	}
	// An empty source must be absent, not "": a filter on source should not
	// match documents that never declared one.
	if _, exists := decoded[ChunkMetadataSource]; exists {
		t.Fatal("metadata contains source key for a document with no source")
	}
}

// TestChunkFromMetadataToleratesForeignRecords covers a record written by
// something other than an Indexer: retrieval should still return usable
// content rather than failing the whole query.
func TestChunkFromMetadataToleratesForeignRecords(t *testing.T) {
	chunk, err := chunkFromMetadata(SimilarityResult{
		ID:       "foreign-1",
		Content:  "some text",
		Metadata: json.RawMessage(`{"unrelated":"value"}`),
	})
	if err != nil {
		t.Fatalf("chunkFromMetadata error = %v", err)
	}
	if chunk.ID != "foreign-1" || chunk.Content != "some text" {
		t.Fatalf("chunk = %+v, want ID and content preserved", chunk)
	}
	if chunk.DocumentID != "" || chunk.Source != "" || chunk.Index != 0 {
		t.Fatalf("chunk = %+v, want zero provenance for a foreign record", chunk)
	}
}

func TestChunkFromMetadataRejectsMalformedProvenance(t *testing.T) {
	tests := []struct {
		name     string
		metadata string
	}{
		{name: "invalid JSON", metadata: `{`},
		{name: "document_id wrong type", metadata: `{"document_id":42}`},
		{name: "source wrong type", metadata: `{"source":[1]}`},
		{name: "chunk_index wrong type", metadata: `{"chunk_index":"two"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := chunkFromMetadata(SimilarityResult{ID: "x", Metadata: json.RawMessage(test.metadata)}); err == nil {
				t.Fatal("chunkFromMetadata error = nil, want an error")
			}
		})
	}
}

func TestMergeVectorMetadataFiltersEnforcedWins(t *testing.T) {
	caller := VectorMetadataFilter{Match: map[string]json.RawMessage{
		"tenant": json.RawMessage(`"attacker"`),
		"topic":  json.RawMessage(`"billing"`),
	}}
	enforced := VectorMetadataFilter{Match: map[string]json.RawMessage{
		"tenant": json.RawMessage(`"acme"`),
	}}

	merged := mergeVectorMetadataFilters(caller, enforced)

	// The enforced scope must survive a caller naming the same key, which is
	// what makes handing a retrieval tool to a model safe.
	if string(merged.Match["tenant"]) != `"acme"` {
		t.Fatalf("tenant = %s, want %q", merged.Match["tenant"], "acme")
	}
	if string(merged.Match["topic"]) != `"billing"` {
		t.Fatalf("topic = %s, want %q", merged.Match["topic"], "billing")
	}
}

func TestMergeVectorMetadataFiltersEmpty(t *testing.T) {
	merged := mergeVectorMetadataFilters(VectorMetadataFilter{}, VectorMetadataFilter{})
	if len(merged.Match) != 0 {
		t.Fatalf("Match = %v, want empty", merged.Match)
	}
}

func TestCloneVectorMetadataFilterIsDefensive(t *testing.T) {
	original := VectorMetadataFilter{Match: map[string]json.RawMessage{"tenant": json.RawMessage(`"acme"`)}}
	cloned := cloneVectorMetadataFilter(original)

	original.Match["tenant"] = json.RawMessage(`"other"`)
	delete(original.Match, "tenant")

	if string(cloned.Match["tenant"]) != `"acme"` {
		t.Fatalf("cloned tenant = %s, want %q", cloned.Match["tenant"], "acme")
	}
}

// contains keeps the assertion sites terse; it is a thin alias so the intent
// reads as an assertion rather than a string operation.
func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

// TestDocumentValidateRejectsNullMetadata covers the crash path: JSON null
// unmarshals into a nil map without error, and the indexer writes provenance
// keys into that map.
func TestDocumentValidateRejectsNullMetadata(t *testing.T) {
	document := Document{ID: "doc-1", Content: "hello", Metadata: json.RawMessage(`null`)}
	err := document.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want a rejection of null metadata")
	}
	if !contains(err.Error(), "null") {
		t.Fatalf("Validate() error = %q, want it to name the null metadata", err.Error())
	}
}

// TestChunkMetadataSurvivesNullMetadata guards the same defect one layer down: a
// custom Chunker can emit chunk metadata that never passed Document.Validate.
func TestChunkMetadataSurvivesNullMetadata(t *testing.T) {
	encoded, err := chunkMetadata(Chunk{
		ID:         "doc-1#0",
		DocumentID: "doc-1",
		Content:    "hello",
		Index:      0,
		Metadata:   json.RawMessage(`null`),
	})
	if err != nil {
		t.Fatalf("chunkMetadata error = %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal error = %v", err)
	}
	if string(decoded[ChunkMetadataDocumentID]) != `"doc-1"` {
		t.Fatalf("document_id = %s, want provenance written despite null metadata", decoded[ChunkMetadataDocumentID])
	}
}

// TestChunkFromMetadataRejectsMalformedReservedValues covers reserved keys that
// are present but unusable. Decoding JSON null into a string or int is a silent
// no-op, so without these checks a hit would carry an empty document ID or a
// negative index as though it were real provenance.
func TestChunkFromMetadataRejectsMalformedReservedValues(t *testing.T) {
	tests := []struct {
		name     string
		metadata string
	}{
		{name: "null document_id", metadata: `{"document_id":null}`},
		{name: "empty document_id", metadata: `{"document_id":""}`},
		{name: "null source", metadata: `{"source":null}`},
		{name: "empty source", metadata: `{"source":""}`},
		{name: "null chunk_index", metadata: `{"chunk_index":null}`},
		{name: "negative chunk_index", metadata: `{"chunk_index":-5}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := chunkFromMetadata(SimilarityResult{ID: "x", Metadata: json.RawMessage(test.metadata)}); err == nil {
				t.Fatalf("chunkFromMetadata(%s) error = nil, want a rejection", test.metadata)
			}
		})
	}
}
