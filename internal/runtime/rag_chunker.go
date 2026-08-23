package runtime

import (
	"context"
	"errors"
	"fmt"
)

// DefaultChunkSize is the character-window size used when a
// CharacterChunkerConfig leaves Size zero.
const DefaultChunkSize = 1000

var errInvalidDocumentUTF8 = errors.New("lebro: document content must be valid UTF-8")

// CharacterChunkerConfig configures a fixed-width character-window chunker.
//
// Size and Overlap are measured in runes, not bytes, so a window is a
// predictable amount of text regardless of encoding and a multi-byte character
// is never split across two chunks.
type CharacterChunkerConfig struct {
	// Size is the maximum number of runes per chunk. A zero value uses
	// DefaultChunkSize.
	Size int
	// Overlap is the number of runes each chunk repeats from the end of the
	// previous one. Overlap keeps a span that straddles a window boundary
	// retrievable from at least one chunk. It must be less than Size, since an
	// overlap at or above the window size would never advance.
	Overlap int
}

// CharacterChunker splits a document into fixed-width, optionally overlapping
// rune windows. It is the deliberately simple initial strategy: it makes no
// assumptions about language, markup, or sentence structure, so it behaves
// identically on prose, code, and structured text.
//
// It is safe for concurrent use; the configuration is fixed at construction.
type CharacterChunker struct {
	size    int
	overlap int
}

var _ Chunker = (*CharacterChunker)(nil)

// NewCharacterChunker validates the configuration and returns a chunker safe
// for concurrent use.
func NewCharacterChunker(config CharacterChunkerConfig) (*CharacterChunker, error) {
	size, err := chunkerSize(config.Size)
	if err != nil {
		return nil, err
	}
	if config.Overlap < 0 {
		return nil, errors.New("lebro: chunk overlap must not be negative")
	}
	if config.Overlap >= size {
		return nil, fmt.Errorf("lebro: chunk overlap %d must be less than chunk size %d", config.Overlap, size)
	}
	return &CharacterChunker{size: size, overlap: config.Overlap}, nil
}

// Chunk splits the document into rune windows of at most the configured size,
// advancing by size-overlap runes per window.
//
// A document shorter than one window yields exactly one chunk. Every chunk
// carries the document's Source and Metadata so provenance survives ingestion,
// and a stable ID derived from the document ID and the chunk's ordinal
// position.
func (c *CharacterChunker) Chunk(ctx context.Context, document Document) ([]Chunk, error) {
	if c == nil {
		return nil, errors.New("lebro: character chunker is nil")
	}
	if err := validateChunkerInput(ctx, document); err != nil {
		return nil, err
	}

	// Convert once: indexing runes directly keeps window boundaries aligned to
	// character boundaries without re-scanning the string per window.
	runes := []rune(document.Content)
	stride := c.size - c.overlap

	chunks := make([]Chunk, 0, (len(runes)/stride)+1)
	for start := 0; start < len(runes); start += stride {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := start + c.size
		if end > len(runes) {
			end = len(runes)
		}
		index := len(chunks)
		chunks = append(chunks, Chunk{
			ID:         ChunkID(document.ID, index),
			DocumentID: document.ID,
			Content:    string(runes[start:end]),
			Source:     document.Source,
			Index:      index,
			Metadata:   cloneRawMessage(document.Metadata),
		})
		// The final window is short whenever the content does not divide evenly.
		// Stopping here keeps overlap from emitting a trailing chunk that is a
		// pure suffix of the one before it.
		if end == len(runes) {
			break
		}
	}

	// No empty-result guard is needed here: Validate has already rejected empty
	// content, and any non-empty valid string yields at least one rune, so the
	// loop above always appends. The Indexer still guards the general Chunker
	// contract, where a custom implementation could return nothing.
	return chunks, nil
}
