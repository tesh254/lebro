// Command rag-chunkers demonstrates structure-aware and fixed-window document
// chunking. Both strategies use rune lengths, preserve provenance, and need no
// provider or network connection.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/tesh254/lebro"
)

func main() {
	document := lebro.Document{
		ID:      "guide",
		Content: "Alpha paragraph.\n\nBeta paragraph.\n\nGamma paragraph.",
		Source:  "docs/guide.md",
	}
	recursive := must(lebro.NewRecursiveChunker(lebro.RecursiveChunkerConfig{Size: 20}))
	sliding := must(lebro.NewSlidingWindowChunker(lebro.SlidingWindowChunkerConfig{Size: 20}))

	for _, strategy := range []struct {
		name    string
		chunker lebro.Chunker
	}{{"recursive (paragraph boundaries)", recursive}, {"sliding window (fixed rune boundaries)", sliding}} {
		chunks, err := strategy.chunker.Chunk(context.Background(), document)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s:\n", strategy.name)
		for _, chunk := range chunks {
			fmt.Printf("  %s %q (%s)\n", chunk.ID, chunk.Content, chunk.Source)
		}
	}
}

func must[T any](value T, err error) T {
	if err != nil {
		log.Fatal(err)
	}
	return value
}
