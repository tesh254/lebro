package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunCombinesOrdinaryToolAndAgentSteps(t *testing.T) {
	var output bytes.Buffer
	if err := run(&output); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "status: succeeded") || !strings.Contains(got, "summary: Nairobi is 24.5C.") {
		t.Fatalf("output = %q", got)
	}
}
