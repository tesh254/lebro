package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
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

func TestPostReturnsRequestError(t *testing.T) {
	_, err := post(&http.Client{}, "http://127.0.0.1:1", "{}")
	if err == nil {
		t.Fatal("post() error = nil, want request error")
	}
}

func TestPostRendersResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	got, err := post(server.Client(), server.URL, "{}")
	if err != nil || got != "201 Created\n{\"ok\":true}" {
		t.Fatalf("post() = %q, %v", got, err)
	}
}
