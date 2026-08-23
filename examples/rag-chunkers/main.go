// Command rag-chunkers demonstrates structure-aware and fixed-window document
// chunking. Both strategies use rune lengths, preserve provenance, and need no
// provider or network connection.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/tesh254/lebro"
)

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(output io.Writer) error {
	document := lebro.Document{
		ID:      "guide",
		Content: "Alpha paragraph.\n\nBeta paragraph.\n\nGamma paragraph.",
		Source:  "docs/guide.md",
	}
	recursive, err := lebro.NewRecursiveChunker(lebro.RecursiveChunkerConfig{Size: 20})
	if err != nil {
		return err
	}
	sliding, err := lebro.NewSlidingWindowChunker(lebro.SlidingWindowChunkerConfig{Size: 20})
	if err != nil {
		return err
	}

	for _, strategy := range []struct {
		name    string
		chunker lebro.Chunker
	}{{"recursive (paragraph boundaries)", recursive}, {"sliding window (fixed rune boundaries)", sliding}} {
		chunks, err := strategy.chunker.Chunk(context.Background(), document)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(output, "%s:\n", strategy.name); err != nil {
			return err
		}
		for _, chunk := range chunks {
			if _, err := fmt.Fprintf(output, "  %s %q (%s)\n", chunk.ID, chunk.Content, chunk.Source); err != nil {
				return err
			}
		}
	}
	return nil
}
