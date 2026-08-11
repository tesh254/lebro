package mcp

import (
	"encoding/json"
	"testing"

	"github.com/tesh254/lebro"
)

func TestWorkflowResultToMCP_SuspendedMarksResumeUnavailable(t *testing.T) {
	result := workflowResultToMCP(lebro.WorkflowRunResult{
		ID:     "run-1",
		Status: lebro.RunStatusSuspended,
		Suspend: &lebro.SuspendResult{
			Step:     1,
			StepID:   "approval",
			Contract: json.RawMessage(`{"type":"object"}`),
		},
	})

	content, ok := result.StructuredContent.(json.RawMessage)
	if !ok {
		t.Fatalf("StructuredContent type = %T, want json.RawMessage", result.StructuredContent)
	}
	var output map[string]any
	if err := json.Unmarshal(content, &output); err != nil {
		t.Fatalf("decode structured content: %v", err)
	}
	if available, ok := output["resume_available"].(bool); !ok || available {
		t.Fatalf("resume_available = %v, want false", output["resume_available"])
	}
}

func TestWorkflowToolInputSchema_PreservesLocalReferences(t *testing.T) {
	schema := workflowToolInputSchema("workflow.refs", json.RawMessage(`{
		"$defs": {
			"request": {
				"type": "object",
				"required": ["value"],
				"properties": {"value": {"type": "string"}}
			}
		},
		"$ref": "#/$defs/request"
	}`))
	compiled := mustCompileMCPInputSchema(schema)
	if err := compiled.Validate(json.RawMessage(`{"input":{"value":"ok"}}`)); err != nil {
		t.Fatalf("validate schema with local reference: %v", err)
	}
}

func TestWorkflowToolInputSchema_UsesUniqueResourceID(t *testing.T) {
	inputSchema := json.RawMessage(`{"type":"object"}`)
	first := workflowToolInputSchema("workflow.first", inputSchema)
	second := workflowToolInputSchema("workflow.second", inputSchema)

	resourceID := func(schema json.RawMessage) string {
		t.Helper()
		var wrapper struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(schema, &wrapper); err != nil {
			t.Fatalf("decode wrapper: %v", err)
		}
		var input struct {
			ID string `json:"$id"`
		}
		if err := json.Unmarshal(wrapper.Properties["input"], &input); err != nil {
			t.Fatalf("decode input schema: %v", err)
		}
		return input.ID
	}
	if firstID, secondID := resourceID(first), resourceID(second); firstID == secondID {
		t.Fatalf("resource IDs match: %q", firstID)
	}
}
