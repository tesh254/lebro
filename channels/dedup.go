package channels

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
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

// StoreDeduplicator persists processed message keys through a lebro.Store so
// deduplication survives a restart. Each processed key becomes its own marker
// thread record whose ID is a hash of the key; recording a key is a single
// CreateThread guarded by the store's per-record uniqueness. This makes the
// check-and-record atomic on every backend, including a read-committed
// PostgreSQL where a shared mutable ring would let two concurrent writers each
// overwrite the other: two concurrent creates of the same marker cannot both
// succeed, so exactly one caller observes "not seen".
//
// Unlike MemoryDeduplicator, StoreDeduplicator does not bound its retained
// window: a marker persists until the store is pruned externally, so a
// redelivery is recognized however late it arrives. The tradeoff is that the
// marker set grows with the number of distinct messages processed; deployments
// that need bounded storage should prune old marker records out of band. Marker
// IDs are prefixed with "chdedup-" so such pruning can target them.
type StoreDeduplicator struct {
	store     lebro.Store
	namespace string
	clock     func() time.Time
}

// StoreDeduplicatorConfig configures a StoreDeduplicator.
type StoreDeduplicatorConfig struct {
	// Store persists the marker records. Required.
	Store lebro.Store
	// Namespace isolates one logical scope's markers from another's sharing the
	// same store. It is folded into every marker ID, so two servers — or two
	// adapters routed through separate deduplicators — do not treat an equal
	// provider key as the same message. The channel Server sets this per
	// agent-platform route.
	Namespace string
}

// DefaultDedupCapacity is the retained-key window used by NewMemoryDeduplicator
// when it is left unset. StoreDeduplicator does not use it; its retention is
// bounded only by external pruning.
const DefaultDedupCapacity = 1024

// NewStoreDeduplicator constructs a StoreDeduplicator. It returns an error when
// no store is configured, because a persistent deduplicator without storage
// cannot record anything.
func NewStoreDeduplicator(config StoreDeduplicatorConfig) (*StoreDeduplicator, error) {
	if config.Store == nil {
		return nil, errors.New("lebro/channels: StoreDeduplicator requires a Store")
	}
	return &StoreDeduplicator{
		store:     config.Store,
		namespace: config.Namespace,
		clock:     time.Now,
	}, nil
}

// markerID is the deterministic ID of the marker for one key. Folding the
// namespace and a separator into the hash keeps two scopes' keys apart and
// keeps a key that contains the separator from colliding with a different
// (namespace, key) split.
func (d *StoreDeduplicator) markerID(key string) lebro.ThreadID {
	sum := sha256.Sum256(lengthPrefixed(d.namespace, key))
	return lebro.ThreadID("chdedup-" + hex.EncodeToString(sum[:]))
}

// Seen reports whether key was already recorded and records it otherwise. The
// record is a single marker thread created under the store's uniqueness
// guarantee, so the check and the record are one atomic step.
func (d *StoreDeduplicator) Seen(ctx context.Context, key string) (bool, error) {
	id := d.markerID(key)

	// A prior run may already have recorded this key. Checking first turns the
	// common redelivery case into a read, and distinguishes a genuine duplicate
	// from a real create failure below.
	if _, err := d.store.Threads().GetThread(ctx, id); err == nil {
		return true, nil
	} else if !errors.Is(err, lebro.ErrNotFound) {
		return false, fmt.Errorf("lebro/channels: deduplicate %q: %w", key, err)
	}

	now := d.clock().UTC()
	err := d.store.Threads().CreateThread(ctx, lebro.ThreadRecord{
		ID:        id,
		Namespace: d.namespace,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err == nil {
		return false, nil
	}

	// The create failed. A concurrent delivery of the same key may have created
	// the marker between the Get and the Create; the store reports that as a
	// uniqueness error (ErrConflict on PostgreSQL, a plain "already exists" on
	// the in-memory and SQLite stores). Re-read to tell that apart from a real
	// storage failure: if the marker now exists, this delivery is the duplicate.
	if _, getErr := d.store.Threads().GetThread(ctx, id); getErr == nil {
		return true, nil
	}
	return false, fmt.Errorf("lebro/channels: deduplicate %q: %w", key, err)
}

// lengthPrefixed encodes fields so no field's contents can shift a boundary and
// alias a different split — ("a","bc") and ("ab","c") encode differently. Each
// field is prefixed with its byte length and a separator.
func lengthPrefixed(fields ...string) []byte {
	var buf []byte
	for _, field := range fields {
		buf = append(buf, []byte(strconv.Itoa(len(field)))...)
		buf = append(buf, ':')
		buf = append(buf, []byte(field)...)
	}
	return buf
}

var (
	_ Deduplicator = (*MemoryDeduplicator)(nil)
	_ Deduplicator = (*StoreDeduplicator)(nil)
)
