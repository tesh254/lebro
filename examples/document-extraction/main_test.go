package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunExtractionSucceedsAndFailsLoudly(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`200 OK`,
		`"status":"succeeded"`,
		`"invoice_id":"INV-2043"`,
		`"total_cents":4198000`,
		`502 Bad Gateway`,
		`"code":"invalid_output"`,
		"/agents/{id}/runs",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want it to contain %q", out, want)
		}
	}
}
