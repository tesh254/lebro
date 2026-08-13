package channels_test

import (
	"context"
	"sync"
	"testing"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/channels"
)

func TestMemoryDeduplicatorDropsRepeat(t *testing.T) {
	dedup := channels.NewMemoryDeduplicator(8)
	ctx := context.Background()

	seen, err := dedup.Seen(ctx, "m1")
	if err != nil {
		t.Fatalf("first Seen: %v", err)
	}
	if seen {
		t.Fatal("first delivery reported as duplicate")
	}
	seen, err = dedup.Seen(ctx, "m1")
	if err != nil {
		t.Fatalf("second Seen: %v", err)
	}
	if !seen {
		t.Fatal("redelivery not reported as duplicate")
	}
}

func TestMemoryDeduplicatorEvictsOldest(t *testing.T) {
	dedup := channels.NewMemoryDeduplicator(2)
	ctx := context.Background()
	for _, key := range []string{"a", "b", "c"} { // "a" is evicted by "c"
		if _, err := dedup.Seen(ctx, key); err != nil {
			t.Fatalf("Seen(%q): %v", key, err)
		}
	}
	// "a" fell out of the 2-entry window, so it is no longer remembered.
	seen, err := dedup.Seen(ctx, "a")
	if err != nil {
		t.Fatalf("Seen(a) after eviction: %v", err)
	}
	if seen {
		t.Fatal("evicted key still reported as duplicate")
	}
	// "c" is still within the window.
	seen, err = dedup.Seen(ctx, "c")
	if err != nil {
		t.Fatalf("Seen(c): %v", err)
	}
	if !seen {
		t.Fatal("retained key not reported as duplicate")
	}
}

func TestStoreDeduplicatorPersistsAcrossInstances(t *testing.T) {
	store := lebro.NewMemoryStore()
	ctx := context.Background()

	first, err := channels.NewStoreDeduplicator(channels.StoreDeduplicatorConfig{Store: store, Namespace: "prod"})
	if err != nil {
		t.Fatalf("NewStoreDeduplicator: %v", err)
	}
	seen, err := first.Seen(ctx, "m1")
	if err != nil {
		t.Fatalf("first Seen: %v", err)
	}
	if seen {
		t.Fatal("first delivery reported as duplicate")
	}

	// A fresh deduplicator over the same store models a process restart: the
	// record persists, so the earlier key is still remembered.
	second, err := channels.NewStoreDeduplicator(channels.StoreDeduplicatorConfig{Store: store, Namespace: "prod"})
	if err != nil {
		t.Fatalf("NewStoreDeduplicator: %v", err)
	}
	seen, err = second.Seen(ctx, "m1")
	if err != nil {
		t.Fatalf("second Seen: %v", err)
	}
	if !seen {
		t.Fatal("persisted key not reported as duplicate after restart")
	}
}

func TestStoreDeduplicatorConcurrentSameKey(t *testing.T) {
	store := lebro.NewMemoryStore()
	dedup, err := channels.NewStoreDeduplicator(channels.StoreDeduplicatorConfig{Store: store})
	if err != nil {
		t.Fatalf("NewStoreDeduplicator: %v", err)
	}
	ctx := context.Background()

	const workers = 16
	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		notSeenWins int
		errs        int
	)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			seen, err := dedup.Seen(ctx, "same")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs++
				return
			}
			if !seen {
				notSeenWins++
			}
		}()
	}
	wg.Wait()

	if errs != 0 {
		t.Fatalf("Seen returned %d errors under contention", errs)
	}
	// Exactly one concurrent delivery of the same key may observe "not seen";
	// every other must be told it is a duplicate.
	if notSeenWins != 1 {
		t.Fatalf("expected exactly 1 processor of the key, got %d", notSeenWins)
	}
}

func TestNewStoreDeduplicatorRequiresStore(t *testing.T) {
	if _, err := channels.NewStoreDeduplicator(channels.StoreDeduplicatorConfig{}); err == nil {
		t.Fatal("NewStoreDeduplicator with no store returned nil error")
	}
}
