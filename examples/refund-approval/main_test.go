package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tesh254/lebro"
)

func TestRunSuspendsGatesAndCommitsRefund(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"status: suspended",
		`suspended at "await-approval"`,
		`support-agent refused`,
		"invalid consent rejected:",
		"resumed by dana; status: succeeded",
		`{"amount_cents":8400,"approver":"dana","refunded":true}`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want it to contain %q", out, want)
		}
	}
}

func TestApproverGateRefusesMissingCapability(t *testing.T) {
	if err := authorizeApprover(lebro.Identity{Subject: "sam"}); err == nil {
		t.Fatal("expected refusal without refunds:approve")
	}
	if err := authorizeApprover(lebro.Identity{Subject: "dana", Capabilities: []lebro.Capability{approveRefunds}}); err != nil {
		t.Fatalf("expected approval for capable identity: %v", err)
	}
}
