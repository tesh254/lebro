package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestRunEnforcesTenantBoundaryAtFourPoints(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"other tenant run: denied before any model call (fixtures left: 2)",
		`kim without tools:call: tool call denied (tool.call on "weather.lookup")`,
		"ava with tools:call: Nairobi is 24.5C.",
		"cross-tenant thread read: denied before reaching the store",
		"streamed tool call weather.lookup: arguments visible=false",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want it to contain %q", out, want)
		}
	}
}

func TestWritefReturnsWriterError(t *testing.T) {
	if err := writef(failingWriter{}, "nope"); !errors.Is(err, errWrite) {
		t.Fatalf("writef() error = %v, want %v", err, errWrite)
	}
}

var errWrite = errors.New("write failed")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errWrite }
