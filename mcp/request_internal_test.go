package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tesh254/lebro"
)

func TestExecuteAdapterTool_RecoversPanicAndHonorsCancellation(t *testing.T) {
	panicResult := executeAdapterTool(context.Background(), "panic", json.RawMessage(`{}`), func(context.Context, lebro.ToolExecutionRequest) lebro.ToolExecutionResult {
		panic("boom")
	}, nil)
	if panicResult.State != lebro.ToolExecutionPanicked {
		t.Fatalf("panic state = %q, want %q", panicResult.State, lebro.ToolExecutionPanicked)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := executeAdapterTool(ctx, "cancelled", json.RawMessage(`{}`), func(context.Context, lebro.ToolExecutionRequest) lebro.ToolExecutionResult {
		return lebro.ToolExecutionResult{State: lebro.ToolExecutionSucceeded}
	}, nil)
	if cancelled.State != lebro.ToolExecutionCancelled || cancelled.Err != context.Canceled {
		t.Fatalf("cancelled = %#v", cancelled)
	}

	ctx, cancel = context.WithCancel(context.Background())
	cancelled = executeAdapterTool(ctx, "post-cancelled", json.RawMessage(`{}`), func(context.Context, lebro.ToolExecutionRequest) lebro.ToolExecutionResult {
		cancel()
		return lebro.ToolExecutionResult{State: lebro.ToolExecutionSucceeded}
	}, nil)
	if cancelled.State != lebro.ToolExecutionCancelled || cancelled.Err != context.Canceled {
		t.Fatalf("post-cancelled = %#v", cancelled)
	}
}

func TestValidateWorkflowAdapterSchema(t *testing.T) {
	for _, schema := range []json.RawMessage{json.RawMessage(`null`), json.RawMessage(`{`)} {
		if err := validateWorkflowAdapterSchema(schema); err == nil {
			t.Fatalf("schema %q accepted", schema)
		}
	}
}

func TestExposeWorkflowAdapter_RejectsMalformedSchema(t *testing.T) {
	server := NewServer(ServerConfig{Implementation: &mcpsdk.Implementation{Name: "test", Version: "test"}})
	err := server.ExposeWorkflowAdapter(WorkflowAdapter{
		Definition:    lebro.WorkflowDefinition{ID: "invalid"},
		InputSchema:   json.RawMessage(`null`),
		ValidateInput: func(json.RawMessage) error { return nil },
		Run: func(context.Context, lebro.WorkflowRunInput) (lebro.WorkflowRunResult, error) {
			return lebro.WorkflowRunResult{}, nil
		},
	})
	if err == nil {
		t.Fatal("malformed workflow schema accepted")
	}
}
