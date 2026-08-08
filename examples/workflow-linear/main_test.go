package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunPrintsDoubledAndAddedOutput(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "status: succeeded") {
		t.Fatalf("output = %q", out)
	}
	if !strings.Contains(out, "output: 11") {
		t.Fatalf("output = %q", out)
	}
}
