package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/httpapi"
)

// newTestClient serves the example's API and returns a client for it.
func newTestClient(t *testing.T) *httpapi.Client {
	t.Helper()

	server, err := newServer()
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	client, err := httpapi.NewClient(httpapi.ClientConfig{BaseURL: httpServer.URL})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestExampleClientRunsAgentAndWorkflow(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()

	if err := client.CheckCompatibility(ctx); err != nil {
		t.Fatalf("contract handshake: %v", err)
	}

	result, err := client.Run(ctx, "assistant", httpapi.RunRequest{
		Messages: []httpapi.MessageInput{{Content: "Hi there"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "You said: Hi there" {
		t.Fatalf("agent reply = %q", result.Content)
	}

	workflowResult, err := client.RunWorkflow(ctx, "greet", httpapi.WorkflowRunRequest{
		Input: json.RawMessage(`{"name":"Ada"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		Greeting string `json:"greeting"`
	}
	if err := json.Unmarshal(workflowResult.Output, &output); err != nil {
		t.Fatal(err)
	}
	if output.Greeting != "Hello, Ada!" {
		t.Fatalf("workflow greeting = %q", output.Greeting)
	}
}

func TestExampleClientRoundTripsStructuredOutput(t *testing.T) {
	client := newTestClient(t)

	result, err := client.Run(context.Background(), "forecaster", httpapi.RunRequest{
		Messages: []httpapi.MessageInput{{Content: "Forecast for Nairobi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		City         string  `json:"city"`
		TemperatureC float64 `json:"temperature_c"`
	}
	if err := json.Unmarshal(result.StructuredOutput, &decoded); err != nil {
		t.Fatalf("decode structured output %q: %v", result.StructuredOutput, err)
	}
	if decoded.City != "Nairobi" || decoded.TemperatureC != 24 {
		t.Fatalf("structured output = %+v", decoded)
	}
}

// The example's headline behavior: a streamed run the caller stops part way
// through must report a cancelled run rather than completing.
func TestExampleClientCancelsStreamedRun(t *testing.T) {
	client := newTestClient(t)

	stream, err := client.RunStream(context.Background(), "assistant", httpapi.RunRequest{
		Messages: []httpapi.MessageInput{{Content: "Stream please"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Cancel()

	var seen int
	for range stream.Events {
		seen++
		if seen == 2 {
			stream.Cancel()
			break
		}
	}
	if seen == 0 {
		t.Fatal("no deltas were delivered")
	}

	if _, err := stream.Drain(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Drain error = %v, want it to match context.Canceled", err)
	}
}

func TestExampleClientReportsTypedErrors(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()

	if _, err := client.Run(ctx, "no-such-agent", httpapi.RunRequest{}); !errors.Is(err, lebro.ErrNotFound) {
		t.Fatalf("unknown agent error = %v, want lebro.ErrNotFound", err)
	}

	_, err := client.RunWorkflow(ctx, "greet", httpapi.WorkflowRunRequest{
		Input: json.RawMessage(`{"name":42}`),
	})
	var apiErr *httpapi.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not an *APIError", err)
	}
	if apiErr.Code != httpapi.ErrorCodeInvalidInput {
		t.Fatalf("code = %q, want invalid_input", apiErr.Code)
	}
}
