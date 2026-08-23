package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunChunksWithBothStrategies(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"recursive (paragraph boundaries):",
		"sliding window (fixed rune boundaries):",
		`guide#0 "Alpha paragraph.`,
		`Gamma paragraph." (docs/guide.md)`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want it to contain %q", out, want)
		}
	}
}
