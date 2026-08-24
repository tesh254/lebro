package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestTenantPolicyExample(t *testing.T) {
	var output bytes.Buffer
	if err := run(&output); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if got := output.String(); !strings.Contains(got, "other tenant: denied") || !strings.Contains(got, "acme tenant: Your ticket is open.") {
		t.Fatalf("output = %q", got)
	}
}
