package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRecallsRelevantHistory(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "Deploy to Nairobi tomorrow") {
		t.Fatalf("output = %q, want the recalled message", out)
	}
}
