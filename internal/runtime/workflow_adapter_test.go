package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestAgentStepRunsAfterOrdinaryStepAndCorrelatesEvents(t *testing.T) {
	recorder := NewRunRecorder()
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "echo-agent", Model: "fixture"},
		Model:      echoModel{},
		Listener:   recorder,
		IDSource:   NewFixedIDSource([]RunID{"agent-run"}, []StepID{"agent-model"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	agentStep, err := NewAgentStep(agent)
	if err != nil {
		t.Fatal(err)
	}
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "parent-workflow"},
		Listener:   recorder,
		IDSource:   NewFixedIDSource([]RunID{"workflow-run"}, nil),
		Steps: []Step{
			{Definition: StepDefinition{ID: "prepare"}, Handler: StepHandlerFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`"summarize this"`), nil
			})},
			{Definition: StepDefinition{ID: "agent"}, Handler: agentStep},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := wf.Run(context.Background(), WorkflowRunInput{
		Input:    json.RawMessage(`null`),
		ThreadID: "thread-7",
		Metadata: map[string]string{"request_id": "req-7"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Output) != `"summarize this"` {
		t.Fatalf("output = %s", result.Output)
	}

	var nested []RunEvent
	for _, event := range recorder.Events() {
		if event.RunID == "agent-run" {
			nested = append(nested, event)
		}
	}
	if len(nested) == 0 {
		t.Fatal("agent did not emit events")
	}
	for _, event := range nested {
		if event.ParentRunID != "workflow-run" || event.ParentStepID != "agent" || event.ParentStep != 2 {
			t.Fatalf("nested event correlation = %#v", event)
		}
	}
}

func TestToolStepValidatesInputOutputAndForwardsMetadata(t *testing.T) {
	inputSchema := json.RawMessage(`{"input":true}`)
	outputSchema := json.RawMessage(`{"output":true}`)
	compiler := stubSchemaCompiler{compile: func(schema json.RawMessage) (CompiledSchema, error) {
		expected := `{"city":"Nairobi"}`
		if string(schema) == string(outputSchema) {
			expected = `{"temperature_c":24.5}`
		}
		return validatingSchema{validate: func(value json.RawMessage) *ValidationError {
			if string(value) == expected {
				return nil
			}
			return &ValidationError{Issues: []ValidationIssue{{Path: "/", Keyword: "const", Message: "unexpected value"}}}
		}}, nil
	}}
	registry, err := NewToolRegistry(compiler)
	if err != nil {
		t.Fatal(err)
	}
	var gotMetadata map[string]string
	invalidOutput := false
	if err := registry.Register(toolFunc{
		definition: ToolDefinition{ID: "weather", InputSchema: inputSchema, OutputSchema: outputSchema},
		execute: func(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
			gotMetadata = ToolMetadataFromContext(ctx)
			if invalidOutput {
				return json.RawMessage(`{"temperature_c":"unknown"}`), nil
			}
			return json.RawMessage(`{"temperature_c":24.5}`), nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	registered, ok := registry.Resolve("weather")
	if !ok {
		t.Fatal("registered tool not found")
	}
	toolStep, err := NewToolStep(registered)
	if err != nil {
		t.Fatal(err)
	}
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "weather-workflow"},
		Steps:      []Step{{Definition: StepDefinition{ID: "weather"}, Handler: toolStep}},
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata := map[string]string{"request_id": "req-9"}
	result, err := wf.Run(context.Background(), WorkflowRunInput{Input: json.RawMessage(`{"city":"Nairobi"}`), Metadata: metadata})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Output) != `{"temperature_c":24.5}` {
		t.Fatalf("output = %s", result.Output)
	}
	if !reflect.DeepEqual(gotMetadata, metadata) {
		t.Fatalf("tool metadata = %#v, want %#v", gotMetadata, metadata)
	}

	_, err = wf.Run(context.Background(), WorkflowRunInput{Input: json.RawMessage(`{"city":"Kampala"}`)})
	var workflowErr *WorkflowError
	var toolErr *ToolExecutionError
	if !errors.As(err, &workflowErr) || workflowErr.Kind != WorkflowErrorStepFailed || !errors.As(err, &toolErr) || toolErr.State != ToolExecutionInvalidInput {
		t.Fatalf("error = %v", err)
	}

	invalidOutput = true
	_, err = wf.Run(context.Background(), WorkflowRunInput{Input: json.RawMessage(`{"city":"Nairobi"}`)})
	if !errors.As(err, &workflowErr) || workflowErr.Kind != WorkflowErrorStepFailed || !errors.As(err, &toolErr) || toolErr.State != ToolExecutionInvalidOutput {
		t.Fatalf("error = %v", err)
	}
}

func TestAgentStepRejectsMissingAgent(t *testing.T) {
	if _, err := NewAgentStep(nil); err == nil {
		t.Fatal("NewAgentStep(nil) error = nil")
	}
	var step *AgentStep
	if _, err := step.Execute(context.Background(), json.RawMessage(`null`)); err == nil {
		t.Fatal("nil AgentStep Execute() error = nil")
	}
	if _, err := NewToolStep(nil); err == nil {
		t.Fatal("NewToolStep(nil) error = nil")
	}
	var toolStep *ToolStep
	if _, err := toolStep.Execute(context.Background(), json.RawMessage(`null`)); err == nil {
		t.Fatal("nil ToolStep Execute() error = nil")
	}
}

func TestAgentStepForwardsWorkflowThreadMetadataAndContext(t *testing.T) {
	stub := &recordingWorkflow{}
	step, err := NewAgentStep(stub)
	if err != nil {
		t.Fatal(err)
	}
	ctx := withWorkflowInvocation(context.Background(), "parent", 3, "agent", "thread-3", map[string]string{"request_id": "req-3"})
	output, err := step.Execute(ctx, json.RawMessage(`{"task":"summarize"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != `"done"` {
		t.Fatalf("output = %s", output)
	}
	if stub.input.ThreadID != "thread-3" || !reflect.DeepEqual(stub.input.Metadata, map[string]string{"request_id": "req-3"}) {
		t.Fatalf("run input = %#v", stub.input)
	}
	if len(stub.input.Messages) != 1 || stub.input.Messages[0].Role != RoleUser || stub.input.Messages[0].Content != `{"task":"summarize"}` {
		t.Fatalf("messages = %#v", stub.input.Messages)
	}
	if stub.ctx != ctx {
		t.Fatal("agent step did not forward context")
	}
}

func TestAgentStepReturnsStructuredOutputWhenPresent(t *testing.T) {
	stub := &recordingWorkflow{result: RunResult{
		Status: RunStatusSucceeded,
		Messages: []Message{{
			Role:             RoleAssistant,
			StructuredOutput: NewModelStructuredOutput(json.RawMessage(`{"answer":42}`)),
		}},
	}}
	step, err := NewAgentStep(stub)
	if err != nil {
		t.Fatal(err)
	}
	output, err := step.Execute(context.Background(), json.RawMessage(`"ask"`))
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != `{"answer":42}` {
		t.Fatalf("output = %s", output)
	}
}

func TestAgentStepErrorsOnNonSucceededStatus(t *testing.T) {
	stub := &recordingWorkflow{result: RunResult{Status: RunStatusFailed}}
	step, err := NewAgentStep(stub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := step.Execute(context.Background(), json.RawMessage(`"ask"`)); err == nil {
		t.Fatal("non-succeeded status error = nil")
	}
}

func TestAgentStepErrorsWhenNoAssistantMessage(t *testing.T) {
	stub := &recordingWorkflow{result: RunResult{Status: RunStatusSucceeded}}
	step, err := NewAgentStep(stub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := step.Execute(context.Background(), json.RawMessage(`"ask"`)); err == nil {
		t.Fatal("missing assistant message error = nil")
	}
}

func TestAgentStepPromptHandlesNullInput(t *testing.T) {
	if got := workflowAgentPrompt(json.RawMessage(`null`)); got != "null" {
		t.Fatalf("null input prompt = %q", got)
	}
	if got := workflowAgentPrompt(json.RawMessage(`"hi"`)); got != "hi" {
		t.Fatalf("string input prompt = %q", got)
	}
	if got := workflowAgentPrompt(json.RawMessage(`""`)); got != "" {
		t.Fatalf("empty string input prompt = %q", got)
	}
	if got := workflowAgentPrompt(json.RawMessage(`{"task":"go"}`)); got != `{"task":"go"}` {
		t.Fatalf("object input prompt = %q", got)
	}
}

type recordingWorkflow struct {
	ctx    context.Context
	input  RunInput
	result RunResult
}

func (*recordingWorkflow) Definition() WorkflowDefinition { return WorkflowDefinition{ID: "recording"} }

func (w *recordingWorkflow) Run(ctx context.Context, input RunInput) (RunResult, error) {
	w.ctx = ctx
	w.input = input
	if w.result.Status == "" {
		return RunResult{Status: RunStatusSucceeded, Messages: []Message{{Role: RoleAssistant, Content: "done"}}}, nil
	}
	return w.result, nil
}
