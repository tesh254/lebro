package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExampleServesAgentWorkflowAndContract(t *testing.T) {
	server, err := newServer()
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	t.Run("agent run", func(t *testing.T) {
		body := post(httpServer.URL+"/agents/assistant/runs", `{"messages":[{"content":"Hi there"}]}`)
		if !strings.HasPrefix(body, "200 OK") {
			t.Fatalf("agent run = %s", body)
		}
		if !strings.Contains(body, "You said: Hi there") {
			t.Fatalf("agent reply missing from %s", body)
		}
	})

	t.Run("workflow run", func(t *testing.T) {
		body := post(httpServer.URL+"/workflows/greet/runs", `{"input":{"name":"Ada"}}`)
		if !strings.Contains(body, "Hello, Ada!") {
			t.Fatalf("workflow output missing from %s", body)
		}
	})

	t.Run("invalid workflow input is rejected", func(t *testing.T) {
		body := post(httpServer.URL+"/workflows/greet/runs", `{"input":{"name":42}}`)
		if !strings.HasPrefix(body, "400 Bad Request") {
			t.Fatalf("schema violation = %s, want 400", body)
		}
		if !strings.Contains(body, "invalid_input") {
			t.Fatalf("error code missing from %s", body)
		}
	})

	t.Run("stream", func(t *testing.T) {
		body := post(httpServer.URL+"/agents/assistant/runs/stream", `{"messages":[{"content":"Stream please"}]}`)
		if !strings.Contains(body, "event: model_delta") {
			t.Fatalf("no deltas in %s", body)
		}
		if !strings.Contains(body, "event: run_succeeded") {
			t.Fatalf("no terminal event in %s", body)
		}
	})

	t.Run("openapi", func(t *testing.T) {
		response, err := http.Get(httpServer.URL + "/openapi.json")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		var document struct {
			OpenAPI string                    `json:"openapi"`
			Paths   map[string]map[string]any `json:"paths"`
		}
		if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
			t.Fatal(err)
		}
		if document.OpenAPI != "3.1.0" {
			t.Fatalf("openapi = %q, want 3.1.0", document.OpenAPI)
		}
		if _, ok := document.Paths["/agents/{id}/runs"]; !ok {
			t.Fatalf("agent run path missing from the contract: %v", document.Paths)
		}
	})
}

// The thread route must show what the agent persisted, which is what makes
// thread_id worth supporting at all.
func TestExamplePersistsThreadTranscript(t *testing.T) {
	server, err := newServer()
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	post(httpServer.URL+"/agents/assistant/runs?thread_id=demo", `{"messages":[{"content":"remember this"}]}`)

	body := get(httpServer.URL + "/threads/demo/messages")
	if !strings.Contains(body, "remember this") {
		t.Fatalf("thread does not carry the user turn: %s", body)
	}
	if !strings.Contains(body, "You said: remember this") {
		t.Fatalf("thread does not carry the assistant turn: %s", body)
	}
}
