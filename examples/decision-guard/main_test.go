package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestDecisionGuardExample(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatalf("run failed: %v", err)
	}

	output := buf.String()

	cases := []string{
		"allowed                    status=approved     handler_calls=1",
		"uncertain                  status=needs-review handler_calls=0",
		"denied                     status=denied       handler_calls=0",
		"provider failure           status=denied       handler_calls=0",
		"tool outside the allowlist status=denied       handler_calls=0",
		"compaction trigger         compact=true",
	}
	for _, want := range cases {
		if !strings.Contains(output, want) {
			t.Errorf("expected %q in the output, got:\n%s", want, output)
		}
	}
}
