package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tesh254/lebro"
)

func TestRunProducesExpectedOutput(t *testing.T) {
	output := &bytes.Buffer{}
	if err := run(output); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{
		"assistant: hello",
		"usage: in=1 out=1 total=2",
		"finish: stop",
		"failure: kind=unavailable status=503",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q\n got: %s", want, got)
		}
	}
}

func TestMustPanic(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("must did not panic")
		}
	}()
	must(errors.New("boom"))
}

func TestExtensionSerialization(t *testing.T) {
	extension := json.RawMessage(`{"temperature":0.2,"max_tokens":16}`)
	if !json.Valid(extension) {
		t.Fatal("extension must be valid JSON")
	}
	request := lebro.ModelRequest{Extension: extension}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
}
