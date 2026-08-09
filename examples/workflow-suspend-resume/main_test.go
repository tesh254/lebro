package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunSuspendsAndResumesAcrossReopen(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"suspended status: suspended",
		"suspended at step: 2 (await-approval)",
		"invalid resume rejected:",
		"resumed status: succeeded",
		"persisted final status: succeeded",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want %q", out, want)
		}
	}
}
