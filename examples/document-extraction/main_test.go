package main

import (
	"bytes"
	"errors"
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

func TestPostReturnsReadError(t *testing.T) {
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: errReadCloser{}}, nil
	})}
	_, err := post(client, "https://example.test", "{}")
	if !errors.Is(err, errRead) {
		t.Fatalf("post() error = %v, want %v", err, errRead)
	}
}

func TestRunReturnsWriterError(t *testing.T) {
	if err := run(failingWriter{}); !errors.Is(err, errWrite) {
		t.Fatalf("run() error = %v, want %v", err, errWrite)
	}
}

var (
	errRead  = errors.New("read failed")
	errWrite = errors.New("write failed")
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type errReadCloser struct{}

func (errReadCloser) Read([]byte) (int, error) { return 0, errRead }
func (errReadCloser) Close() error             { return nil }

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errWrite }
