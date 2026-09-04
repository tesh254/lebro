package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestRunProducesSuccessfulTranscript(t *testing.T) {
	var output bytes.Buffer
	if err := run(&output); err != nil {
		t.Fatal(err)
	}
	content := output.String()
	if !strings.Contains(content, "status: succeeded") {
		t.Fatalf("output missing succeeded status: %q", content)
	}
	if !strings.Contains(content, "run_id: ") {
		t.Fatalf("output missing run id: %q", content)
	}
	if !strings.Contains(content, "assistant: The temperature in Nairobi is 24.5C.") {
		t.Fatalf("output missing final assistant text: %q", content)
	}
	if !strings.Contains(content, "tool[") {
		t.Fatalf("output missing tool result: %q", content)
	}
	if !strings.Contains(content, "exhausted: failed") {
		t.Fatalf("output missing step-limit failure: %q", content)
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
