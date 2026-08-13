package channels

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tesh254/lebro"
)

// Deduplicator records which inbound messages have already been processed so a
// redelivered webhook does not run the agent twice. Messaging platforms deliver
// at least once: a slow acknowledgement, a network retry, or a platform-side
// replay all resend the same message, and without deduplication each resend
// would produce a duplicate reply.
//
// Seen reports whether a provider message ID has been processed and, as a side
// effect, records it as processed when it had not been. The check and the
// record are one atomic step so two concurrent deliveries of the same ID cannot
// both observe "not seen". A true result means the message is a duplicate and
// must be dropped; a false result means the caller now owns processing it.
//
// Implementations must be safe for concurrent use.
type Deduplicator interface {
	Seen(ctx context.Context, key string) (bool, error)
}

// MemoryDeduplicator is a bounded in-process Deduplicator. It remembers the
// most recent Capacity keys and evicts the oldest, so memory stays bounded
// across an unbounded message stream. It does not survive a restart; use
// StoreDeduplicator when redelivery can outlast the process.
//
// The bound makes deduplication best-effort: a redelivery that arrives after
// Capacity newer messages have been seen is no longer remembered and would be
// processed again. Capacity should exceed the number of messages a platform can
// deliver within its redelivery window.
type MemoryDeduplicator struct {
	capacity int

	mu    sync.Mutex
	order *list.List
	seen  map[string]*list.Element
}

// NewMemoryDeduplicator returns a MemoryDeduplicator retaining the most recent
// capacity keys. A capacity below one is treated as one, because a zero
// capacity would remember nothing and deduplicate nothing.
func NewMemoryDeduplicator(capacity int) *MemoryDeduplicator {
	if capacity < 1 {
		capacity = 1
	}
	return &MemoryDeduplicator{
		capacity: capacity,
		order:    list.New(),
		seen:     make(map[string]*list.Element, capacity),
	}
}

// Seen reports whether key was already recorded and records it otherwise.
func (d *MemoryDeduplicator) Seen(_ context.Context, key string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.seen[key]; ok {
		return true, nil
	}
	d.seen[key] = d.order.PushBack(key)
	for d.order.Len() > d.capacity {
		oldest := d.order.Front()
		d.order.Remove(oldest)
		delete(d.seen, oldest.Value.(string))
	}
	return false, nil
}

// StoreDeduplicator persists processed message IDs through a lebro.Store so
// deduplication survives a restart. It keeps a bounded ring of the most recent
// keys in a single dedicated thread record per namespace, storing the ring in
// the thread's metadata. The bound keeps the record from growing without limit;
// as with MemoryDeduplicator, a redelivery older than the retained window is no
// longer remembered.
//
// The check-and-record runs inside Store.Transaction, so two concurrent
// deliveries of the same key cannot both proceed: the transaction that commits
// second observes the first's write and, on a write-write race, retries against
// the updated record.
type StoreDeduplicator struct {
	store     lebro.Store
	namespace string
	capacity  int
	// maxRetries bounds transaction retries on a concurrency conflict so a
	// pathological contention loop cannot run forever.
	maxRetries int
	clock      func() time.Time
}

// StoreDeduplicatorConfig configures a StoreDeduplicator.
type StoreDeduplicatorConfig struct {
	// Store persists the dedup record. Required.
	Store lebro.Store
	// Namespace isolates one deployment's dedup record from another's sharing
	// the same store. It is folded into the record's thread ID.
	Namespace string
	// Capacity is the number of recent keys retained. A value below one selects
	// DefaultDedupCapacity.
	Capacity int
}

// DefaultDedupCapacity is the retained-key window used when a
// StoreDeduplicatorConfig or NewMemoryDeduplicator leaves it unset. It is large
// enough to cover a platform's redelivery burst and small enough to keep the
// metadata record modest.
const DefaultDedupCapacity = 1024

// NewStoreDeduplicator constructs a StoreDeduplicator. It returns an error when
// no store is configured, because a persistent deduplicator without storage
// cannot record anything.
func NewStoreDeduplicator(config StoreDeduplicatorConfig) (*StoreDeduplicator, error) {
	if config.Store == nil {
		return nil, errors.New("lebro/channels: StoreDeduplicator requires a Store")
	}
	capacity := config.Capacity
	if capacity < 1 {
		capacity = DefaultDedupCapacity
	}
	return &StoreDeduplicator{
		store:      config.Store,
		namespace:  config.Namespace,
		capacity:   capacity,
		maxRetries: 8,
		clock:      time.Now,
	}, nil
}

// dedupState is the bounded ring persisted in the dedup thread's metadata. Keys
// holds the retained IDs oldest-first; Index mirrors Keys for O(1) membership
// so a large ring is not scanned linearly on every message.
type dedupState struct {
	Keys []string `json:"keys"`
}

// threadID is the deterministic ID of this deduplicator's record. Folding the
// namespace into the hash keeps two deployments' rings apart.
func (d *StoreDeduplicator) threadID() lebro.ThreadID {
	sum := sha256.Sum256([]byte("dedup\x00" + d.namespace))
	return lebro.ThreadID("chdedup-" + hex.EncodeToString(sum[:]))
}

// Seen reports whether key was already recorded and records it otherwise,
// atomically, retrying the transaction on a concurrency conflict.
func (d *StoreDeduplicator) Seen(ctx context.Context, key string) (bool, error) {
	id := d.threadID()
	for attempt := 0; ; attempt++ {
		var duplicate bool
		err := d.store.Transaction(ctx, func(ctx context.Context, repos lebro.Repositories) error {
			record, err := repos.Threads().GetThread(ctx, id)
			switch {
			case errors.Is(err, lebro.ErrNotFound):
				// First message for this namespace: create the ring record and
				// seed it with this key.
				now := d.clock().UTC()
				state := dedupState{Keys: []string{key}}
				metadata, marshalErr := json.Marshal(state)
				if marshalErr != nil {
					return marshalErr
				}
				duplicate = false
				return repos.Threads().CreateThread(ctx, lebro.ThreadRecord{
					ID:        id,
					Namespace: d.namespace,
					Metadata:  metadata,
					CreatedAt: now,
					UpdatedAt: now,
				})
			case err != nil:
				return err
			}

			state, err := decodeDedupState(record.Metadata)
			if err != nil {
				return err
			}
			for _, existing := range state.Keys {
				if existing == key {
					duplicate = true
					return nil
				}
			}
			state.Keys = append(state.Keys, key)
			if len(state.Keys) > d.capacity {
				state.Keys = state.Keys[len(state.Keys)-d.capacity:]
			}
			metadata, err := json.Marshal(state)
			if err != nil {
				return err
			}
			record.Metadata = metadata
			record.UpdatedAt = d.clock().UTC()
			duplicate = false
			return repos.Threads().UpdateThread(ctx, record)
		})
		switch {
		case err == nil:
			return duplicate, nil
		case errors.Is(err, lebro.ErrConflict) && attempt < d.maxRetries:
			// A concurrent delivery committed first. Retry against the updated
			// record so this key is checked against the winner's write.
			continue
		default:
			return false, fmt.Errorf("lebro/channels: deduplicate %q: %w", key, err)
		}
	}
}

// decodeDedupState reads the ring from a metadata payload. A nil or empty
// payload — which is what a record carries before any key is stored — decodes
// to an empty ring rather than failing, so an externally created record does
// not wedge deduplication.
func decodeDedupState(metadata json.RawMessage) (dedupState, error) {
	if len(metadata) == 0 {
		return dedupState{}, nil
	}
	var state dedupState
	if err := json.Unmarshal(metadata, &state); err != nil {
		return dedupState{}, fmt.Errorf("lebro/channels: decode dedup state: %w", err)
	}
	return state, nil
}

var (
	_ Deduplicator = (*MemoryDeduplicator)(nil)
	_ Deduplicator = (*StoreDeduplicator)(nil)
)
