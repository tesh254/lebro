package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

var errInvalidDocumentUTF8 = errors.New("lebro: document content must be valid UTF-8")

// RecursiveChunkerConfig configures a chunker that prefers structural text
// boundaries before falling back to individual runes. Size and Overlap are
// measured in runes.
type RecursiveChunkerConfig struct {
	// Size is the maximum number of runes per chunk. A zero value uses
	// DefaultChunkSize.
	Size int
	// Overlap is the maximum number of trailing runes from one chunk copied to
	// its successor. It must be less than Size.
	Overlap int
	// Separators is an ordered list of preferred boundaries. A nil value uses
	// paragraph, line, and word boundaries. The empty-string rune fallback is
	// always added when omitted, ensuring no valid document is unsplittable.
	Separators []string
}

// RecursiveChunker splits documents at the first available preferred boundary,
// recursively trying less-preferred boundaries only for an oversized span.
// Separators stay with the preceding span, preserving document text exactly.
// It is safe for concurrent use after construction.
type RecursiveChunker struct {
	size       int
	overlap    int
	separators []string
}

var _ Chunker = (*RecursiveChunker)(nil)

// NewRecursiveChunker validates config and returns a structural chunker.
func NewRecursiveChunker(config RecursiveChunkerConfig) (*RecursiveChunker, error) {
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
	separators := config.Separators
	if separators == nil {
		separators = []string{"\n\n", "\n", " ", ""}
	} else {
		separators = append([]string(nil), separators...)
		if len(separators) == 0 || separators[len(separators)-1] != "" {
			separators = append(separators, "")
		}
	}
	return &RecursiveChunker{size: size, overlap: config.Overlap, separators: separators}, nil
}

// Chunk applies document validation before preserving input in ordered, bounded
// chunks. Every returned chunk has stable identity and copied provenance.
func (c *RecursiveChunker) Chunk(ctx context.Context, document Document) ([]Chunk, error) {
	if c == nil {
		return nil, errors.New("lebro: recursive chunker is nil")
	}
	if err := validateChunkerInput(ctx, document); err != nil {
		return nil, err
	}
	contents, err := mergeChunkParts(ctx, c.split(document.Content), c.size, c.overlap)
	if err != nil {
		return nil, err
	}
	return chunkContents(document, contents), nil
}

func (c *RecursiveChunker) split(text string) []string {
	return c.splitAt(text, c.separators)
}

func (c *RecursiveChunker) splitAt(text string, separators []string) []string {
	if utf8.RuneCountInString(text) <= c.size || len(separators) == 0 {
		return []string{text}
	}
	separatorIndex := len(separators) - 1
	for i, separator := range separators {
		if separator == "" || strings.Contains(text, separator) {
			separatorIndex = i
			break
		}
	}
	separator := separators[separatorIndex]
	if separator == "" {
		return splitRunes(text)
	}
	parts := strings.SplitAfter(text, separator)
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		if utf8.RuneCountInString(part) <= c.size {
			result = append(result, part)
			continue
		}
		result = append(result, c.splitAt(part, separators[separatorIndex+1:])...)
	}
	return result
}

// SlidingWindowChunkerConfig configures explicit fixed-width rune windows.
// It has the same fields and behavior as CharacterChunkerConfig, but names the
// strategy used by common RAG APIs.
type SlidingWindowChunkerConfig struct {
	Size    int
	Overlap int
}

// SlidingWindowChunker exposes fixed-width, optionally overlapping rune
// windows under an explicit strategy name. It preserves CharacterChunker's
// deterministic behavior for compatibility.
type SlidingWindowChunker struct{ character *CharacterChunker }

var _ Chunker = (*SlidingWindowChunker)(nil)

// NewSlidingWindowChunker validates config and returns a rune-safe fixed-window
// chunker.
func NewSlidingWindowChunker(config SlidingWindowChunkerConfig) (*SlidingWindowChunker, error) {
	character, err := NewCharacterChunker(CharacterChunkerConfig(config))
	if err != nil {
		return nil, err
	}
	return &SlidingWindowChunker{character: character}, nil
}

// Chunk delegates to CharacterChunker so both public strategy names retain
// identical validation, UTF-8, provenance, and overlap semantics.
func (c *SlidingWindowChunker) Chunk(ctx context.Context, document Document) ([]Chunk, error) {
	if c == nil || c.character == nil {
		return nil, errors.New("lebro: sliding-window chunker is nil")
	}
	return c.character.Chunk(ctx, document)
}

func chunkerSize(size int) (int, error) {
	if size == 0 {
		return DefaultChunkSize, nil
	}
	if size < 0 {
		return 0, errors.New("lebro: chunk size must not be negative")
	}
	return size, nil
}

func validateChunkerInput(ctx context.Context, document Document) error {
	if ctx == nil {
		return errors.New("lebro: chunker context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := document.Validate(); err != nil {
		return &RAGError{Kind: RAGErrorInvalidDocument, DocumentID: document.ID, Err: err}
	}
	if !utf8.ValidString(document.Content) {
		return &RAGError{Kind: RAGErrorInvalidDocument, DocumentID: document.ID, Err: errInvalidDocumentUTF8}
	}
	return nil
}

func splitRunes(text string) []string {
	runes := []rune(text)
	parts := make([]string, len(runes))
	for i, rune := range runes {
		parts[i] = string(rune)
	}
	return parts
}

func mergeChunkParts(ctx context.Context, parts []string, size, overlap int) ([]string, error) {
	chunks := make([]string, 0)
	current := ""
	for _, part := range parts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if part == "" {
			continue
		}
		partRunes := utf8.RuneCountInString(part)
		if utf8.RuneCountInString(current)+partRunes <= size {
			current += part
			continue
		}
		if current != "" {
			chunks = append(chunks, current)
		}
		prefixRunes := []rune(suffixRunes(current, overlap))
		prefixStart := 0
		for len(prefixRunes)-prefixStart+partRunes > size {
			prefixStart++
		}
		current = string(prefixRunes[prefixStart:]) + part
	}
	if current != "" {
		chunks = append(chunks, current)
	}
	return chunks, nil
}

func suffixRunes(text string, count int) string {
	runes := []rune(text)
	if count >= len(runes) {
		return text
	}
	return string(runes[len(runes)-count:])
}

func chunkContents(document Document, contents []string) []Chunk {
	chunks := make([]Chunk, len(contents))
	for index, content := range contents {
		chunks[index] = Chunk{ID: ChunkID(document.ID, index), DocumentID: document.ID, Content: content, Source: document.Source, Index: index, Metadata: cloneRawMessage(document.Metadata)}
	}
	return chunks
}
