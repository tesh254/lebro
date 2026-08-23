package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunLoopsUntilConditionHolds(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, `"attempts":2`) {
		t.Fatalf("output = %q, want two attempts", out)
	}
}
