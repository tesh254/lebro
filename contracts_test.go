package lebro

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

var (
	_ Model    = contractModel{}
	_ Tool     = contractTool{}
	_ Workflow = contractWorkflow{}
)

type contractModel struct{}

func (contractModel) Generate(_ context.Context, request ModelRequest) (ModelResponse, error) {
	return ModelResponse{Message: Message{Role: RoleAssistant, Content: request.Messages[0].Content}, FinishReason: FinishReasonStop}, nil
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
	if response.Message.Role != RoleAssistant || response.Message.Content != message.Content || response.FinishReason != FinishReasonStop {
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

func TestMAD18RunRecordPublicContract(t *testing.T) {
	t.Parallel()

	eventTypes := map[RunEventType]string{
		RunEventStarted:       "run_started",
		RunEventModelStarted:  "model_started",
		RunEventModelFinished: "model_finished",
		RunEventToolRequested: "tool_requested",
		RunEventToolStarted:   "tool_started",
		RunEventToolFinished:  "tool_finished",
		RunEventSucceeded:     "run_succeeded",
		RunEventFailed:        "run_failed",
		RunEventCancelled:     "run_cancelled",
	}
	for eventType, want := range eventTypes {
		if string(eventType) != want {
			t.Fatalf("RunEventType = %q, want %q", eventType, want)
		}
	}
	if !RunEventSucceeded.IsTerminal() || !RunEventFailed.IsTerminal() || !RunEventCancelled.IsTerminal() {
		t.Fatal("terminal event types must report IsTerminal() = true")
	}
	if RunEventStarted.IsTerminal() {
		t.Fatal("non-terminal event type reported IsTerminal() = true")
	}

	recorder := NewRunRecorder()
	if recorder.EventCount() != 0 {
		t.Fatalf("new recorder count = %d, want 0", recorder.EventCount())
	}
	if _, ok := recorder.TerminalEvent(); ok {
		t.Fatal("empty recorder should have no terminal event")
	}

	clock := NewFixedClock(time.Unix(0, 0))
	if clock.Now() != time.Unix(0, 0) {
		t.Fatalf("fixed clock = %v, want epoch", clock.Now())
	}

	ids := NewFixedIDSource([]RunID{"run-1"}, []StepID{"step-1"})
	if ids.NewRunID() != "run-1" {
		t.Fatalf("fixed run ID = %q", ids.NewRunID())
	}
	if ids.NewStepID() != "step-1" {
		t.Fatalf("fixed step ID = %q", ids.NewStepID())
	}
}

func TestMAD17AgentLoopPublicContract(t *testing.T) {
	t.Parallel()

	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "echo", Model: "contract-model"},
		Model:      contractModel{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if agent.Definition().ID != "echo" {
		t.Fatalf("Definition() = %#v", agent.Definition())
	}

	result, err := agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
		Metadata: map[string]string{"source": "test"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
	if result.Messages[0].Role != RoleUser || result.Messages[0].Content != "hello" {
		t.Fatalf("user message = %#v", result.Messages[0])
	}
	if result.Messages[1].Content != "hello" {
		t.Fatalf("assistant message = %#v", result.Messages[1])
	}
	if result.Metadata["source"] != "test" {
		t.Fatalf("metadata = %#v", result.Metadata)
	}

	kinds := map[AgentErrorKind]string{
		AgentErrorUnknownTool:          "unknown_tool",
		AgentErrorInvalidToolArguments: "invalid_tool_arguments",
		AgentErrorInvalidToolOutput:    "invalid_tool_output",
		AgentErrorToolFailure:          "tool_failure",
		AgentErrorProviderFailure:      "provider_failure",
		AgentErrorStepLimitExhausted:   "step_limit_exhausted",
		AgentErrorCancelled:            "cancelled",
	}
	for kind, want := range kinds {
		if string(kind) != want {
			t.Fatalf("AgentErrorKind = %q, want %q", kind, want)
		}
	}
	if DefaultAgentMaxSteps <= 0 {
		t.Fatalf("DefaultAgentMaxSteps = %d, want positive", DefaultAgentMaxSteps)
	}

	sentinels := []error{
		ErrAgentUnknownTool, ErrAgentInvalidToolArguments, ErrAgentInvalidToolOutput,
		ErrAgentToolFailure, ErrAgentProviderFailure, ErrAgentStepLimitExhausted, ErrAgentCancelled,
	}
	for _, sentinel := range sentinels {
		if sentinel == nil {
			t.Fatalf("sentinel error is nil: %v", sentinel)
		}
	}
}
