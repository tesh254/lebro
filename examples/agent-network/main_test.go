package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestAgentNetworkRoutesAndPersistsRecord(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"status: succeeded", "routes: 1", "answer: Lebro is written in Go."} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want it to contain %q", out, want)
		}
	}
}
