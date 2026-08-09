package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunPersistsAcrossReopen(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"live status: succeeded",
		"persisted status: succeeded",
		"persisted version: v1",
		"persisted current step: 2 (add-one)",
		"persisted snapshots: 2",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want %q", out, want)
		}
	}
}
