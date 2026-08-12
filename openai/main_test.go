package openai

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain registers goleak verification for the whole openai test suite,
// matching internal/runtime. The adapters here spawn goroutines indirectly
// through net/http, and the tests that hold a connection open to reproduce a
// mid-read cancellation are easy to write in a way that parks a server
// goroutine forever. VerifyTestMain turns that into a test failure rather than
// a silent leak.
//
// It runs once after every test completes, so it does not conflict with
// t.Parallel the way per-test verification would.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// net/http keeps idle connections and their reader/writer goroutines
		// alive after a response is handled; they are pooled, not leaked.
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
	)
}
