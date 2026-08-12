package evals_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain registers goleak verification for the whole evals test suite. An
// Experiment dispatches cases across a worker pool, so a worker that outlives
// Run — or a dispatch loop that abandons workers on cancellation — leaks, which
// is exactly the defect bounded concurrency must not introduce.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
