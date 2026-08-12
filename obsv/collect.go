package obsv

import "sync"

// guarded is a mutex-protected append-only slice shared by the in-memory
// exporters and repository. It exists so each collector does not restate the
// same locking, and it deliberately exposes no way to read the slice without
// copying.
type guarded[T any] struct {
	mu      sync.Mutex
	records []T
}

func (g *guarded[T]) append(records ...T) {
	if len(records) == 0 {
		return
	}
	g.mu.Lock()
	g.records = append(g.records, records...)
	g.mu.Unlock()
}

// snapshot returns a shallow copy of the collected records. Callers holding
// records with reference fields must deep-copy the result before handing it out.
func (g *guarded[T]) snapshot() []T {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.records) == 0 {
		return nil
	}
	return append([]T(nil), g.records...)
}
