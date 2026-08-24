package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunBlocksDeployOnRegressedCase(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"dataset support-bot-regression version ",
		"baseline (support-bot-regression-",
		"): 3/3 passed",
		"): 2/3 passed",
		"REGRESSED password-reset/exact_match:",
		"DEPLOY BLOCKED: regressions against dataset support-bot-regression",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want it to contain %q", out, want)
		}
	}
	if strings.Contains(out, "deploy approved") {
		t.Fatalf("gate approved a regressed candidate: %q", out)
	}
}
