package geminiapi

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain registers goleak verification for the whole geminiapi test suite,
// because the streaming adapter spawns a goroutine per stream that must exit
// once the stream is closed.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		goleak.IgnoreTopFunction("go.opencensus.io/stats/view.(*worker).start"),
	)
}
