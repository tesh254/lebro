package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNewCharacterChunkerValidation(t *testing.T) {
	tests := []struct {
		name    string
		config  CharacterChunkerConfig
		wantErr string
	}{
		{name: "defaults", config: CharacterChunkerConfig{}},
		{name: "explicit size and overlap", config: CharacterChunkerConfig{Size: 100, Overlap: 20}},
		{name: "zero overlap", config: CharacterChunkerConfig{Size: 100}},
		{name: "negative size", config: CharacterChunkerConfig{Size: -1}, wantErr: "size must not be negative"},
		{name: "negative overlap", config: CharacterChunkerConfig{Size: 10, Overlap: -1}, wantErr: "overlap must not be negative"},
		{name: "overlap equals size", config: CharacterChunkerConfig{Size: 10, Overlap: 10}, wantErr: "must be less than chunk size"},
		{name: "overlap exceeds size", config: CharacterChunkerConfig{Size: 10, Overlap: 11}, wantErr: "must be less than chunk size"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			chunker, err := NewCharacterChunker(test.config)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("NewCharacterChunker error = %v, want nil", err)
				}
				if chunker == nil {
					t.Fatal("NewCharacterChunker returned nil chunker")
				}
				return
			}
			if err == nil {
				t.Fatalf("NewCharacterChunker error = nil, want %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), test.wantErr)
			}
		})
	}
}

func TestCharacterChunkerDefaultSize(t *testing.T) {
	chunker, err := NewCharacterChunker(CharacterChunkerConfig{})
	if err != nil {
		t.Fatalf("NewCharacterChunker error = %v", err)
	}
	if chunker.size != DefaultChunkSize {
		t.Fatalf("size = %d, want %d", chunker.size, DefaultChunkSize)
	}
}

func TestCharacterChunkerNoOverlap(t *testing.T) {
	chunker, err := NewCharacterChunker(CharacterChunkerConfig{Size: 5})
	if err != nil {
		t.Fatalf("NewCharacterChunker error = %v", err)
	}

	chunks, err := chunker.Chunk(context.Background(), Document{ID: "doc-1", Content: "abcdefghij"})
	if err != nil {
		t.Fatalf("Chunk error = %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("len(chunks) = %d, want 2", len(chunks))
	}
	if chunks[0].Content != "abcde" {
		t.Fatalf("chunks[0].Content = %q, want %q", chunks[0].Content, "abcde")
	}
	if chunks[1].Content != "fghij" {
		t.Fatalf("chunks[1].Content = %q, want %q", chunks[1].Content, "fghij")
	}
	for i, chunk := range chunks {
		if chunk.Index != i {
			t.Fatalf("chunks[%d].Index = %d, want %d", i, chunk.Index, i)
		}
		if want := ChunkID("doc-1", i); chunk.ID != want {
			t.Fatalf("chunks[%d].ID = %q, want %q", i, chunk.ID, want)
		}
		if chunk.DocumentID != "doc-1" {
			t.Fatalf("chunks[%d].DocumentID = %q, want %q", i, chunk.DocumentID, "doc-1")
		}
	}
}

func TestCharacterChunkerWithOverlap(t *testing.T) {
	chunker, err := NewCharacterChunker(CharacterChunkerConfig{Size: 5, Overlap: 2})
	if err != nil {
		t.Fatalf("NewCharacterChunker error = %v", err)
	}

	chunks, err := chunker.Chunk(context.Background(), Document{ID: "doc-1", Content: "abcdefghij"})
	if err != nil {
		t.Fatalf("Chunk error = %v", err)
	}

	// Stride is 3 (size 5 - overlap 2): windows start at 0, 3, 6, 9.
	want := []string{"abcde", "defgh", "ghij"}
	if len(chunks) != len(want) {
		got := make([]string, len(chunks))
		for i, chunk := range chunks {
			got[i] = chunk.Content
		}
		t.Fatalf("chunks = %q, want %q", got, want)
	}
	for i, expected := range want {
		if chunks[i].Content != expected {
			t.Fatalf("chunks[%d].Content = %q, want %q", i, chunks[i].Content, expected)
		}
	}
}

// TestCharacterChunkerOverlapNoTrailingSuffix guards the boundary rule: with
// overlap, a naive loop emits a final window that is a pure suffix of the
// previous one, indexing the same text twice.
func TestCharacterChunkerOverlapNoTrailingSuffix(t *testing.T) {
	chunker, err := NewCharacterChunker(CharacterChunkerConfig{Size: 4, Overlap: 2})
	if err != nil {
		t.Fatalf("NewCharacterChunker error = %v", err)
	}

	chunks, err := chunker.Chunk(context.Background(), Document{ID: "doc-1", Content: "abcdef"})
	if err != nil {
		t.Fatalf("Chunk error = %v", err)
	}

	// Windows: [0:4]="abcd", [2:6]="cdef". A third window at 4 would yield "ef",
	// already fully contained in "cdef".
	want := []string{"abcd", "cdef"}
	if len(chunks) != len(want) {
		got := make([]string, len(chunks))
		for i, chunk := range chunks {
			got[i] = chunk.Content
		}
		t.Fatalf("chunks = %q, want %q", got, want)
	}
	for i, expected := range want {
		if chunks[i].Content != expected {
			t.Fatalf("chunks[%d].Content = %q, want %q", i, chunks[i].Content, expected)
		}
	}
}

func TestCharacterChunkerShorterThanWindow(t *testing.T) {
	chunker, err := NewCharacterChunker(CharacterChunkerConfig{Size: 100, Overlap: 10})
	if err != nil {
		t.Fatalf("NewCharacterChunker error = %v", err)
	}

	chunks, err := chunker.Chunk(context.Background(), Document{ID: "doc-1", Content: "short"})
	if err != nil {
		t.Fatalf("Chunk error = %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("len(chunks) = %d, want 1", len(chunks))
	}
	if chunks[0].Content != "short" {
		t.Fatalf("Content = %q, want %q", chunks[0].Content, "short")
	}
}

// TestCharacterChunkerRuneSafety is why windows are measured in runes: byte
// slicing would split a multi-byte character and produce invalid UTF-8.
func TestCharacterChunkerRuneSafety(t *testing.T) {
	chunker, err := NewCharacterChunker(CharacterChunkerConfig{Size: 3})
	if err != nil {
		t.Fatalf("NewCharacterChunker error = %v", err)
	}

	// Each of these characters is multi-byte in UTF-8.
	chunks, err := chunker.Chunk(context.Background(), Document{ID: "doc-1", Content: "日本語のテキスト"})
	if err != nil {
		t.Fatalf("Chunk error = %v", err)
	}

	want := []string{"日本語", "のテキ", "スト"}
	if len(chunks) != len(want) {
		got := make([]string, len(chunks))
		for i, chunk := range chunks {
			got[i] = chunk.Content
		}
		t.Fatalf("chunks = %q, want %q", got, want)
	}
	for i, expected := range want {
		if chunks[i].Content != expected {
			t.Fatalf("chunks[%d].Content = %q, want %q", i, chunks[i].Content, expected)
		}
		if !utf8.ValidString(chunks[i].Content) {
			t.Fatalf("chunks[%d].Content = %q is not valid UTF-8", i, chunks[i].Content)
		}
	}
}

func TestCharacterChunkerPropagatesProvenance(t *testing.T) {
	chunker, err := NewCharacterChunker(CharacterChunkerConfig{Size: 3})
	if err != nil {
		t.Fatalf("NewCharacterChunker error = %v", err)
	}

	metadata := json.RawMessage(`{"team":"platform"}`)
	chunks, err := chunker.Chunk(context.Background(), Document{
		ID:       "doc-1",
		Content:  "abcdef",
		Source:   "handbook.md",
		Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("Chunk error = %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("len(chunks) = %d, want 2", len(chunks))
	}
	for i, chunk := range chunks {
		if chunk.Source != "handbook.md" {
			t.Fatalf("chunks[%d].Source = %q, want %q", i, chunk.Source, "handbook.md")
		}
		if string(chunk.Metadata) != string(metadata) {
			t.Fatalf("chunks[%d].Metadata = %s, want %s", i, chunk.Metadata, metadata)
		}
	}

	// Chunk metadata must be a defensive copy: mutating one chunk's metadata
	// must not reach the document or its siblings.
	chunks[0].Metadata[0] = 'X'
	if string(chunks[1].Metadata) != string(metadata) {
		t.Fatalf("sibling metadata mutated: %s", chunks[1].Metadata)
	}
	if string(metadata) != `{"team":"platform"}` {
		t.Fatalf("document metadata mutated: %s", metadata)
	}
}

func TestCharacterChunkerRejectsInvalidDocument(t *testing.T) {
	chunker, err := NewCharacterChunker(CharacterChunkerConfig{Size: 10})
	if err != nil {
		t.Fatalf("NewCharacterChunker error = %v", err)
	}

	tests := []struct {
		name     string
		document Document
	}{
		{name: "empty ID", document: Document{Content: "hello"}},
		{name: "empty content", document: Document{ID: "doc-1"}},
		{name: "reserved metadata key", document: Document{ID: "doc-1", Content: "hello", Metadata: json.RawMessage(`{"source":"x"}`)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := chunker.Chunk(context.Background(), test.document)
			if !errors.Is(err, ErrRAGInvalidDocument) {
				t.Fatalf("Chunk error = %v, want ErrRAGInvalidDocument", err)
			}
		})
	}
}

func TestCharacterChunkerCanceledContext(t *testing.T) {
	chunker, err := NewCharacterChunker(CharacterChunkerConfig{Size: 10})
	if err != nil {
		t.Fatalf("NewCharacterChunker error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := chunker.Chunk(ctx, Document{ID: "doc-1", Content: "hello"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Chunk error = %v, want context.Canceled", err)
	}
}

func TestCharacterChunkerNilReceiverAndContext(t *testing.T) {
	var chunker *CharacterChunker
	if _, err := chunker.Chunk(context.Background(), Document{ID: "doc-1", Content: "x"}); err == nil {
		t.Fatal("Chunk on nil chunker error = nil, want an error")
	}

	real, err := NewCharacterChunker(CharacterChunkerConfig{Size: 10})
	if err != nil {
		t.Fatalf("NewCharacterChunker error = %v", err)
	}
	//nolint:staticcheck // deliberately passing a nil context to assert the guard
	if _, err := real.Chunk(nil, Document{ID: "doc-1", Content: "x"}); err == nil {
		t.Fatal("Chunk with nil context error = nil, want an error")
	}
}

// TestCharacterChunkerChunksAreValid ties the chunker to the contract every
// downstream stage assumes.
func TestCharacterChunkerChunksAreValid(t *testing.T) {
	chunker, err := NewCharacterChunker(CharacterChunkerConfig{Size: 7, Overlap: 3})
	if err != nil {
		t.Fatalf("NewCharacterChunker error = %v", err)
	}

	chunks, err := chunker.Chunk(context.Background(), Document{
		ID:      "doc-1",
		Content: strings.Repeat("lebro ", 20),
		Source:  "readme",
	})
	if err != nil {
		t.Fatalf("Chunk error = %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("len(chunks) = 0, want at least one")
	}
	for i, chunk := range chunks {
		if err := chunk.Validate(); err != nil {
			t.Fatalf("chunks[%d].Validate() error = %v", i, err)
		}
	}
}
