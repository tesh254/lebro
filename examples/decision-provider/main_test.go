package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestDecisionProviderExample(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatalf("run failed: %v", err)
	}

	output := buf.String()

	cases := []string{
		"provider: systemone",
		"refund: probability=0.93 confidence=0.86",
		"department: billing",
		"urgency: expected=1.40 selected=1",
	}
	for _, want := range cases {
		if !strings.Contains(output, want) {
			t.Errorf("expected %q in the output, got:\n%s", want, output)
		}
	}
}
