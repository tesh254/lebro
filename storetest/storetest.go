// Package storetest exposes Lebro's adapter conformance suites to applications
// that implement RuntimeStore against their own persistence layer.
package storetest

import (
	"testing"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
)

// RuntimeStoreFactory returns a fresh adapter ready for each subtest. The
// factory should run any application-owned migrations or fixture setup it
// needs; Lebro does not run migrations for RuntimeStore adapters.
type RuntimeStoreFactory func(*testing.T) lebro.RuntimeStore

// RuntimeStoreContractSuite verifies advertised capabilities, record
// round-trips, pagination, cancellation, scope isolation, optimistic conflict
// behavior, transactional commit/rollback, and idempotent observability
// writes. Partial adapters are supported: tests for unadvertised capabilities
// are skipped.
func RuntimeStoreContractSuite(t *testing.T, factory RuntimeStoreFactory) {
	t.Helper()
	internal := func(t *testing.T) lebro.RuntimeStore { return factory(t) }
	testkit.RuntimeStoreContractSuite(t, internal)
}
