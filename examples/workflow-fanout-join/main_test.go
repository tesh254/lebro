package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunPrintsFanOutJoinResults(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "status: succeeded") {
		t.Fatalf("output = %q", out)
	}
	if !strings.Contains(out, "fan-out joins: 1") {
		t.Fatalf("output = %q", out)
	}
	if !strings.Contains(out, "enrichment") {
		t.Fatalf("output = %q", out)
	}
	if !strings.Contains(out, "risk-check") {
		t.Fatalf("output = %q", out)
	}
}
