package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStudioExampleRunsAndInspectsEvents(t *testing.T) {
	studioServer, err := newStudio()
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(studioServer.Handler())
	defer httpServer.Close()

	t.Run("agent run through the API", func(t *testing.T) {
		body := post(httpServer.URL+"/api/agents/assistant/runs", `{"messages":[{"content":"Hi there"}]}`)
		if !strings.HasPrefix(body, "200 OK") {
			t.Fatalf("agent run = %s", body)
		}
		if !strings.Contains(body, "You said: Hi there") {
			t.Fatalf("agent reply missing from %s", body)
		}
	})

	t.Run("workflow run with custom JSON input", func(t *testing.T) {
		body := post(httpServer.URL+"/api/workflows/greet/runs", `{"input":{"name":"Ada"}}`)
		if !strings.Contains(body, "Hello, Ada!") {
			t.Fatalf("workflow output missing from %s", body)
		}
	})

	t.Run("ordered events are inspectable", func(t *testing.T) {
		list := get(httpServer.URL + "/api/studio/traces")
		traceID := firstTraceID(list)
		if traceID == "" {
			t.Fatalf("no trace recorded: %s", list)
		}
		trace := get(httpServer.URL + "/api/studio/traces/" + traceID)
		// The run root and the model call both appear in the timeline.
		if !strings.Contains(trace, `"kind":"run"`) || !strings.Contains(trace, `"kind":"model"`) {
			t.Fatalf("ordered events missing run or model span: %s", trace)
		}
	})
}
