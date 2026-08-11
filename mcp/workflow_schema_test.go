package mcp_test

import (
	"context"
	"encoding/json"
	"testing"
)

func TestExposeWorkflow_AdvertisesEntryInputSchema(t *testing.T) {
	srv := newTestServer(t)
	must(t, srv.ExposeWorkflow(mustWorkflow(t, "schema")))

	session, cleanup := connectServer(t, srv)
	defer cleanup()
	tools, err := session.ListTools(context.Background(), nil)
	must(t, err)
	if len(tools.Tools) != 1 {
		t.Fatalf("tools/list returned %d tools, want 1", len(tools.Tools))
	}

	schema, err := json.Marshal(tools.Tools[0].InputSchema)
	must(t, err)
	var advertised struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	must(t, json.Unmarshal(schema, &advertised))
	if len(advertised.Required) != 1 || advertised.Required[0] != "input" {
		t.Fatalf("required = %v, want [input]", advertised.Required)
	}
	var entryInput struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	must(t, json.Unmarshal(advertised.Properties["input"], &entryInput))
	if len(entryInput.Required) != 2 || entryInput.Required[0] != "a" || entryInput.Required[1] != "b" {
		t.Fatalf("entry required = %v, want [a b]", entryInput.Required)
	}
	if len(entryInput.Properties) != 2 {
		t.Fatalf("entry properties = %v, want a and b", entryInput.Properties)
	}
}
