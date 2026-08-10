package runtime

import (
	"errors"
	"fmt"
	"sync"
)

// ProviderID is a stable identifier for a model provider adapter.
type ProviderID string

// ProviderCapabilities describes what a provider adapter supports. The router
// uses these to filter providers before routing a request.
type ProviderCapabilities struct {
	SupportsTools            bool
	SupportsStructuredOutput bool
	SupportsStreaming        bool
}

// Satisfies reports whether caps meets the requirements declared by req.
func (caps ProviderCapabilities) Satisfies(req ProviderCapabilities) bool {
	if req.SupportsTools && !caps.SupportsTools {
		return false
	}
	if req.SupportsStructuredOutput && !caps.SupportsStructuredOutput {
		return false
	}
	if req.SupportsStreaming && !caps.SupportsStreaming {
		return false
	}
	return true
}

// ProviderEntry binds a provider identity to its adapter and capabilities.
type ProviderEntry struct {
	ID           ProviderID
	Model        Model
	Capabilities ProviderCapabilities
}

// Validate checks the invariants a registry entry must satisfy.
func (e ProviderEntry) Validate() error {
	if e.ID == "" {
		return errors.New("lebro: provider entry requires an ID")
	}
	if e.Model == nil || isNilInterface(e.Model) {
		return fmt.Errorf("lebro: provider %q requires a model adapter", e.ID)
	}
	return nil
}

var (
	ErrProviderNotFound      = errors.New("lebro: provider not found")
	ErrProviderAlreadyExists = errors.New("lebro: provider already registered")
)

// ProviderRegistry is a thread-safe collection of provider adapters. It is
// safe for concurrent use after construction.
type ProviderRegistry struct {
	mu        sync.RWMutex
	providers map[ProviderID]ProviderEntry
	order     []ProviderID
}

// NewProviderRegistry creates an empty registry.
func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{
		providers: make(map[ProviderID]ProviderEntry),
	}
}

// Register adds a provider entry to the registry. Returns
// ErrProviderAlreadyExists when a provider with the same ID is already
// registered.
func (r *ProviderRegistry) Register(entry ProviderEntry) error {
	if err := entry.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.providers[entry.ID]; exists {
		return fmt.Errorf("%w: %q", ErrProviderAlreadyExists, entry.ID)
	}
	r.providers[entry.ID] = entry
	r.order = append(r.order, entry.ID)
	return nil
}

// Get returns the provider entry for id. Returns ErrProviderNotFound when no
// such provider exists.
func (r *ProviderRegistry) Get(id ProviderID) (ProviderEntry, error) {
	if r == nil {
		return ProviderEntry{}, fmt.Errorf("%w: %q", ErrProviderNotFound, id)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.providers[id]
	if !ok {
		return ProviderEntry{}, fmt.Errorf("%w: %q", ErrProviderNotFound, id)
	}
	return entry, nil
}

// List returns all registered provider entries in registration order.
func (r *ProviderRegistry) List() []ProviderEntry {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	entries := make([]ProviderEntry, 0, len(r.order))
	for _, id := range r.order {
		entries = append(entries, r.providers[id])
	}
	return entries
}

// IDs returns all registered provider IDs in registration order.
func (r *ProviderRegistry) IDs() []ProviderID {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]ProviderID(nil), r.order...)
}

// Len returns the number of registered providers.
func (r *ProviderRegistry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.providers)
}
