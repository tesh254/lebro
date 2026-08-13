package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	var output bytes.Buffer
	if err := run(&output); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "output: [2,4,6]") {
		t.Fatalf("output = %q", got)
	}
}
