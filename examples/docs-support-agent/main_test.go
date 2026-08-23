package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunAnswersFromPublicHandbookOnly(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"indexed refunds",
		"indexed shipping",
		"indexed margins",
		"According to policies/refunds.md",
		"According to policies/shipping.md",
		"The public handbook says nothing about that",
		"customer-acme-1 persisted messages: 8",
		"customer-globex-9 persisted messages: 4",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want it to contain %q", out, want)
		}
	}
	// The internal document must never reach a customer-facing answer.
	if strings.Contains(strings.ToLower(out), "margin targets are confidential") {
		t.Fatalf("internal document leaked into an answer: %q", out)
	}
}
