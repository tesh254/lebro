package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRoutesTicketsToSpecialists(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"TCK-1 routed to it (1 hop(s))",
		"TCK-2 routed to hr (1 hop(s))",
		"TCK-3 routed to facilities (1 hop(s))",
		"Reset the credentials and confirm.",
		"Unused leave carries over automatically",
		"A replacement chair is scheduled for tomorrow.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want it to contain %q", out, want)
		}
	}
}
