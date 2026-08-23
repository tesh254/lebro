package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunFiresOverdueDigestAfterRestart(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"schedule persisted at 2026-08-12T07:00:00Z; process exits",
		"tick fired 1 schedule(s)",
		"digest run agent-run-0001: succeeded",
		"MORNING COMPETITOR BRIEF",
		"news (industry news)",
		"Rival added SSO on mid-tier plans",
		"$59 per seat",
		"next fire: 2026-08-13T06:00:00Z",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want it to contain %q", out, want)
		}
	}
}
