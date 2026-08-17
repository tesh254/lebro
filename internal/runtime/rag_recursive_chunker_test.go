package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"unicode/utf8"
)

func TestRecursiveChunkerBoundariesAndOverlap(t *testing.T) {
	chunker, err := NewRecursiveChunker(RecursiveChunkerConfig{Size: 11, Overlap: 3})
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := chunker.Chunk(t.Context(), Document{ID: "doc", Content: "alpha beta gamma"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := chunkTexts(chunks), []string{"alpha beta ", "ta gamma"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("chunks = %#v, want %#v", got, want)
	}
	assertChunkContract(t, chunks, "doc")
}

func TestRecursiveChunkerCustomSeparatorsAndRuneFallback(t *testing.T) {
	chunker, err := NewRecursiveChunker(RecursiveChunkerConfig{Size: 8, Separators: []string{"|"}})
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := chunker.Chunk(t.Context(), Document{ID: "doc", Content: "one|two|three"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := chunkTexts(chunks), []string{"one|two|", "three"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("custom chunks = %#v, want %#v", got, want)
	}

	fallback, err := NewRecursiveChunker(RecursiveChunkerConfig{Size: 3})
	if err != nil {
		t.Fatal(err)
	}
	chunks, err = fallback.Chunk(t.Context(), Document{ID: "unicode", Content: "日本語のテキスト"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := chunkTexts(chunks), []string{"日本語", "のテキ", "スト"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("fallback chunks = %#v, want %#v", got, want)
	}
	for _, chunk := range chunks {
		if !utf8.ValidString(chunk.Content) {
			t.Fatalf("invalid UTF-8 chunk %q", chunk.Content)
		}
	}
}

func TestRecursiveChunkerProvenanceAndValidation(t *testing.T) {
	chunker, err := NewRecursiveChunker(RecursiveChunkerConfig{Size: 3})
	if err != nil {
		t.Fatal(err)
	}
	metadata := json.RawMessage(`{"team":"platform"}`)
	chunks, err := chunker.Chunk(t.Context(), Document{ID: "doc", Content: "abcdef", Source: "guide.md", Metadata: metadata})
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range chunks {
		if chunk.Source != "guide.md" || string(chunk.Metadata) != string(metadata) {
			t.Fatalf("chunk provenance = %#v", chunk)
		}
	}
	chunks[0].Metadata[0] = 'X'
	if string(chunks[1].Metadata) != string(metadata) || string(metadata) != `{"team":"platform"}` {
		t.Fatal("metadata was not defensively copied")
	}

	for _, document := range []Document{{Content: "missing ID"}, {ID: "doc"}, {ID: "doc", Content: "x", Metadata: json.RawMessage(`{"source":"x"}`)}} {
		if _, err := chunker.Chunk(t.Context(), document); !errors.Is(err, ErrRAGInvalidDocument) {
			t.Fatalf("Chunk(%#v) error = %v, want invalid document", document, err)
		}
	}
	invalid := string([]byte{0xff, 'x'})
	if _, err := chunker.Chunk(t.Context(), Document{ID: "doc", Content: invalid}); !errors.Is(err, ErrRAGInvalidDocument) {
		t.Fatalf("invalid UTF-8 error = %v", err)
	}
}

func TestRecursiveChunkerCancellationAndNil(t *testing.T) {
	chunker, err := NewRecursiveChunker(RecursiveChunkerConfig{Size: 3})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := chunker.Chunk(ctx, Document{ID: "doc", Content: "hello"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}
	var nilChunker *RecursiveChunker
	if _, err := nilChunker.Chunk(t.Context(), Document{ID: "doc", Content: "hello"}); err == nil {
		t.Fatal("nil recursive chunker error = nil")
	}
}

func TestSlidingWindowChunkerMatchesCharacterChunker(t *testing.T) {
	config := SlidingWindowChunkerConfig{Size: 4, Overlap: 2}
	sliding, err := NewSlidingWindowChunker(config)
	if err != nil {
		t.Fatal(err)
	}
	character, err := NewCharacterChunker(CharacterChunkerConfig(config))
	if err != nil {
		t.Fatal(err)
	}
	document := Document{ID: "doc", Content: "日本語abcdef", Source: "guide.md"}
	got, err := sliding.Chunk(t.Context(), document)
	if err != nil {
		t.Fatal(err)
	}
	want, err := character.Chunk(t.Context(), document)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sliding chunks = %#v, want %#v", got, want)
	}
	var nilChunker *SlidingWindowChunker
	if _, err := nilChunker.Chunk(t.Context(), document); err == nil {
		t.Fatal("nil sliding-window chunker error = nil")
	}
}

func TestNewRecursiveAndSlidingWindowChunkerValidation(t *testing.T) {
	for _, config := range []RecursiveChunkerConfig{{Size: -1}, {Size: 4, Overlap: -1}, {Size: 4, Overlap: 4}} {
		if _, err := NewRecursiveChunker(config); err == nil {
			t.Fatalf("NewRecursiveChunker(%#v) error = nil", config)
		}
	}
	if _, err := NewSlidingWindowChunker(SlidingWindowChunkerConfig{Size: 4, Overlap: 4}); err == nil {
		t.Fatal("NewSlidingWindowChunker overlap error = nil")
	}
}

func chunkTexts(chunks []Chunk) []string {
	texts := make([]string, len(chunks))
	for i, chunk := range chunks {
		texts[i] = chunk.Content
	}
	return texts
}

func assertChunkContract(t *testing.T, chunks []Chunk, documentID string) {
	t.Helper()
	for index, chunk := range chunks {
		if err := chunk.Validate(); err != nil {
			t.Fatalf("chunks[%d].Validate() = %v", index, err)
		}
		if chunk.ID != ChunkID(documentID, index) || chunk.Index != index || chunk.DocumentID != documentID {
			t.Fatalf("chunks[%d] = %#v", index, chunk)
		}
	}
}
