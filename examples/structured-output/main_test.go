package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestRunProducesStructuredResult(t *testing.T) {
	var output bytes.Buffer
	if err := run(&output); err != nil {
		t.Fatal(err)
	}
	content := output.String()
	if !strings.Contains(content, "status: succeeded") {
		t.Fatalf("output missing succeeded status: %q", content)
	}
	if !strings.Contains(content, "structured: {") {
		t.Fatalf("output missing structured payload: %q", content)
	}
	if !strings.Contains(content, "decoded: 24.5C in Nairobi") {
		t.Fatalf("output missing decoded value: %q", content)
	}
}

func TestExampleMain(t *testing.T) {
	var output bytes.Buffer
	original := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = original })

	done := make(chan struct{})
	go func() {
		main()
		_ = w.Close()
		close(done)
	}()
	if _, err := io.Copy(&output, r); err != nil {
		t.Fatal(err)
	}
	<-done
	if !strings.Contains(output.String(), "status: succeeded") {
		t.Fatalf("main output = %q", output.String())
	}
}

func TestMustPanicsOnError(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("must did not panic")
		}
	}()
	must(errors.New("boom"))
}
