package testkit

import (
	"testing"

	"github.com/tesh254/lebro/internal/runtime"
)

// TestRuntimeStoreContractSuiteAgainstMemoryStore runs the capability-based
// suite against the in-memory store so the suite itself stays exercised and
// verified: a regression in the suite fails here instead of surfacing as a
// confusing pass in every adapter.
func TestRuntimeStoreContractSuiteAgainstMemoryStore(t *testing.T) {
	RuntimeStoreContractSuite(t, func(*testing.T) runtime.RuntimeStore {
		return runtime.NewMemoryStore()
	})
}
