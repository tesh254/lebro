package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestThreadHistoryIndexesScopesUpdatesAndDeletes(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := store.CreateThread(ctx, ThreadRecord{ID: "thread", Namespace: "tenant", OwnerID: "owner", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	embedder := newStubEmbedder(8)
	history, err := NewThreadHistory(ThreadHistoryConfig{Store: store, Vectors: NewMemoryVectorStore(), Embeddings: embedder, Index: "history"})
	if err != nil {
		t.Fatal(err)
	}
	if err := history.EnsureIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if err := history.AppendMessages(ctx, []MessageRecord{
		{ID: "old", ThreadID: "thread", Message: Message{Role: RoleUser, Content: "Nairobi weather"}, CreatedAt: now},
		{ID: "new", ThreadID: "thread", Message: Message{Role: RoleUser, Content: "Nairobi weather"}, CreatedAt: now.Add(time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := history.Retrieve(ctx, ThreadHistoryQuery{Scope: ThreadHistoryScope{Namespace: "tenant", OwnerID: "owner"}, Query: "Nairobi weather", TopK: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].MessageID != "new" {
		t.Fatalf("hits = %#v, want newest equal-score message first", hits)
	}
	otherTenant, err := history.Retrieve(ctx, ThreadHistoryQuery{Scope: ThreadHistoryScope{Namespace: "tenant", OwnerID: "other"}, Query: "Nairobi weather"})
	if err != nil {
		t.Fatal(err)
	}
	if len(otherTenant) != 0 {
		t.Fatalf("cross-owner hits = %#v", otherTenant)
	}
	budgeted, err := history.Retrieve(ctx, ThreadHistoryQuery{Scope: ThreadHistoryScope{Namespace: "tenant", OwnerID: "owner"}, Query: "Nairobi weather", TopK: 2, MaxTokens: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(budgeted) != 1 || budgeted[0].MessageID != "new" {
		t.Fatalf("budgeted hits = %#v", budgeted)
	}
	if err := history.UpdateMessages(ctx, []MessageRecord{{ID: "old", ThreadID: "thread", Message: Message{Role: RoleUser, Content: "Mombasa weather"}}}); err != nil {
		t.Fatal(err)
	}
	batches := embedder.recordedBatches()
	if got := batches[len(batches)-1]; len(got) != 1 || got[0] != "Mombasa weather" {
		t.Fatalf("update embedding batch = %#v, want changed message only", got)
	}
	hits, err = history.Retrieve(ctx, ThreadHistoryQuery{Scope: ThreadHistoryScope{Namespace: "tenant", OwnerID: "owner"}, Query: "Mombasa weather"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].MessageID != "old" {
		t.Fatalf("updated hits = %#v", hits)
	}
	if err := history.DeleteMessages(ctx, "thread", []string{"old"}); err != nil {
		t.Fatal(err)
	}
	vectors, err := history.embeddings.Embed(ctx, []string{"Mombasa weather"})
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := history.vectors.Search(ctx, SimilarityQuery{Index: "history", Vector: vectors[0], TopK: 1, Filter: VectorMetadataFilter{Match: map[string]json.RawMessage{"message_id": json.RawMessage(`"old"`)}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("deleted message remains indexed: %#v", remaining)
	}
}
