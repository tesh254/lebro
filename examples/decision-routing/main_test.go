package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestDecisionRoutingExample(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatalf("run failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "route: billing") {
		t.Errorf("expected the request to route to billing, got:\n%s", output)
	}
	if !strings.Contains(output, "eligibility: probability=0.930 outcome=select") {
		t.Errorf("expected the eligibility to select, got:\n%s", output)
	}
	if !strings.Contains(output, "action: the refund can be issued automatically") {
		t.Errorf("expected the refund to be issued automatically, got:\n%s", output)
	}
	if !strings.Contains(output, "composite:") {
		t.Errorf("expected a composite score, got:\n%s", output)
	}
	if !strings.Contains(output, "action: escalate to a human, the combined score is high") {
		t.Errorf("expected the composite score to escalate, got:\n%s", output)
	}
}
