// thread-history stores conversation messages and recalls relevant history
// within one tenant and owner scope.
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/tesh254/lebro"
)

func main() {
	ctx := context.Background()
	store := lebro.NewMemoryStore()
	now := time.Now().UTC()
	must(store.CreateThread(ctx, lebro.ThreadRecord{ID: "thread-1", Namespace: "acme", OwnerID: "user-1", CreatedAt: now, UpdatedAt: now}))
	history := mustValue(lebro.NewThreadHistory(lebro.ThreadHistoryConfig{Store: store, Vectors: lebro.NewMemoryVectorStore(), Embeddings: localEmbedder{}, Index: "thread-history"}))
	must(history.EnsureIndex(ctx))
	must(history.AppendMessages(ctx, []lebro.MessageRecord{{ID: "m1", ThreadID: "thread-1", Message: lebro.Message{Role: lebro.RoleUser, Content: "Deploy to Nairobi tomorrow"}, CreatedAt: now}}))
	hits := mustValue(history.Retrieve(ctx, lebro.ThreadHistoryQuery{Scope: lebro.ThreadHistoryScope{Namespace: "acme", OwnerID: "user-1"}, Query: "Where is deployment?", TopK: 1, MaxTokens: 64}))
	for _, hit := range hits {
		fmt.Println(hit.Content)
	}
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
func must(err error) {
	if err != nil {
		panic(err)
	}
}
func mustValue[T any](value T, err error) T { must(err); return value }
