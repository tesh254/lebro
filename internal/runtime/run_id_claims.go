package runtime

import "sync"

// runIDClaims serializes caller-supplied run identities within one executor
// instance. Durable adapters still enforce cross-process uniqueness.
type runIDClaims struct {
	mu     sync.Mutex
	active map[RunID]struct{}
}

func (c *runIDClaims) Claim(id RunID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil {
		c.active = make(map[RunID]struct{})
	}
	if _, exists := c.active[id]; exists {
		return false
	}
	c.active[id] = struct{}{}
	return true
}

func (c *runIDClaims) Release(id RunID) {
	c.mu.Lock()
	delete(c.active, id)
	c.mu.Unlock()
}
