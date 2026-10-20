package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestDecisionImagesExample(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatalf("run failed: %v", err)
	}

	output := buf.String()

	cases := []string{
		"images on the wire: 1",
		"image media type: image/png",
		"expected level: 1.40",
		"confidence: 0.41",
		"action: escalated to a human reviewer",
		"unsupported image: rejected before the request was sent",
	}
	for _, want := range cases {
		if !strings.Contains(output, want) {
			t.Errorf("expected %q in the output, got:\n%s", want, output)
		}
	}
}
