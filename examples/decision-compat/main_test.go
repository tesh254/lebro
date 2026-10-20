package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestDecisionCompatExample(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatalf("run failed: %v", err)
	}

	output := buf.String()

	cases := []string{
		"assistant: Your refund is on the way.",
		"finish: stop",
		"satisfied: 0.88",
		"compatibility: the generative client needed no change",
	}
	for _, want := range cases {
		if !strings.Contains(output, want) {
			t.Errorf("expected %q in the output, got:\n%s", want, output)
		}
	}
}
