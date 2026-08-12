package studio_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain registers goleak verification for the studio test suite. Start owns
// an http.Server and a serve goroutine; a test that started one without
// shutting it down would park that goroutine forever, and VerifyTestMain turns
// that into a failure rather than a silent leak. The net/http pool goroutines
// are ignored for the same reason httpapi ignores them: they are pooled, not
// leaked.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
	)
}
