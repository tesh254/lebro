package testkit

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tesh254/lebro"
)

// WorkingMemoryContractSuite verifies that durable working memory is isolated
// by namespace and owner, and clears only its own scope. Run it against every
// Store implementation.
func WorkingMemoryContractSuite(t *testing.T, newStore StoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)
	repo := store.WorkingMemory()
	ownerA := lebro.WorkingMemoryScope{Namespace: "tenant-a", OwnerID: "owner-a"}
	ownerB := lebro.WorkingMemoryScope{Namespace: "tenant-a", OwnerID: "owner-b"}
	otherTenant := lebro.WorkingMemoryScope{Namespace: "tenant-b", OwnerID: "owner-a"}
	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)

	for _, item := range []struct {
		scope lebro.WorkingMemoryScope
		value string
	}{
		{ownerA, `"Ada"`},
		{ownerB, `"Grace"`},
		{otherTenant, `"Lin"`},
	} {
		fact, err := repo.UpsertWorkingMemoryFact(ctx, lebro.WorkingMemoryFact{
			ID: "name-" + item.scope.Namespace + "-" + item.scope.OwnerID, Namespace: item.scope.Namespace,
			OwnerID: item.scope.OwnerID, Key: "name", Value: []byte(item.value), CreatedAt: now, UpdatedAt: now,
		}, 0)
		if err != nil || fact.Version != 1 {
			t.Fatalf("create %v = %#v, %v", item.scope, fact, err)
		}
	}

	fact, err := repo.GetWorkingMemoryFact(ctx, ownerA, "name")
	if err != nil || string(fact.Value) != `"Ada"` {
		t.Fatalf("owner A fact = %#v, %v", fact, err)
	}
	fact.Value = []byte(`"Ada Lovelace"`)
	fact.UpdatedAt = now.Add(time.Minute)
	fact, err = repo.UpsertWorkingMemoryFact(ctx, fact, 1)
	if err != nil || fact.Version != 2 {
		t.Fatalf("update = %#v, %v", fact, err)
	}
	if _, err := repo.UpsertWorkingMemoryFact(ctx, fact, 1); !errors.Is(err, lebro.ErrConflict) {
		t.Fatalf("stale update error = %v, want ErrConflict", err)
	}

	page, err := repo.ListWorkingMemoryFacts(ctx, ownerA, lebro.PageRequest{})
	if err != nil || len(page.Records) != 1 || string(page.Records[0].Value) != `"Ada Lovelace"` {
		t.Fatalf("owner A facts = %#v, %v", page, err)
	}
	if err := repo.ClearWorkingMemory(ctx, ownerA); err != nil {
		t.Fatalf("clear owner A: %v", err)
	}
	if _, err := repo.GetWorkingMemoryFact(ctx, ownerA, "name"); !errors.Is(err, lebro.ErrNotFound) {
		t.Fatalf("cleared fact error = %v, want ErrNotFound", err)
	}
	for _, scope := range []lebro.WorkingMemoryScope{ownerB, otherTenant} {
		if _, err := repo.GetWorkingMemoryFact(ctx, scope, "name"); err != nil {
			t.Fatalf("clear leaked into %v: %v", scope, err)
		}
	}
}

// WorkflowCheckpointContractSuite verifies that every completed workflow step
// leaves ordered, durable checkpoint state. Run it against every Store.
func WorkflowCheckpointContractSuite(t *testing.T, newStore StoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)
	workflow, err := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition: lebro.WorkflowDefinition{ID: "checkpoint-contract", Version: "v1"},
		Store:      store,
		IDSource:   lebro.NewFixedIDSource([]lebro.RunID{"checkpoint-run"}, nil),
		Steps: []lebro.Step{
			{Definition: lebro.StepDefinition{ID: "first"}, Handler: lebro.StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`{"step":1}`), nil
			})},
			{Definition: lebro.StepDefinition{ID: "second"}, Handler: lebro.StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`{"step":2}`), nil
			})},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := workflow.Run(ctx, lebro.WorkflowRunInput{Input: json.RawMessage(`{}`)})
	if err != nil || run.Status != lebro.RunStatusSucceeded {
		t.Fatalf("run = %#v, %v", run, err)
	}
	record, err := store.WorkflowRuns().GetWorkflowRun(ctx, run.ID)
	if err != nil || record.Status != lebro.RunStatusSucceeded || len(record.StepOutputs) != 2 {
		t.Fatalf("durable run = %#v, %v", record, err)
	}
	snapshots, err := store.WorkflowSnapshots().ListWorkflowSnapshots(ctx, run.ID, lebro.PageRequest{})
	if err != nil || len(snapshots.Records) != 2 || snapshots.Records[0].Sequence >= snapshots.Records[1].Sequence {
		t.Fatalf("checkpoints = %#v, %v", snapshots, err)
	}
}

// ThreadHistoryContractSuite verifies that deleting a durable message also
// removes its semantic-recall vector while preserving another owner's recall.
func ThreadHistoryContractSuite(t *testing.T, newStore StoreFactory, newVectors VectorStoreFactory) {
	t.Helper()
	ctx := context.Background()
	store, vectors := newStore(t), newVectors(t)
	embedder := contractEmbedder{}
	history, err := lebro.NewThreadHistory(lebro.ThreadHistoryConfig{Store: store, Vectors: vectors, Embeddings: embedder, Index: "thread-history-contract"})
	if err != nil {
		t.Fatal(err)
	}
	if err := history.EnsureIndex(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	for _, thread := range []lebro.ThreadRecord{
		{ID: "owner-a", Namespace: "tenant", OwnerID: "a", CreatedAt: now, UpdatedAt: now},
		{ID: "owner-b", Namespace: "tenant", OwnerID: "b", CreatedAt: now, UpdatedAt: now},
	} {
		if err := store.Threads().CreateThread(ctx, thread); err != nil {
			t.Fatal(err)
		}
	}
	if err := history.AppendMessages(ctx, []lebro.MessageRecord{
		{ID: "delete-me", ThreadID: "owner-a", Message: lebro.Message{Role: lebro.RoleUser, Content: "Nairobi weather"}, CreatedAt: now},
		{ID: "keep-me", ThreadID: "owner-b", Message: lebro.Message{Role: lebro.RoleUser, Content: "Nairobi weather"}, CreatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	if err := history.DeleteMessages(ctx, "owner-a", []string{"delete-me"}); err != nil {
		t.Fatal(err)
	}
	for _, query := range []struct {
		scope lebro.ThreadHistoryScope
		want  string
	}{
		{scope: lebro.ThreadHistoryScope{Namespace: "tenant", OwnerID: "a"}, want: ""},
		{scope: lebro.ThreadHistoryScope{Namespace: "tenant", OwnerID: "b"}, want: "keep-me"},
	} {
		hits, err := history.Retrieve(ctx, lebro.ThreadHistoryQuery{Scope: query.scope, Query: "Nairobi weather"})
		if err != nil {
			t.Fatal(err)
		}
		if query.want == "" && len(hits) != 0 {
			t.Fatalf("deleted owner's hits = %#v", hits)
		}
		if query.want != "" && (len(hits) != 1 || hits[0].MessageID != query.want) {
			t.Fatalf("preserved owner's hits = %#v", hits)
		}
	}
}

type contractEmbedder struct{}

func (contractEmbedder) Dimension() int { return 2 }
func (contractEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	vectors := make([][]float32, len(inputs))
	for i := range inputs {
		vectors[i] = []float32{1, 0}
	}
	return vectors, nil
}
