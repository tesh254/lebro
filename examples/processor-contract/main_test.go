package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestProcessorContractExample(t *testing.T) {
	var output bytes.Buffer
	if err := run(&output); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"processor: prefix", "decision: transform", "message: processed: hello"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output missing %q: %s", want, output.String())
		}
	}
}
