package main

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRunBooksAndRemembersCallerAcrossCalls(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`heard: "Book a table for two on Friday at seven"`,
		"confirmed.",
		`heard: "Add a high chair to that booking"`,
		"a high chair is added to your Friday table for two",
		`heard: "Do you have outdoor seating"`,
		"outdoor seating is first come",
		"caller-555 persisted turns: 4",
		"caller-777 persisted turns: 2",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want it to contain %q", out, want)
		}
	}
}

func TestFirstHalfRunesPreservesUTF8(t *testing.T) {
	got := firstHalfRunes("naïrobi")
	if got != "naï" || !utf8.ValidString(got) {
		t.Fatalf("firstHalfRunes() = %q, valid=%t", got, utf8.ValidString(got))
	}
}
