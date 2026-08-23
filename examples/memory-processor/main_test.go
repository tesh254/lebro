package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunApprovesAndStoresFact(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"approved: true", `"Ada"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want it to contain %q", out, want)
		}
	}
}
