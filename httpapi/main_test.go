package httpapi_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain registers goleak verification for the whole httpapi test suite,
// matching internal/runtime and openai. The streaming route hands a run to a
// goroutine that outlives the handler unless Cancel and Wait are both reached,
// and the disconnect tests deliberately abandon a stream mid-flight — exactly
// the shape that parks a goroutine forever. VerifyTestMain turns that into a
// test failure rather than a silent leak.
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
