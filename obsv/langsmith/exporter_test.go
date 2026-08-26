package langsmith

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tesh254/lebro/obsv"
)

func TestExporterUsesLangSmithHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "key" {
			t.Errorf("x-api-key = %q", got)
		}
		if got := r.Header.Get("Langsmith-Project"); got != "support" {
			t.Errorf("project = %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	exporter, err := New(Config{Endpoint: server.URL, APIKey: "key", Project: "support"})
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.ExportSpans(context.Background(), []obsv.Span{{TraceID: "trace", SpanID: "span", Name: "run"}}); err != nil {
		t.Fatalf("ExportSpans() = %v", err)
	}
}

func TestNewRequiresAPIKey(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New() error = nil")
	}
}
