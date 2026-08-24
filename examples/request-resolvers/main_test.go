package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunResolvesPremiumModelAndInstructions(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "premium tenant response") {
		t.Fatalf("output = %q, want premium response", out)
	}
}
