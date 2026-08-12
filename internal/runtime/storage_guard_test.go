package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

// actionPolicy denies exactly one action and allows the rest, so a test can
// pin down which operation the guard authorized.
type actionPolicy struct {
	deny Action
	seen []Action
}

func (p *actionPolicy) Authorize(_ context.Context, _ Identity, action Action, _ Resource) Decision {
	p.seen = append(p.seen, action)
	if action == p.deny {
		return Deny("blocked")
	}
	return Allow()
}

func newGuardedStore(t *testing.T, policy Policy) *PolicyStore {
	t.Helper()
	guarded, err := NewPolicyStore(NewMemoryStore(), policy)
	if err != nil {
		t.Fatalf("NewPolicyStore: %v", err)
	}
	return guarded
}

func TestPolicyStoreRequiresStore(t *testing.T) {
	if _, err := NewPolicyStore(nil, AllowAllPolicy{}); err == nil {
		t.Fatal("expected error for nil store")
	}
}

func TestPolicyStoreNilPolicyPassesThrough(t *testing.T) {
	guarded := newGuardedStore(t, nil)
	now := time.Unix(0, 0).UTC()

	if err := guarded.Threads().CreateThread(context.Background(), ThreadRecord{ID: "t1", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create with nil policy: %v", err)
	}
	if _, err := guarded.Threads().GetThread(context.Background(), "t1"); err != nil {
		t.Fatalf("get with nil policy: %v", err)
	}
}

func TestPolicyStoreAllowsWrites(t *testing.T) {
	policy := &actionPolicy{deny: ActionStorageRead}
	guarded := newGuardedStore(t, policy)
	now := time.Unix(0, 0).UTC()

	if err := guarded.Threads().CreateThread(context.Background(), ThreadRecord{ID: "t1", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("write should be allowed: %v", err)
	}
	if len(policy.seen) != 1 || policy.seen[0] != ActionStorageWrite {
		t.Fatalf("expected one storage.write authorization, got %v", policy.seen)
	}
}

func TestPolicyStoreDeniesReadAndIsTyped(t *testing.T) {
	policy := &actionPolicy{deny: ActionStorageRead}
	guarded := newGuardedStore(t, policy)

	_, err := guarded.Threads().GetThread(context.Background(), "t1")
	if err == nil {
		t.Fatal("expected read denial")
	}
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("denial must match ErrPolicyDenied: %v", err)
	}
	var denial *PolicyDenial
	if !errors.As(err, &denial) {
		t.Fatalf("denial must be *PolicyDenial: %v", err)
	}
	if denial.Action != ActionStorageRead || denial.Resource.Kind != ResourceKindThread {
		t.Fatalf("denial lost context: %+v", denial)
	}
}

func TestPolicyStoreDeniesWriteBeforeDelegating(t *testing.T) {
	inner := NewMemoryStore()
	guarded, err := NewPolicyStore(inner, &actionPolicy{deny: ActionStorageWrite})
	if err != nil {
		t.Fatalf("NewPolicyStore: %v", err)
	}
	now := time.Unix(0, 0).UTC()

	writeErr := guarded.Threads().CreateThread(context.Background(), ThreadRecord{ID: "t1", CreatedAt: now, UpdatedAt: now})
	if !errors.Is(writeErr, ErrPolicyDenied) {
		t.Fatalf("expected write denial: %v", writeErr)
	}
	// The underlying store must not have been mutated by a denied write.
	if _, err := inner.Threads().GetThread(context.Background(), "t1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("denied write must not reach the store, got %v", err)
	}
}

func TestPolicyStoreTransactionRepositoriesAreGuarded(t *testing.T) {
	guarded := newGuardedStore(t, &actionPolicy{deny: ActionStorageWrite})

	err := guarded.Transaction(context.Background(), func(ctx context.Context, repos Repositories) error {
		return repos.Messages().AppendMessages(ctx, []MessageRecord{{ID: "m1", ThreadID: "t1"}})
	})
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("transaction repositories must enforce policy: %v", err)
	}
}
