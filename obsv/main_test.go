package obsv_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain registers goleak verification for the whole obsv test suite. An
// Observer owns an export goroutine, so a test that forgets to Close it, or a
// Close that fails to join the drain loop, leaks — which is exactly the defect
// the isolation guarantee must not introduce.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
