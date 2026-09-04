package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func TestCustomStorePersistsAndReloadsThread(t *testing.T) {
	var output bytes.Buffer
	if err := run(&output); err != nil {
		t.Fatal(err)
	}
	content := output.String()
	if !strings.Contains(content, "status: succeeded") {
		t.Fatalf("output missing succeeded status: %q", content)
	}
	if !strings.Contains(content, "thread support-42 holds 4 persisted messages") {
		t.Fatalf("output missing persisted thread: %q", content)
	}
	if !strings.Contains(content, "(saw 2 prior messages)") || !strings.Contains(content, "(saw 4 prior messages)") {
		t.Fatalf("output missing reloaded history counts: %q", content)
	}
	if !strings.Contains(content, "adapter-owned keys:") || !strings.Contains(content, "thread:support-42") {
		t.Fatalf("output missing adapter-owned layout: %q", content)
	}
	if !strings.Contains(content, `capability check: lebro: storage adapter does not support capability "transcript"`) {
		t.Fatalf("output missing typed capability error: %q", content)
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