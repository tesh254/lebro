package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewAgentValidatesConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  AgentConfig
		want string
	}{
		{
			name: "missing definition ID",
			cfg:  AgentConfig{Model: echoModel{}},
			want: "agent definition ID is required",
		},
		{
			name: "missing model",
			cfg:  AgentConfig{Definition: AgentDefinition{ID: "agent"}},
			want: "agent model or router is required",
		},
		{
			name: "nil model",
			cfg:  AgentConfig{Definition: AgentDefinition{ID: "agent"}, Model: (*echoModel)(nil)},
			want: "agent model or router is required",
		},
		{
			name: "tools referenced without registry",
			cfg: AgentConfig{
				Definition: AgentDefinition{ID: "agent", Tools: []ToolID{"lookup"}},
				Model:      echoModel{},
			},
			want: "agent tool registry is required when the definition references tools",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewAgent(test.cfg)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewAgent() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAgentDefaultsMaxStepsWhenZero(t *testing.T) {
	t.Parallel()
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent"},
		Model:      echoModel{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if agent.maxSteps != DefaultAgentMaxSteps {
		t.Fatalf("maxSteps = %d, want %d", agent.maxSteps, DefaultAgentMaxSteps)
	}
}

func TestAgentImplementsWorkflow(t *testing.T) {
	t.Parallel()
	var _ Workflow = (*Agent)(nil)
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent-1", Name: "Echo", Instructions: "be brief"},
		Model:      echoModel{},
	})
	if err != nil {
		t.Fatal(err)
	}
	def := agent.Definition()
	if def.ID != WorkflowID("agent-1") || def.Name != "Echo" || def.Description != "be brief" {
		t.Fatalf("Definition() = %#v", def)
	}
}

func TestAgentCompletesTextOnlyRequest(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(textResponse("hello back"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "echo", Model: "fixture-model", Instructions: "be brief"},
		Model:      model,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
		Metadata: map[string]string{"request_id": "req-1"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
	if result.Metadata["request_id"] != "req-1" {
		t.Fatalf("metadata = %#v", result.Metadata)
	}
	if len(result.Messages) != 3 {
		t.Fatalf("transcript length = %d, want 3 (system+user+assistant)", len(result.Messages))
	}
	if result.Messages[0].Role != RoleSystem || result.Messages[0].Content != "be brief" {
		t.Fatalf("system message = %#v", result.Messages[0])
	}
	if result.Messages[1].Role != RoleUser || result.Messages[1].Content != "hello" {
		t.Fatalf("user message = %#v", result.Messages[1])
	}
	if result.Messages[2].Role != RoleAssistant || result.Messages[2].Content != "hello back" {
		t.Fatalf("assistant message = %#v", result.Messages[2])
	}
	if !strings.HasPrefix(string(result.ID), "agent-run-") {
		t.Fatalf("run ID = %q", result.ID)
	}
	if len(model.calls) != 1 {
		t.Fatalf("model calls = %d, want 1", len(model.calls))
	}
	if len(model.calls[0].Tools) != 0 {
		t.Fatalf("text-only request exposed %d tools", len(model.calls[0].Tools))
	}
}

func TestAgentCompletesMultiToolRequest(t *testing.T) {
	t.Parallel()

	registry, handler := newAgentTestRegistry(t)
	handler.execute = func(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
		return append(json.RawMessage(nil), input...), nil
	}

	calls, err := NewModelToolCalls(
		ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{"city":"Nairobi"}`)},
		ModelToolCall{ID: "call-2", ToolID: "lookup", Arguments: json.RawMessage(`{"city":"Kampala"}`)},
	)
	if err != nil {
		t.Fatal(err)
	}
	model := newScriptedModel(
		scriptedResponse{response: ModelResponse{Message: Message{Role: RoleAssistant, ToolCalls: calls}, FinishReason: FinishReasonToolCalls}},
		textResponse("Nairobi 24.5, Kampala 25.0"),
	)

	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "weather", Model: "fixture-model", Tools: []ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "weather in Nairobi and Kampala?"}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
	// user, assistant(tool calls), tool(Nairobi), tool(Kampala), assistant(final)
	if len(result.Messages) != 5 {
		t.Fatalf("transcript length = %d, want 5", len(result.Messages))
	}
	if result.Messages[1].Role != RoleAssistant || result.Messages[1].ToolCalls.IsZero() {
		t.Fatalf("assistant tool-call message = %#v", result.Messages[1])
	}
	if result.Messages[2].Role != RoleTool || result.Messages[2].ToolCallID != "call-1" {
		t.Fatalf("first tool result = %#v", result.Messages[2])
	}
	if result.Messages[3].Role != RoleTool || result.Messages[3].ToolCallID != "call-2" {
		t.Fatalf("second tool result = %#v", result.Messages[3])
	}
	if result.Messages[2].ToolCallID == result.Messages[3].ToolCallID {
		t.Fatal("tool call IDs must be distinct")
	}
	if result.Messages[4].Content != "Nairobi 24.5, Kampala 25.0" {
		t.Fatalf("final message = %#v", result.Messages[4])
	}
	if len(model.calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(model.calls))
	}
	if model.calls[1].Messages[2].ToolCallID == "" {
		t.Fatalf("second model request did not carry tool result: %#v", model.calls[1].Messages)
	}
}

func TestAgentFailsWhenModelRequestsUnknownTool(t *testing.T) {
	t.Parallel()

	registry, _ := newAgentTestRegistry(t)
	model := newScriptedModel(toolCallResponse(ModelToolCall{ID: "call-1", ToolID: "missing", Arguments: json.RawMessage(`{}`)}))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model", Tools: []ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "use missing tool"}},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want unknown tool failure")
	}
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != AgentErrorUnknownTool {
		t.Fatalf("error = %v, want AgentErrorUnknownTool", err)
	}
	if !errors.Is(err, ErrAgentUnknownTool) {
		t.Fatalf("errors.Is(ErrAgentUnknownTool) = false")
	}
	if result.Status != RunStatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	// user + assistant(tool call) + tool(error) = 3
	if len(result.Messages) != 3 {
		t.Fatalf("transcript length = %d, want 3", len(result.Messages))
	}
	if result.Messages[2].Role != RoleTool {
		t.Fatalf("tool error message = %#v", result.Messages[2])
	}
}

func TestAgentFailsOnInvalidToolArguments(t *testing.T) {
	t.Parallel()

	registry := registryForAgentTest(t, validatingSchema{validate: func(value json.RawMessage) *ValidationError {
		return &ValidationError{Target: ValidationTargetToolInput, Issues: []ValidationIssue{{Path: "/city", Keyword: "type", Message: "must be string"}}}
	}})
	model := newScriptedModel(toolCallResponse(ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{"city":42}`)}))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model", Tools: []ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "lookup with bad args"}},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want invalid arguments failure")
	}
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != AgentErrorInvalidToolArguments {
		t.Fatalf("error = %v, want AgentErrorInvalidToolArguments", err)
	}
	if !errors.Is(err, ErrAgentInvalidToolArguments) {
		t.Fatalf("errors.Is(ErrAgentInvalidToolArguments) = false")
	}
	if result.Status != RunStatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}
}

func TestAgentFailsOnToolHandlerError(t *testing.T) {
	t.Parallel()

	registry, handler := newAgentTestRegistry(t)
	handler.execute = func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return nil, errors.New("handler offline")
	}
	model := newScriptedModel(toolCallResponse(ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{}`)}))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model", Tools: []ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "lookup"}},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want tool failure")
	}
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != AgentErrorToolFailure {
		t.Fatalf("error = %v, want AgentErrorToolFailure", err)
	}
	if !errors.Is(err, ErrAgentToolFailure) {
		t.Fatalf("errors.Is(ErrAgentToolFailure) = false")
	}
}

func TestAgentFailsOnToolHandlerPanic(t *testing.T) {
	t.Parallel()

	registry, handler := newAgentTestRegistry(t)
	handler.execute = func(context.Context, json.RawMessage) (json.RawMessage, error) {
		panic("boom")
	}
	model := newScriptedModel(toolCallResponse(ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{}`)}))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model", Tools: []ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "lookup"}},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want tool failure")
	}
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != AgentErrorToolFailure {
		t.Fatalf("error = %v, want AgentErrorToolFailure", err)
	}
}

func TestAgentFailsOnInvalidToolOutput(t *testing.T) {
	t.Parallel()

	inputSchema := json.RawMessage(`{"type":"object","x":"input"}`)
	outputSchema := json.RawMessage(`{"type":"object","x":"output"}`)
	tool := toolFunc{
		definition: ToolDefinition{ID: "lookup", InputSchema: inputSchema, OutputSchema: outputSchema},
		execute: func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`"not an object"`), nil
		},
	}
	registry, err := NewToolRegistry(stubSchemaCompiler{compile: func(schema json.RawMessage) (CompiledSchema, error) {
		if string(schema) == string(outputSchema) {
			return validatingSchema{validate: func(json.RawMessage) *ValidationError {
				return &ValidationError{Target: ValidationTargetToolOutput, Issues: []ValidationIssue{{Path: "", Keyword: "type", Message: "must be object"}}}
			}}, nil
		}
		return stubCompiledSchema{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}

	model := newScriptedModel(toolCallResponse(ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{}`)}))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model", Tools: []ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "lookup"}},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want invalid output failure")
	}
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != AgentErrorInvalidToolOutput {
		t.Fatalf("error = %v, want AgentErrorInvalidToolOutput", err)
	}
	if !errors.Is(err, ErrAgentInvalidToolOutput) {
		t.Fatalf("errors.Is(ErrAgentInvalidToolOutput) = false")
	}
}

func TestAgentFailsOnProviderFailure(t *testing.T) {
	t.Parallel()

	providerErr := &ModelError{Kind: ModelErrorUnavailable, Provider: "fixture", Message: "offline"}
	model := newScriptedModel(scriptedResponse{err: providerErr})
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model"},
		Model:      model,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want provider failure")
	}
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != AgentErrorProviderFailure {
		t.Fatalf("error = %v, want AgentErrorProviderFailure", err)
	}
	if !errors.Is(err, ErrAgentProviderFailure) {
		t.Fatalf("errors.Is(ErrAgentProviderFailure) = false")
	}
	var wrapped *ModelError
	if !errors.As(err, &wrapped) || wrapped.Kind != ModelErrorUnavailable {
		t.Fatalf("wrapped model error = %v", err)
	}
}

func TestAgentFailsOnInvalidModelResponse(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(scriptedResponse{response: ModelResponse{
		Message:      Message{Role: RoleUser, Content: "wrong role"},
		FinishReason: FinishReasonStop,
	}})
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model"},
		Model:      model,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want provider failure from invalid response")
	}
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != AgentErrorProviderFailure {
		t.Fatalf("error = %v, want AgentErrorProviderFailure", err)
	}
}

func TestAgentFailsOnStepLimitExhaustion(t *testing.T) {
	t.Parallel()

	registry, handler := newAgentTestRegistry(t)
	handler.execute = func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	}
	fixtures := make([]scriptedResponse, 0, 5)
	for i := 0; i < 5; i++ {
		fixtures = append(fixtures, toolCallResponse(ModelToolCall{ID: fmt.Sprintf("call-%d", i), ToolID: "lookup", Arguments: json.RawMessage(`{}`)}))
	}
	model := newScriptedModel(fixtures...)

	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model", Tools: []ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
		MaxSteps:   3,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "loop"}},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want step limit exhausted")
	}
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != AgentErrorStepLimitExhausted {
		t.Fatalf("error = %v, want AgentErrorStepLimitExhausted", err)
	}
	if agentErr.Step != 3 {
		t.Fatalf("step = %d, want 3", agentErr.Step)
	}
	if !errors.Is(err, ErrAgentStepLimitExhausted) {
		t.Fatalf("errors.Is(ErrAgentStepLimitExhausted) = false")
	}
	if result.Status != RunStatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}
}

func TestAgentHonoursCancelledContext(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(scriptedResponse{waitForCancel: true})
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model"},
		Model:      model,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := agent.Run(ctx, RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want cancellation")
	}
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != AgentErrorCancelled {
		t.Fatalf("error = %v, want AgentErrorCancelled", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(context.Canceled) = false")
	}
	if !errors.Is(err, ErrAgentCancelled) {
		t.Fatalf("errors.Is(ErrAgentCancelled) = false")
	}
	if result.Status != RunStatusCancelled {
		t.Fatalf("status = %q, want cancelled", result.Status)
	}
}

func TestAgentHonoursCancelledContextMidRun(t *testing.T) {
	t.Parallel()

	registry, handler := newAgentTestRegistry(t)
	calls := make(chan struct{}, 1)
	handler.execute = func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
		select {
		case calls <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	model := newScriptedModel(
		toolCallResponse(ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{}`)}),
		textResponse("never reached"),
	)
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model", Tools: []ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-calls
		cancel()
	}()
	result, err := agent.Run(ctx, RunInput{
		Messages: []Message{{Role: RoleUser, Content: "lookup"}},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if result.Status != RunStatusCancelled {
		t.Fatalf("status = %q, want cancelled", result.Status)
	}
}

func TestAgentHonoursDeadline(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(scriptedResponse{waitForCancel: true})
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model"},
		Model:      model,
		Deadline:   5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if result.Status != RunStatusCancelled {
		t.Fatalf("status = %q, want cancelled", result.Status)
	}
}

func TestAgentRejectsUnregisteredDefinitionTool(t *testing.T) {
	t.Parallel()

	registry, _ := newAgentTestRegistry(t)
	model := newScriptedModel(textResponse("unused"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model", Tools: []ToolID{"lookup", "missing"}},
		Model:      model,
		Tools:      registry,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want unknown tool failure")
	}
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != AgentErrorUnknownTool {
		t.Fatalf("error = %v, want AgentErrorUnknownTool", err)
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error = %v, want mention of missing tool", err)
	}
}

func TestAgentRejectsDuplicateDefinitionTools(t *testing.T) {
	t.Parallel()

	registry, _ := newAgentTestRegistry(t)
	model := newScriptedModel(textResponse("unused"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model", Tools: []ToolID{"lookup", "lookup"}},
		Model:      model,
		Tools:      registry,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want unknown tool failure")
	}
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != AgentErrorUnknownTool {
		t.Fatalf("error = %v, want AgentErrorUnknownTool", err)
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error = %v, want mention of duplicate tool", err)
	}
}

func TestAgentRejectsInvalidRunInputMessage(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(textResponse("unused"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model"},
		Model:      model,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: "invalid", Content: "hello"}},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want invalid message failure")
	}
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != AgentErrorProviderFailure {
		t.Fatalf("error = %v, want AgentErrorProviderFailure", err)
	}
	if !strings.Contains(err.Error(), "run input message 0") {
		t.Fatalf("error = %v, want mention of run input message index", err)
	}
}

func TestAgentPropagatesToolMetadata(t *testing.T) {
	t.Parallel()

	registry, handler := newAgentTestRegistry(t)
	var observed map[string]string
	handler.execute = func(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
		observed = ToolMetadataFromContext(ctx)
		return json.RawMessage(`{}`), nil
	}
	model := newScriptedModel(
		toolCallResponse(ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{}`)}),
		textResponse("done"),
	)
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "weather", Model: "fixture-model", Tools: []ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "lookup"}},
		Metadata: map[string]string{"request_id": "req-42"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if observed == nil {
		t.Fatal("tool metadata not observed")
	}
	if observed["request_id"] != "req-42" {
		t.Fatalf("request_id = %q, want req-42", observed["request_id"])
	}
	if observed["run_id"] == "" {
		t.Fatal("run_id metadata missing")
	}
	if observed["step"] != "1" {
		t.Fatalf("step = %q, want 1", observed["step"])
	}
	if observed["tool_call_id"] != "call-1" {
		t.Fatalf("tool_call_id = %q, want call-1", observed["tool_call_id"])
	}
}

func TestAgentDoesNotMutateCallerInput(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(textResponse("hi"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model"},
		Model:      model,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
		Metadata: map[string]string{"k": "v"},
	}
	if _, err := agent.Run(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if len(input.Messages) != 1 || input.Messages[0].Content != "hello" {
		t.Fatalf("caller input messages mutated: %#v", input.Messages)
	}
}

func TestAgentRunIsConcurrencySafe(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(
		textResponse("a"),
		textResponse("b"),
		textResponse("c"),
		textResponse("d"),
	)
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model"},
		Model:      model,
	})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			_, err := agent.Run(context.Background(), RunInput{
				Messages: []Message{{Role: RoleUser, Content: "x"}},
			})
			done <- err
		}()
	}
	for i := 0; i < 4; i++ {
		if err := <-done; err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	}
}

func TestAgentErrorFormatting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  *AgentError
		want string
	}{
		{name: "nil", err: nil, want: "lebro: agent failure"},
		{name: "no cause", err: &AgentError{Kind: AgentErrorStepLimitExhausted, Step: 2}, want: "step_limit_exhausted at step 2"},
		{name: "with cause", err: &AgentError{Kind: AgentErrorToolFailure, Step: 1, Err: errors.New("boom")}, want: "tool_failure at step 1: boom"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.err.Error(); !strings.Contains(got, test.want) {
				t.Fatalf("Error() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestAgentSentinelErrorsAreDistinct(t *testing.T) {
	t.Parallel()
	sentinels := []error{
		ErrAgentUnknownTool, ErrAgentInvalidToolArguments, ErrAgentInvalidToolOutput,
		ErrAgentToolFailure, ErrAgentProviderFailure, ErrAgentStepLimitExhausted, ErrAgentCancelled,
	}
	for i, a := range sentinels {
		for j, b := range sentinels {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Fatalf("sentinel %v matches %v", a, b)
			}
		}
	}
}

func TestAgentEnforcesToolAllowlistAgainstRegistry(t *testing.T) {
	t.Parallel()

	registry, err := NewToolRegistry(stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		return stubCompiledSchema{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	mustRegister := func(id ToolID) {
		t.Helper()
		tool := toolFunc{
			definition: ToolDefinition{ID: id, InputSchema: json.RawMessage(`{"type":"object"}`)},
			execute:    func(context.Context, json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`{}`), nil },
		}
		if err := registry.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	mustRegister("allowed")
	mustRegister("forbidden")

	model := newScriptedModel(toolCallResponse(ModelToolCall{ID: "call-1", ToolID: "forbidden", Arguments: json.RawMessage(`{}`)}))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model", Tools: []ToolID{"allowed"}},
		Model:      model,
		Tools:      registry,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "use forbidden tool"}},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want unknown tool failure")
	}
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != AgentErrorUnknownTool {
		t.Fatalf("error = %v, want AgentErrorUnknownTool", err)
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("error = %v, want mention of not-allowed tool", err)
	}
	if result.Status != RunStatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}
}

func TestAgentRejectsToolCallsWhenDefinitionHasNoTools(t *testing.T) {
	t.Parallel()

	registry, _ := newAgentTestRegistry(t)
	model := newScriptedModel(toolCallResponse(ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{}`)}))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model"},
		Model:      model,
		Tools:      registry,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "use lookup"}},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want unknown tool failure")
	}
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != AgentErrorUnknownTool {
		t.Fatalf("error = %v, want AgentErrorUnknownTool", err)
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("error = %v, want mention of not-allowed tool", err)
	}
	if result.Status != RunStatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}
}

func TestAgentCopiesDefinitionToolIDsToAvoidCallerMutation(t *testing.T) {
	t.Parallel()

	def := AgentDefinition{ID: "agent", Model: "fixture-model", Tools: []ToolID{"lookup"}}
	registry, _ := newAgentTestRegistry(t)
	agent, err := NewAgent(AgentConfig{Definition: def, Model: echoModel{}, Tools: registry})
	if err != nil {
		t.Fatal(err)
	}
	def.Tools[0] = "tampered"
	if agent.definition.Tools[0] != "lookup" {
		t.Fatalf("agent definition tool mutated to %q", agent.definition.Tools[0])
	}
}

func TestAgentDoesNotAppendZeroMessageOnProviderFailure(t *testing.T) {
	t.Parallel()

	registry, handler := newAgentTestRegistry(t)
	handler.execute = func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	}
	model := newScriptedModel(scriptedResponse{err: errors.New("provider gone")})
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model", Tools: []ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want provider failure")
	}
	if result.Status != RunStatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	for i, message := range result.Messages {
		if message.Role == "" {
			t.Fatalf("result message %d is a zero-value Message: %#v", i, message)
		}
	}
}

func TestAgentFailOnNil(t *testing.T) {
	t.Parallel()
	var agent *Agent
	_, err := agent.Run(context.Background(), RunInput{})
	if err == nil {
		t.Fatal("Run() error = nil, want nil-agent failure")
	}
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != AgentErrorProviderFailure {
		t.Fatalf("error = %v, want AgentErrorProviderFailure", err)
	}
}

func TestNewAgentValidatesOutputSchema(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  AgentConfig
		want string
	}{
		{
			name: "empty output schema",
			cfg: AgentConfig{
				Definition:     AgentDefinition{ID: "agent"},
				Model:          echoModel{},
				SchemaCompiler: stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) { return stubCompiledSchema{}, nil }},
				OutputSchema:   &ModelOutputSchema{Name: "result", Schema: json.RawMessage(``)},
			},
			want: "agent output schema must not be empty",
		},
		{
			name: "invalid output schema JSON",
			cfg: AgentConfig{
				Definition:     AgentDefinition{ID: "agent"},
				Model:          echoModel{},
				SchemaCompiler: stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) { return stubCompiledSchema{}, nil }},
				OutputSchema:   &ModelOutputSchema{Name: "result", Schema: json.RawMessage(`{`)},
			},
			want: "agent output schema must be valid JSON",
		},
		{
			name: "missing schema compiler",
			cfg: AgentConfig{
				Definition:   AgentDefinition{ID: "agent"},
				Model:        echoModel{},
				OutputSchema: &ModelOutputSchema{Name: "result", Schema: json.RawMessage(`{"type":"object"}`)},
			},
			want: "agent schema compiler is required when output schema is set",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewAgent(test.cfg)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewAgent() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAgentStructuredOutputSuccess(t *testing.T) {
	t.Parallel()

	schema := json.RawMessage(`{"type":"object"}`)
	model := newScriptedModel(structuredResponse(json.RawMessage(`{"temperature_c":24.5}`)))
	agent, err := NewAgent(AgentConfig{
		Definition:     AgentDefinition{ID: "weather", Model: "fixture-model"},
		Model:          model,
		SchemaCompiler: stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) { return stubCompiledSchema{}, nil }},
		OutputSchema:   &ModelOutputSchema{Name: "weather_result", Schema: schema},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "what is the weather?"}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
	output := result.StructuredOutput()
	if output == "" {
		t.Fatal("StructuredOutput() is empty")
	}
	if string(output.Raw()) != `{"temperature_c":24.5}` {
		t.Fatalf("StructuredOutput() = %s", output.Raw())
	}
	var decoded struct {
		TemperatureC float64 `json:"temperature_c"`
	}
	if err := result.DecodeStructuredOutput(&decoded); err != nil {
		t.Fatalf("DecodeStructuredOutput() error = %v", err)
	}
	if decoded.TemperatureC != 24.5 {
		t.Fatalf("decoded temperature = %v, want 24.5", decoded.TemperatureC)
	}
	if len(model.calls) == 0 || model.calls[0].OutputSchema == nil {
		t.Fatal("model request did not carry OutputSchema")
	}
	if model.calls[0].OutputSchema.Name != "weather_result" {
		t.Fatalf("request OutputSchema name = %q", model.calls[0].OutputSchema.Name)
	}
}

func TestAgentStructuredOutputMissing(t *testing.T) {
	t.Parallel()

	agent, err := NewAgent(AgentConfig{
		Definition:     AgentDefinition{ID: "weather", Model: "fixture-model"},
		Model:          newScriptedModel(textResponse("it is sunny")),
		SchemaCompiler: stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) { return stubCompiledSchema{}, nil }},
		OutputSchema:   &ModelOutputSchema{Name: "weather_result", Schema: json.RawMessage(`{"type":"object"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "weather?"}},
	})
	if !errors.Is(err, ErrAgentInvalidStructuredOutput) {
		t.Fatalf("error = %v, want ErrAgentInvalidStructuredOutput", err)
	}
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != AgentErrorInvalidStructuredOutput {
		t.Fatalf("error = %v, want AgentErrorInvalidStructuredOutput", err)
	}
	if !strings.Contains(err.Error(), "structured output is missing") {
		t.Fatalf("error = %v, want missing message", err)
	}
}

func TestAgentStructuredOutputMalformedJSON(t *testing.T) {
	t.Parallel()

	agent, err := NewAgent(AgentConfig{
		Definition:     AgentDefinition{ID: "weather", Model: "fixture-model"},
		Model:          newScriptedModel(structuredResponse(json.RawMessage(`{`))),
		SchemaCompiler: stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) { return stubCompiledSchema{}, nil }},
		OutputSchema:   &ModelOutputSchema{Name: "weather_result", Schema: json.RawMessage(`{"type":"object"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "weather?"}},
	})
	if !errors.Is(err, ErrAgentInvalidStructuredOutput) {
		t.Fatalf("error = %v, want ErrAgentInvalidStructuredOutput", err)
	}
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != AgentErrorInvalidStructuredOutput {
		t.Fatalf("error = %v, want AgentErrorInvalidStructuredOutput", err)
	}
	if !strings.Contains(err.Error(), "structured output must be valid JSON") {
		t.Fatalf("error = %v, want valid JSON message", err)
	}
}

func TestAgentStructuredOutputMalformedJSONUnrelatedValidationFailure(t *testing.T) {
	t.Parallel()

	nonAssistantWithMalformedOutput := scriptedResponse{response: ModelResponse{
		Message:      Message{Role: RoleUser, StructuredOutput: NewModelStructuredOutput(json.RawMessage(`{`))},
		FinishReason: FinishReasonStop,
	}}
	agent, err := NewAgent(AgentConfig{
		Definition:     AgentDefinition{ID: "weather", Model: "fixture-model"},
		Model:          newScriptedModel(nonAssistantWithMalformedOutput),
		SchemaCompiler: stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) { return stubCompiledSchema{}, nil }},
		OutputSchema:   &ModelOutputSchema{Name: "weather_result", Schema: json.RawMessage(`{"type":"object"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "weather?"}},
	})
	if errors.Is(err, ErrAgentInvalidStructuredOutput) {
		t.Fatalf("error = %v, should not be ErrAgentInvalidStructuredOutput for unrelated validation failure", err)
	}
	if !errors.Is(err, ErrAgentProviderFailure) {
		t.Fatalf("error = %v, want ErrAgentProviderFailure for unrelated validation failure", err)
	}
}

func TestRunResultStructuredOutputReturnsFinalAssistantOnly(t *testing.T) {
	t.Parallel()

	earlier := Message{Role: RoleAssistant, StructuredOutput: NewModelStructuredOutput(json.RawMessage(`{"earlier":true}`))}
	final := Message{Role: RoleAssistant, Content: "no structured payload here"}
	result := RunResult{Messages: []Message{{Role: RoleUser, Content: "hi"}, earlier, {Role: RoleTool, ToolCallID: "call-1", Content: "{}"}, final}}
	if got := result.StructuredOutput(); got != "" {
		t.Fatalf("StructuredOutput() = %q, want empty (final assistant has no payload)", got)
	}

	finalWithPayload := Message{Role: RoleAssistant, StructuredOutput: NewModelStructuredOutput(json.RawMessage(`{"final":true}`))}
	result2 := RunResult{Messages: []Message{earlier, finalWithPayload}}
	if got := string(result2.StructuredOutput().Raw()); got != `{"final":true}` {
		t.Fatalf("StructuredOutput() = %s, want final payload", got)
	}
}

func TestAgentStructuredOutputSchemaMismatch(t *testing.T) {
	t.Parallel()

	validationErr := &ValidationError{
		Target: ValidationTargetStructuredOutput,
		Issues: []ValidationIssue{{Path: "/", Keyword: "type", Message: "expected object"}},
	}
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "weather", Model: "fixture-model"},
		Model:      newScriptedModel(structuredResponse(json.RawMessage(`"not-an-object"`))),
		SchemaCompiler: stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
			return stubCompiledSchema{validationErr: validationErr}, nil
		}},
		OutputSchema: &ModelOutputSchema{Name: "weather_result", Schema: json.RawMessage(`{"type":"object"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "weather?"}},
	})
	if !errors.Is(err, ErrAgentInvalidStructuredOutput) {
		t.Fatalf("error = %v, want ErrAgentInvalidStructuredOutput", err)
	}
	var validationFailure *ValidationError
	if !errors.As(err, &validationFailure) {
		t.Fatalf("error = %v, want wrapped *ValidationError", err)
	}
	if validationFailure.Target != ValidationTargetStructuredOutput {
		t.Fatalf("validation target = %q, want structured_output", validationFailure.Target)
	}
}

func TestAgentStructuredOutputAfterToolUse(t *testing.T) {
	t.Parallel()

	registry, err := NewToolRegistry(stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		return stubCompiledSchema{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(toolFunc{
		definition: ToolDefinition{ID: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)},
		execute: func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"city":"nairobi"}`), nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	call := ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{"q":"nairobi"}`)}
	model := newScriptedModel(toolCallThenStructuredResponse(call, json.RawMessage(`{"temperature_c":21}`))...)
	agent, err := NewAgent(AgentConfig{
		Definition:     AgentDefinition{ID: "weather", Model: "fixture-model", Tools: []ToolID{"lookup"}},
		Model:          model,
		Tools:          registry,
		SchemaCompiler: stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) { return stubCompiledSchema{}, nil }},
		OutputSchema:   &ModelOutputSchema{Name: "weather_result", Schema: json.RawMessage(`{"type":"object"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := agent.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "weather in nairobi?"}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
	if result.StructuredOutput() == "" {
		t.Fatal("expected structured output after tool use")
	}
	if len(model.calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(model.calls))
	}
	if model.calls[0].OutputSchema == nil || model.calls[1].OutputSchema == nil {
		t.Fatal("OutputSchema not forwarded on every step")
	}
}

func TestAgentRunOutputSchemaOverride(t *testing.T) {
	t.Parallel()

	compiledRuns := 0
	var compiled CompiledSchema = stubCompiledSchema{}
	compiler := stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		compiledRuns++
		return compiled, nil
	}}
	agent, err := NewAgent(AgentConfig{
		Definition:     AgentDefinition{ID: "agent", Model: "fixture-model"},
		Model:          newScriptedModel(structuredResponse(json.RawMessage(`{"ok":true}`))),
		SchemaCompiler: compiler,
		OutputSchema:   &ModelOutputSchema{Name: "default", Schema: json.RawMessage(`{"type":"object"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if compiledRuns != 1 {
		t.Fatalf("agent-level compiles = %d, want 1", compiledRuns)
	}

	override := &ModelOutputSchema{Name: "override", Schema: json.RawMessage(`{"type":"object","required":["ok"]}`)}
	result, err := agent.Run(context.Background(), RunInput{
		Messages:     []Message{{Role: RoleUser, Content: "hi"}},
		OutputSchema: override,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if compiledRuns != 2 {
		t.Fatalf("total compiles = %d, want 2 (agent + run override)", compiledRuns)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
}

func TestAgentRunOutputSchemaOverrideWithoutCompiler(t *testing.T) {
	t.Parallel()

	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model"},
		Model:      newScriptedModel(structuredResponse(json.RawMessage(`{}`))),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = agent.Run(context.Background(), RunInput{
		Messages:     []Message{{Role: RoleUser, Content: "hi"}},
		OutputSchema: &ModelOutputSchema{Name: "override", Schema: json.RawMessage(`{"type":"object"}`)},
	})
	if !errors.Is(err, ErrAgentInvalidStructuredOutput) {
		t.Fatalf("error = %v, want ErrAgentInvalidStructuredOutput", err)
	}
	if !strings.Contains(err.Error(), "schema compiler is required") {
		t.Fatalf("error = %v, want compiler required message", err)
	}
}

func TestRunResultDecodeStructuredOutputEmpty(t *testing.T) {
	t.Parallel()

	result := RunResult{Messages: []Message{{Role: RoleAssistant, Content: "no json here"}}}
	if result.StructuredOutput() != "" {
		t.Fatalf("StructuredOutput() = %q, want empty", result.StructuredOutput())
	}
	var v map[string]any
	if err := result.DecodeStructuredOutput(&v); err == nil {
		t.Fatal("DecodeStructuredOutput() error = nil, want error")
	}
}

func TestAgentPersistsThreadOnSuccess(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	model := newScriptedModel(textResponse("hello back"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "persist-agent", Instructions: "be brief"},
		Model:      model,
		Store:      store,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := agent.Run(context.Background(), RunInput{
		ThreadID: "thread-persist-1",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}

	page, err := store.Messages().ListMessages(context.Background(), "thread-persist-1", PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 2 {
		t.Fatalf("stored messages = %d, want 2 (user + assistant)", len(page.Records))
	}
	if page.Records[0].Message.Role != RoleUser || page.Records[0].Message.Content != "hello" {
		t.Fatalf("stored user message = %#v", page.Records[0].Message)
	}
	if page.Records[1].Message.Role != RoleAssistant || page.Records[1].Message.Content != "hello back" {
		t.Fatalf("stored assistant message = %#v", page.Records[1].Message)
	}
}

func TestAgentLoadsPriorMessagesOnSecondRun(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	ctx := context.Background()
	now := timeUTC()
	if err := store.Threads().CreateThread(ctx, ThreadRecord{ID: "thread-load-1", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.Messages().AppendMessages(ctx, []MessageRecord{
		{ID: "msg-1", ThreadID: "thread-load-1", Message: Message{Role: RoleUser, Content: "first question"}, CreatedAt: now},
		{ID: "msg-2", ThreadID: "thread-load-1", Message: Message{Role: RoleAssistant, Content: "first answer"}, CreatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}

	var capturedRequest ModelRequest
	model := &capturingModel{capture: &capturedRequest, response: textResponse("second answer").response}
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "load-agent", Instructions: "be brief"},
		Model:      model,
		Store:      store,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := agent.Run(ctx, RunInput{
		ThreadID: "thread-load-1",
		Messages: []Message{{Role: RoleUser, Content: "second question"}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}

	roles := []Role{RoleSystem, RoleUser, RoleAssistant, RoleUser}
	if len(capturedRequest.Messages) != len(roles) {
		t.Fatalf("model received %d messages, want %d", len(capturedRequest.Messages), len(roles))
	}
	for i, want := range roles {
		if capturedRequest.Messages[i].Role != want {
			t.Fatalf("message %d role = %q, want %q", i, capturedRequest.Messages[i].Role, want)
		}
	}
	if capturedRequest.Messages[1].Content != "first question" {
		t.Fatalf("prior user message = %q, want first question", capturedRequest.Messages[1].Content)
	}
	if capturedRequest.Messages[2].Content != "first answer" {
		t.Fatalf("prior assistant message = %q, want first answer", capturedRequest.Messages[2].Content)
	}
	if capturedRequest.Messages[3].Content != "second question" {
		t.Fatalf("new user message = %q, want second question", capturedRequest.Messages[3].Content)
	}

	page, err := store.Messages().ListMessages(ctx, "thread-load-1", PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 4 {
		t.Fatalf("total stored messages = %d, want 4", len(page.Records))
	}
}

func TestAgentFailedRunLeavesPriorMessagesUnchanged(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	ctx := context.Background()
	now := timeUTC()
	if err := store.Threads().CreateThread(ctx, ThreadRecord{ID: "thread-fail-1", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.Messages().AppendMessages(ctx, []MessageRecord{
		{ID: "prior-1", ThreadID: "thread-fail-1", Message: Message{Role: RoleUser, Content: "prior question"}, CreatedAt: now},
		{ID: "prior-2", ThreadID: "thread-fail-1", Message: Message{Role: RoleAssistant, Content: "prior answer"}, CreatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}

	model := newScriptedModel(scriptedResponse{err: errors.New("provider down")})
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "fail-agent"},
		Model:      model,
		Store:      store,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := agent.Run(ctx, RunInput{
		ThreadID: "thread-fail-1",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want provider failure")
	}
	if result.Status != RunStatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}

	page, err := store.Messages().ListMessages(ctx, "thread-fail-1", PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 2 {
		t.Fatalf("stored messages after failed run = %d, want 2 (prior messages unchanged)", len(page.Records))
	}
	if page.Records[0].Message.Content != "prior question" || page.Records[1].Message.Content != "prior answer" {
		t.Fatalf("prior messages changed after failed run: %#v", page.Records)
	}
}

func TestAgentWithoutStoreIsUnchanged(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(textResponse("echo"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "no-store-agent"},
		Model:      model,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := agent.Run(context.Background(), RunInput{
		ThreadID: "thread-no-store",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
	if len(result.Messages) != 2 {
		t.Fatalf("transcript = %d, want 2", len(result.Messages))
	}
}

func TestAgentAutoCreatesThreadOnFirstRun(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	model := newScriptedModel(textResponse("created"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "auto-create-agent"},
		Model:      model,
		Store:      store,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = agent.Run(context.Background(), RunInput{
		ThreadID: "thread-auto-1",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	thread, err := store.Threads().GetThread(context.Background(), "thread-auto-1")
	if err != nil {
		t.Fatalf("GetThread after auto-create: %v", err)
	}
	if thread.ID != "thread-auto-1" {
		t.Fatalf("thread ID = %q, want thread-auto-1", thread.ID)
	}
}

type capturingModel struct {
	capture  *ModelRequest
	response ModelResponse
}

func (m *capturingModel) Generate(_ context.Context, request ModelRequest) (ModelResponse, error) {
	*m.capture = request
	return m.response, nil
}

// scriptedModel is a minimal FIFO model used only by agent tests. The
// internal/testkit harness cannot be imported here because it would create an
// import cycle with the root package.
type scriptedModel struct {
	responses []scriptedResponse
	next      int
	mu        sync.Mutex
	calls     []ModelRequest
}

type scriptedResponse struct {
	response      ModelResponse
	err           error
	waitForCancel bool
}

var _ Model = (*scriptedModel)(nil)

func newScriptedModel(responses ...scriptedResponse) *scriptedModel {
	return &scriptedModel{responses: responses}
}

func (m *scriptedModel) Generate(ctx context.Context, request ModelRequest) (ModelResponse, error) {
	m.mu.Lock()
	m.calls = append(m.calls, request)
	if m.next >= len(m.responses) {
		m.mu.Unlock()
		return ModelResponse{}, errors.New("lebro: scripted model exhausted")
	}
	resp := m.responses[m.next]
	m.next++
	m.mu.Unlock()

	if resp.waitForCancel {
		<-ctx.Done()
		return ModelResponse{}, ctx.Err()
	}
	if resp.err != nil {
		return ModelResponse{}, resp.err
	}
	return resp.response, nil
}

func textResponse(content string) scriptedResponse {
	return scriptedResponse{response: ModelResponse{
		Message:      Message{Role: RoleAssistant, Content: content},
		FinishReason: FinishReasonStop,
	}}
}

func toolCallResponse(call ModelToolCall) scriptedResponse {
	encoded, err := NewModelToolCalls(call)
	if err != nil {
		panic(err)
	}
	return scriptedResponse{response: ModelResponse{
		Message:      Message{Role: RoleAssistant, ToolCalls: encoded},
		FinishReason: FinishReasonToolCalls,
	}}
}

func structuredResponse(value json.RawMessage) scriptedResponse {
	return scriptedResponse{response: ModelResponse{
		Message:      Message{Role: RoleAssistant, StructuredOutput: NewModelStructuredOutput(value)},
		FinishReason: FinishReasonStop,
	}}
}

func toolCallThenStructuredResponse(call ModelToolCall, value json.RawMessage) []scriptedResponse {
	return []scriptedResponse{toolCallResponse(call), structuredResponse(value)}
}

type echoModel struct{}

func (echoModel) Generate(_ context.Context, request ModelRequest) (ModelResponse, error) {
	return ModelResponse{
		Message:      Message{Role: RoleAssistant, Content: request.Messages[0].Content},
		FinishReason: FinishReasonStop,
	}, nil
}

type agentTestTool struct {
	definition ToolDefinition
	execute    func(context.Context, json.RawMessage) (json.RawMessage, error)
}

func (t agentTestTool) Definition() ToolDefinition { return t.definition }
func (t agentTestTool) Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	return t.execute(ctx, input)
}

func newAgentTestRegistry(t *testing.T) (*ToolRegistry, *agentTestTool) {
	t.Helper()
	tool := &agentTestTool{
		definition: ToolDefinition{ID: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)},
		execute:    func(context.Context, json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`{}`), nil },
	}
	registry, err := NewToolRegistry(stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		return stubCompiledSchema{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}
	return registry, tool
}

func registryForAgentTest(t *testing.T, schema CompiledSchema) *ToolRegistry {
	t.Helper()
	tool := toolFunc{
		definition: ToolDefinition{ID: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)},
		execute:    func(context.Context, json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`{}`), nil },
	}
	registry, err := NewToolRegistry(stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		return schema, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}
	return registry
}
