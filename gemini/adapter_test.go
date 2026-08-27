package gemini

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tesh254/lebro"
)

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New() error = nil, want API key error")
	}
}

func TestGenerateCallsDeveloperAPIEndpoint(t *testing.T) {
	var gotPath, gotKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-goog-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":"hello"}]},"contentRating":{}}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}`))
	}))
	defer server.Close()
	model, err := New(Config{APIKey: "key", Model: "gemini-2.5-flash", BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	response, err := model.Generate(t.Context(), lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Content != "hello" {
		t.Fatalf("content = %q", response.Message.Content)
	}
	if gotPath != "/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotKey != "key" {
		t.Fatalf("api key header = %q", gotKey)
	}
}

func TestGenerateMapsAPIErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"slow down"}}`))
	}))
	defer server.Close()
	model, err := New(Config{APIKey: "key", Model: "gemini-2.5-flash", BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = model.Generate(t.Context(), lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	var modelErr *lebro.ModelError
	if !errors.As(err, &modelErr) || modelErr.Kind != lebro.ModelErrorRateLimited || modelErr.Provider != "gemini" {
		t.Fatalf("error = %#v", err)
	}
}
