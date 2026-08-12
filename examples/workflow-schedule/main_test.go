package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunFiresScheduleAfterRestart(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"fired: 1 skipped: 0 missed: 1",
		"next fire after tick: 2026-08-12T11:00:00Z",
		"history entries: 2",
		"missed scheduled_for=2026-08-12T09:00:00Z",
		"succeeded scheduled_for=2026-08-12T10:00:00Z",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want %q", out, want)
		}
	}
}
