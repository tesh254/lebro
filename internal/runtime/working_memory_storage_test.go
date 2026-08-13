package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tesh254/lebro"
)

func TestWorkingMemoryFactContract(t *testing.T) {
	stores := map[string]func(*testing.T) lebro.Store{
		"memory": func(*testing.T) lebro.Store { return lebro.NewMemoryStore() },
		"sqlite": func(t *testing.T) lebro.Store {
			s, err := lebro.NewSQLiteStore(t.TempDir() + "/memory.db")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			if err := s.Migrate(context.Background()); err != nil {
				t.Fatal(err)
			}
			return s
		},
	}
	for name, newStore := range stores {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := newStore(t)
			repo := store.WorkingMemory()
			scope := lebro.WorkingMemoryScope{Namespace: "tenant-a", OwnerID: "user-a"}
			now := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
			fact, err := repo.UpsertWorkingMemoryFact(ctx, lebro.WorkingMemoryFact{ID: "fact-1", Namespace: scope.Namespace, OwnerID: scope.OwnerID, Key: "name", Value: []byte(`"Ada"`), CreatedAt: now, UpdatedAt: now}, 0)
			if err != nil || fact.Version != 1 {
				t.Fatalf("create = %#v, %v", fact, err)
			}
			if _, err := repo.UpsertWorkingMemoryFact(ctx, fact, 0); !errors.Is(err, lebro.ErrConflict) {
				t.Fatalf("duplicate create = %v, want ErrConflict", err)
			}
			fact.Value = []byte(`"Grace"`)
			fact.UpdatedAt = now.Add(time.Minute)
			fact, err = repo.UpsertWorkingMemoryFact(ctx, fact, 1)
			if err != nil || fact.Version != 2 {
				t.Fatalf("update = %#v, %v", fact, err)
			}
			if _, err := repo.UpsertWorkingMemoryFact(ctx, fact, 1); !errors.Is(err, lebro.ErrConflict) {
				t.Fatalf("stale update = %v, want ErrConflict", err)
			}
			if _, err := repo.GetWorkingMemoryFact(ctx, lebro.WorkingMemoryScope{Namespace: "tenant-b", OwnerID: "user-a"}, "name"); !errors.Is(err, lebro.ErrNotFound) {
				t.Fatalf("cross-tenant get = %v, want ErrNotFound", err)
			}
			page, err := repo.ListWorkingMemoryFacts(ctx, scope, lebro.PageRequest{Limit: 1})
			if err != nil || len(page.Records) != 1 {
				t.Fatalf("list = %#v, %v", page, err)
			}
			if err := repo.DeleteWorkingMemoryFact(ctx, scope, "name", 1); !errors.Is(err, lebro.ErrConflict) {
				t.Fatalf("stale delete = %v, want ErrConflict", err)
			}
			if err := repo.DeleteWorkingMemoryFact(ctx, scope, "name", 2); err != nil {
				t.Fatal(err)
			}
			if err := repo.ClearWorkingMemory(ctx, scope); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWorkingMemoryIdentityScope(t *testing.T) {
	store := lebro.NewMemoryStore()
	ctx := lebro.WithIdentity(context.Background(), lebro.Identity{Tenant: "tenant-a", Subject: "user-a"})
	_, err := store.WorkingMemory().UpsertWorkingMemoryFact(ctx, lebro.WorkingMemoryFact{ID: "x", Namespace: "tenant-b", OwnerID: "user-a", Key: "k", Value: []byte(`true`)}, 0)
	if !errors.Is(err, lebro.ErrPolicyDenied) {
		t.Fatalf("scope mismatch = %v, want ErrPolicyDenied", err)
	}
}
