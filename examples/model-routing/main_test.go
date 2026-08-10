package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestModelRoutingExample(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatalf("run failed: %v", err)
	}

	output := buf.String()

	// Verify primary provider failed and fallback succeeded.
	if !strings.Contains(output, "status: succeeded") {
		t.Errorf("expected status: succeeded, got:\n%s", output)
	}
	if !strings.Contains(output, "model_attempts: 2") {
		t.Errorf("expected 2 model attempts, got:\n%s", output)
	}
	if !strings.Contains(output, "provider=primary status=fallback") {
		t.Errorf("expected primary provider fallback status, got:\n%s", output)
	}
	if !strings.Contains(output, "provider=fallback status=success") {
		t.Errorf("expected fallback provider success status, got:\n%s", output)
	}
	if !strings.Contains(output, "Hello from fallback provider!") {
		t.Errorf("expected fallback provider response, got:\n%s", output)
	}

	// Verify routing events were emitted.
	if !strings.Contains(output, "model_attempt_started provider=primary") {
		t.Errorf("expected model_attempt_started for primary, got:\n%s", output)
	}
	if !strings.Contains(output, "model_attempt_finished provider=primary") {
		t.Errorf("expected model_attempt_finished for primary, got:\n%s", output)
	}
	if !strings.Contains(output, "model_attempt_started provider=fallback") {
		t.Errorf("expected model_attempt_started for fallback, got:\n%s", output)
	}
	if !strings.Contains(output, "model_attempt_finished provider=fallback") {
		t.Errorf("expected model_attempt_finished for fallback, got:\n%s", output)
	}
}
