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
	if !strings.Contains(output, "recommendation: refund eligible, route to the refund approval policy") {
		t.Errorf("expected a recommendation to the refund approval policy, got:\n%s", output)
	}
	if !strings.Contains(output, "composite: urgency=1.70 impact=1.70 combined=1.70") {
		t.Errorf("expected the pinned composite score, got:\n%s", output)
	}
	if !strings.Contains(output, "action: escalate to a human, the combined score is high") {
		t.Errorf("expected the composite score to escalate, got:\n%s", output)
	}
	if !strings.Contains(output, "handler: specialist agent") {
		t.Errorf("expected the request to route to a specialist agent, got:\n%s", output)
	}
}
