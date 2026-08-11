package mcp_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tesh254/lebro"
)

type scalarOutputTool struct{}

func (scalarOutputTool) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{
		ID:           "scalar-output",
		Description:  "Returns a JSON string",
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		OutputSchema: json.RawMessage(`{"type":"string"}`),
	}
}

func (scalarOutputTool) Execute(context.Context, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`"ok"`), nil
}

func TestExposeTool_ScalarOutputSchema(t *testing.T) {
	srv := newTestServer(t)
	registry := mustRegistry(t)
	must(t, registry.Register(scalarOutputTool{}))
	tool, ok := registry.Resolve("scalar-output")
	if !ok {
		t.Fatal("registered scalar-output tool not found")
	}
	must(t, srv.ExposeTool(tool))

	session, cleanup := connectServer(t, srv)
	defer cleanup()
	tools, err := session.ListTools(context.Background(), nil)
	must(t, err)
	if len(tools.Tools) != 1 {
		t.Fatalf("tools/list returned %d tools, want 1", len(tools.Tools))
	}
	schema, err := json.Marshal(tools.Tools[0].OutputSchema)
	must(t, err)
	var outputSchema struct {
		Type string `json:"type"`
	}
	must(t, json.Unmarshal(schema, &outputSchema))
	if outputSchema.Type != "string" {
		t.Fatalf("output schema type = %q, want string", outputSchema.Type)
	}
}
