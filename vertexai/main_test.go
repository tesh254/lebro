package vertexai

import (
	"go.uber.org/goleak"
	"testing"
)

// TestMain registers goleak verification for the whole vertexai test suite,
// because streaming adapters spawn goroutines that must exit once a stream is
// closed.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		// The genai SDK's dependency graph starts an opencensus view worker
		// once per process at init; it is not a leak from this package.
		goleak.IgnoreTopFunction("go.opencensus.io/stats/view.(*worker).start"),
	)
}
