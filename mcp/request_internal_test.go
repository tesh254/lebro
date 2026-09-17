package mcp

import (
	"context"
	"encoding/json"
	"errors"
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
	var panicErr *lebro.ToolPanicError
	if panicResult.ToolID != "panic" || !errors.As(panicResult.Err, &panicErr) || panicErr.Value != "boom" {
		t.Fatalf("panic result = %#v", panicResult)
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

func TestExecuteAdapterTool_ValidatesAndNormalizesResults(t *testing.T) {
	validators := &toolValidators{
		input:  mustCompileMCPInputSchema(json.RawMessage(`{"type":"object","required":["value"],"properties":{"value":{"type":"string"}}}`)),
		output: mustCompileMCPInputSchema(json.RawMessage(`{"type":"object","required":["value"],"properties":{"value":{"type":"string"}}}`)),
	}

	nilContext := executeAdapterTool(testNilContext(), "nil-context", json.RawMessage(`{}`), func(context.Context, lebro.ToolExecutionRequest) lebro.ToolExecutionResult {
		return lebro.ToolExecutionResult{State: lebro.ToolExecutionSucceeded}
	}, validators)
	if nilContext.State != lebro.ToolExecutionHandlerError || nilContext.ToolID != "nil-context" {
		t.Fatalf("nil context = %#v", nilContext)
	}

	called := false
	invalidInput := executeAdapterTool(context.Background(), "input", json.RawMessage(`{}`), func(context.Context, lebro.ToolExecutionRequest) lebro.ToolExecutionResult {
		called = true
		return lebro.ToolExecutionResult{State: lebro.ToolExecutionSucceeded}
	}, validators)
	if invalidInput.State != lebro.ToolExecutionInvalidInput || invalidInput.ToolID != "input" || called {
		t.Fatalf("invalid input = %#v, called = %v", invalidInput, called)
	}

	invalidOutput := executeAdapterTool(context.Background(), "output", json.RawMessage(`{"value":"ok"}`), func(context.Context, lebro.ToolExecutionRequest) lebro.ToolExecutionResult {
		return lebro.ToolExecutionResult{State: lebro.ToolExecutionSucceeded, Output: json.RawMessage(`{"wrong":true}`)}
	}, validators)
	if invalidOutput.State != lebro.ToolExecutionInvalidOutput || invalidOutput.ToolID != "output" {
		t.Fatalf("invalid output = %#v", invalidOutput)
	}

	success := executeAdapterTool(context.Background(), "success", json.RawMessage(`{"value":"ok"}`), func(context.Context, lebro.ToolExecutionRequest) lebro.ToolExecutionResult {
		return lebro.ToolExecutionResult{State: lebro.ToolExecutionSucceeded, Output: json.RawMessage(`{"value":"ok"}`)}
	}, validators)
	if success.State != lebro.ToolExecutionSucceeded || success.ToolID != "success" {
		t.Fatalf("success = %#v", success)
	}
}

func testNilContext() context.Context { return nil }

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
