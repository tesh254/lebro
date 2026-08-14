package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunDelegatesToASelectedSubagent(t *testing.T) {
	var output bytes.Buffer
	if err := run(&output); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{
		"status: succeeded",
		"delegated to: researcher",
		"delegated status: succeeded",
		"delegated output: The Nairobi office opened in 2019.",
		"delegation step: 1 (delegate)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output %q does not contain %q", got, want)
		}
	}
	// The supervisor delegated to exactly one subagent.
	if strings.Contains(got, "delegated to: editor") {
		t.Fatalf("output %q shows an unexpected second delegation", got)
	}
}
