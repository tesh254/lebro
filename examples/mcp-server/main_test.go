package main

import (
	"testing"
)

func TestExample(t *testing.T) {
	// The example runs a blocking stdio MCP server. We only verify the
	// example compiles and the helper functions work.
	if got := mustValue(42, nil); got != 42 {
		t.Fatalf("mustValue() = %d, want 42", got)
	}
}
