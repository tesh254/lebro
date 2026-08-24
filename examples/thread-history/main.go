// thread-history stores conversation messages and recalls relevant history
// within one tenant and owner scope.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/tesh254/lebro"
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
	now := time.Now().UTC()
	if err := store.CreateThread(ctx, lebro.ThreadRecord{ID: "thread-1", Namespace: "acme", OwnerID: "user-1", CreatedAt: now, UpdatedAt: now}); err != nil {
		return err
	}
	history, err := lebro.NewThreadHistory(lebro.ThreadHistoryConfig{Store: store, Vectors: lebro.NewMemoryVectorStore(), Embeddings: localEmbedder{}, Index: "thread-history"})
	if err != nil {
		return err
	}
	if err := history.EnsureIndex(ctx); err != nil {
		return err
	}
	if err := history.AppendMessages(ctx, []lebro.MessageRecord{{ID: "m1", ThreadID: "thread-1", Message: lebro.Message{Role: lebro.RoleUser, Content: "Deploy to Nairobi tomorrow"}, CreatedAt: now}}); err != nil {
		return err
	}
	hits, err := history.Retrieve(ctx, lebro.ThreadHistoryQuery{Scope: lebro.ThreadHistoryScope{Namespace: "acme", OwnerID: "user-1"}, Query: "Where is deployment?", TopK: 1, MaxTokens: 64})
	if err != nil {
		return err
	}
	for _, hit := range hits {
		if _, err := fmt.Fprintln(output, hit.Content); err != nil {
			return err
		}
	}
	return nil
}

type localEmbedder struct{}

func (localEmbedder) Dimension() int { return 2 }
func (localEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	vectors := make([][]float32, len(inputs))
	for i, input := range inputs {
		vectors[i] = []float32{float32(len(input)), 1}
	}
	return vectors, nil
}
