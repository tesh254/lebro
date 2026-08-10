package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunPrintsSelectedBranchOutput(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "status: succeeded") {
		t.Fatalf("output = %q", out)
	}
	if !strings.Contains(out, "plan: enterprise") {
		t.Fatalf("output = %q", out)
	}
	if !strings.Contains(out, "path: [premium-handler]") {
		t.Fatalf("output = %q", out)
	}
}
