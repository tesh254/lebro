package lebro

import (
	"context"
	"encoding/json"
	"testing"
)

var (
	_ Model    = contractModel{}
	_ Tool     = contractTool{}
	_ Workflow = contractWorkflow{}
)

type contractModel struct{}

func (contractModel) Generate(_ context.Context, request ModelRequest) (ModelResponse, error) {
	return ModelResponse{Message: request.Messages[0], FinishReason: FinishReasonStop}, nil
}

type contractTool struct{}

func (contractTool) Definition() ToolDefinition {
	return ToolDefinition{ID: "echo", InputSchema: json.RawMessage(`{"type":"string"}`)}
}

func (contractTool) Execute(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
	return append(json.RawMessage(nil), input...), nil
}

type contractWorkflow struct{}

func (contractWorkflow) Definition() WorkflowDefinition {
	return WorkflowDefinition{ID: "echo", Name: "Echo"}
}

func (contractWorkflow) Run(_ context.Context, input RunInput) (RunResult, error) {
	return RunResult{ID: "run-1", Status: RunStatusSucceeded, Messages: input.Messages, Metadata: input.Metadata}, nil
}

func TestMAD10PublicContracts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	message := Message{Role: RoleUser, Content: "hello"}

	response, err := (contractModel{}).Generate(ctx, ModelRequest{
		Model:    "example/model",
		Messages: []Message{message},
		Tools:    []ToolDefinition{{ID: "echo"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message != message || response.FinishReason != FinishReasonStop {
		t.Fatalf("model response = %#v", response)
	}

	tool := contractTool{}
	definition := tool.Definition()
	if definition.ID != "echo" || !json.Valid(definition.InputSchema) {
		t.Fatalf("tool definition = %#v", definition)
	}
	output, err := tool.Execute(ctx, json.RawMessage(`{"value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != `{"value":1}` {
		t.Fatalf("tool output = %s", output)
	}

	workflow := contractWorkflow{}
	result, err := workflow.Run(ctx, RunInput{ThreadID: "thread-1", Messages: []Message{message}, Metadata: map[string]string{"source": "test"}})
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Definition().ID != "echo" || result.Status != RunStatusSucceeded || result.Messages[0] != message || result.Metadata["source"] != "test" {
		t.Fatalf("workflow result = %#v", result)
	}
}

func TestCanonicalRuntimeValues(t *testing.T) {
	t.Parallel()

	finishReasons := map[FinishReason]string{
		FinishReasonStop:        "stop",
		FinishReasonLength:      "length",
		FinishReasonToolCalls:   "tool_calls",
		FinishReasonContent:     "content_filter",
		FinishReasonCancelled:   "cancelled",
		FinishReasonUnspecified: "unspecified",
	}
	for reason, want := range finishReasons {
		if string(reason) != want {
			t.Fatalf("FinishReason = %q, want %q", reason, want)
		}
	}

	statuses := map[RunStatus]string{
		RunStatusPending:   "pending",
		RunStatusRunning:   "running",
		RunStatusSucceeded: "succeeded",
		RunStatusFailed:    "failed",
		RunStatusCancelled: "cancelled",
		RunStatusSuspended: "suspended",
	}
	for status, want := range statuses {
		if string(status) != want {
			t.Fatalf("RunStatus = %q, want %q", status, want)
		}
	}
}
