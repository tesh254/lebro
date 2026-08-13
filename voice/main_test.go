package voice_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain verifies no goroutine leaks across the suite. Recognition and
// synthesis run on their own goroutines, so a leaked provider goroutine — one
// that keeps writing to a channel after its stream is cancelled, say — fails the
// suite here rather than passing silently.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
