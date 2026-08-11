package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// delegationSchemaCompiler is a minimal compiler covering just the keywords
// the delegation schemas use: required properties and additionalProperties.
// The runtime package must stay independent of a JSON Schema library, so tests
// here supply their own rather than importing the jsonschema package.
type delegationSchemaCompiler struct{}

func (delegationSchemaCompiler) Compile(schema json.RawMessage) (CompiledSchema, error) {
	var parsed struct {
		Required             []string                   `json:"required"`
		Properties           map[string]json.RawMessage `json:"properties"`
		AdditionalProperties *bool                      `json:"additionalProperties"`
	}
	if err := json.Unmarshal(schema, &parsed); err != nil {
		return nil, err
	}
	return delegationCompiledSchema{
		required:   parsed.Required,
		properties: parsed.Properties,
		closed:     parsed.AdditionalProperties != nil && !*parsed.AdditionalProperties,
	}, nil
}

type delegationCompiledSchema struct {
	required   []string
	properties map[string]json.RawMessage
	closed     bool
}

func (s delegationCompiledSchema) Validate(value json.RawMessage) *ValidationError {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(value, &object); err != nil {
		return &ValidationError{Issues: []ValidationIssue{{Path: "/", Keyword: "type", Message: "value is not an object"}}}
	}
	var issues []ValidationIssue
	for _, name := range s.required {
		if _, ok := object[name]; !ok {
			issues = append(issues, ValidationIssue{Path: "/" + name, Keyword: "required", Message: "required property is missing"})
		}
	}
	if s.closed {
		for name := range object {
			if _, ok := s.properties[name]; !ok {
				issues = append(issues, ValidationIssue{Path: "/" + name, Keyword: "additionalProperties", Message: "property is not allowed"})
			}
		}
	}
	if len(issues) == 0 {
		return nil
	}
	return &ValidationError{Issues: sortedValidationIssues(issues)}
}

// delegationModel scripts a supervisor that delegates once and then answers.
type delegationModel struct {
	calls    int
	toolID   ToolID
	task     string
	finalTxt string
}

func (m *delegationModel) Generate(_ context.Context, _ ModelRequest) (ModelResponse, error) {
	m.calls++
	if m.calls == 1 {
		arguments, err := json.Marshal(map[string]string{"task": m.task})
		if err != nil {
			return ModelResponse{}, err
		}
		calls, err := NewModelToolCalls(ModelToolCall{ID: "call-1", ToolID: m.toolID, Arguments: arguments})
		if err != nil {
			return ModelResponse{}, err
		}
		return ModelResponse{
			Message:      Message{Role: RoleAssistant, ToolCalls: calls},
			FinishReason: FinishReasonToolCalls,
		}, nil
	}
	return ModelResponse{
		Message:      Message{Role: RoleAssistant, Content: m.finalTxt},
		FinishReason: FinishReasonStop,
	}, nil
}

// delegationEchoModel answers with a fixed string and records what it was asked.
type delegationEchoModel struct {
	reply    string
	requests []ModelRequest
}

func (m *delegationEchoModel) Generate(_ context.Context, request ModelRequest) (ModelResponse, error) {
	m.requests = append(m.requests, request)
	return ModelResponse{
		Message:      Message{Role: RoleAssistant, Content: m.reply},
		FinishReason: FinishReasonStop,
	}, nil
}

// loopingModel never terminates, so it exhausts whatever step budget it is
// given.
type loopingModel struct{ calls int }

func (m *loopingModel) Generate(_ context.Context, _ ModelRequest) (ModelResponse, error) {
	m.calls++
	calls, err := NewModelToolCalls(ModelToolCall{ID: "loop", ToolID: "noop", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		return ModelResponse{}, err
	}
	return ModelResponse{
		Message:      Message{Role: RoleAssistant, ToolCalls: calls},
		FinishReason: FinishReasonToolCalls,
	}, nil
}

// blockingModel waits for the run context to end, so a deadline is observable.
type blockingModel struct{}

func (blockingModel) Generate(ctx context.Context, _ ModelRequest) (ModelResponse, error) {
	<-ctx.Done()
	return ModelResponse{}, ctx.Err()
}

// noopTool is a trivial registered tool for exercising step budgets.
type noopTool struct{}

func (noopTool) Definition() ToolDefinition {
	return ToolDefinition{
		ID:           "noop",
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		OutputSchema: json.RawMessage(`{"type":"object"}`),
	}
}

func (noopTool) Execute(context.Context, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

func newChildAgent(t *testing.T, model Model, id AgentID) *Agent {
	t.Helper()
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: id, Name: string(id)},
		Model:      model,
	})
	if err != nil {
		t.Fatalf("new child agent: %v", err)
	}
	return agent
}

// newSupervisor wires a supervisor whose only tool is the given subagent.
func newSupervisor(t *testing.T, model Model, sub *Subagent, listener RunListener) *Agent {
	t.Helper()
	registry, err := NewToolRegistry(delegationSchemaCompiler{})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	if err := registry.Register(sub); err != nil {
		t.Fatalf("register subagent: %v", err)
	}
	supervisor, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "supervisor", Tools: []ToolID{sub.Definition().ID}},
		Model:      model,
		Tools:      registry,
		Listener:   listener,
	})
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}
	return supervisor
}

func TestSubagentSupervisorSelectsAndInvokes(t *testing.T) {
	child := &delegationEchoModel{reply: "delegated answer"}
	sub, err := NewSubagent(SubagentConfig{
		ID:          "research",
		Agent:       newChildAgent(t, child, "researcher"),
		Description: "Researches a focused question.",
	})
	if err != nil {
		t.Fatalf("new subagent: %v", err)
	}

	supervisorModel := &delegationModel{toolID: "research", task: "find the answer", finalTxt: "done"}
	supervisor := newSupervisor(t, supervisorModel, sub, nil)

	result, err := supervisor.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "answer this"}},
	})
	if err != nil {
		t.Fatalf("supervisor run: %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want %q", result.Status, RunStatusSucceeded)
	}

	if len(child.requests) != 1 {
		t.Fatalf("child model calls = %d, want 1", len(child.requests))
	}
	// The child saw the delegated task, not the supervisor's own prompt.
	childMessages := child.requests[0].Messages
	if len(childMessages) != 1 {
		t.Fatalf("child messages = %d, want 1", len(childMessages))
	}
	if childMessages[0].Content != "find the answer" {
		t.Fatalf("child prompt = %q, want %q", childMessages[0].Content, "find the answer")
	}

	// The supervisor received the delegation result as a tool message.
	var toolMessage *Message
	for i := range result.Messages {
		if result.Messages[i].Role == RoleTool {
			toolMessage = &result.Messages[i]
			break
		}
	}
	if toolMessage == nil {
		t.Fatal("supervisor transcript has no tool result message")
	}
	var decoded subagentOutput
	if err := json.Unmarshal([]byte(toolMessage.Content), &decoded); err != nil {
		t.Fatalf("decode delegation result: %v", err)
	}
	if decoded.AgentID != "researcher" {
		t.Fatalf("agent_id = %q, want %q", decoded.AgentID, "researcher")
	}
	if decoded.Status != string(RunStatusSucceeded) {
		t.Fatalf("status = %q, want %q", decoded.Status, RunStatusSucceeded)
	}
	if decoded.Output != "delegated answer" {
		t.Fatalf("output = %q, want %q", decoded.Output, "delegated answer")
	}
	if decoded.RunID == "" {
		t.Fatal("delegation result carries no child run ID")
	}
}

func TestSubagentBoundsStepsIndependentlyOfParent(t *testing.T) {
	registry, err := NewToolRegistry(delegationSchemaCompiler{})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	if err := registry.Register(noopTool{}); err != nil {
		t.Fatalf("register noop: %v", err)
	}

	childModel := &loopingModel{}
	child, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "looper", Tools: []ToolID{"noop"}},
		Model:      childModel,
		Tools:      registry,
		MaxSteps:   50,
	})
	if err != nil {
		t.Fatalf("new child: %v", err)
	}

	sub, err := NewSubagent(SubagentConfig{ID: "delegate", Agent: child, MaxSteps: 3})
	if err != nil {
		t.Fatalf("new subagent: %v", err)
	}

	output, err := sub.Execute(context.Background(), json.RawMessage(`{"task":"loop forever"}`))
	if err == nil {
		t.Fatalf("expected delegation failure, got output %s", output)
	}
	if !errors.Is(err, ErrSubagentRunFailed) {
		t.Fatalf("error = %v, want ErrSubagentRunFailed", err)
	}
	// The child stopped at the subagent's bound, not its own configured 50.
	if childModel.calls != 3 {
		t.Fatalf("child model calls = %d, want 3", childModel.calls)
	}
	// The child's own agent configuration is untouched, so a later run through
	// a different path still gets the full budget.
	if child.maxSteps != 50 {
		t.Fatalf("child maxSteps mutated to %d, want 50", child.maxSteps)
	}
	var subErr *SubagentError
	if !errors.As(err, &subErr) {
		t.Fatalf("error %v is not a *SubagentError", err)
	}
	if !errors.Is(subErr.Err, ErrAgentStepLimitExhausted) {
		t.Fatalf("wrapped error = %v, want ErrAgentStepLimitExhausted", subErr.Err)
	}
}

func TestSubagentDeadlineDoesNotCancelParent(t *testing.T) {
	child := newChildAgent(t, blockingModel{}, "blocker")
	sub, err := NewSubagent(SubagentConfig{ID: "slow", Agent: child, Deadline: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("new subagent: %v", err)
	}

	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err = sub.Execute(parent, json.RawMessage(`{"task":"take too long"}`))
	if err == nil {
		t.Fatal("expected delegation failure")
	}
	if !errors.Is(err, ErrSubagentCancelled) {
		t.Fatalf("error = %v, want ErrSubagentCancelled", err)
	}
	// The parent context survived the child's deadline.
	if parentErr := parent.Err(); parentErr != nil {
		t.Fatalf("parent context ended with %v, want nil", parentErr)
	}
}

func TestSubagentIsolatesThreadContextByDefault(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	now := time.Unix(0, 0).UTC()

	const threadID ThreadID = "shared-thread"
	if err := store.Threads().CreateThread(ctx, ThreadRecord{ID: threadID, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := store.Messages().AppendMessages(ctx, []MessageRecord{{
		ID:        "prior-1",
		ThreadID:  threadID,
		Message:   Message{Role: RoleUser, Content: "parent-only secret"},
		CreatedAt: now,
	}}); err != nil {
		t.Fatalf("append prior message: %v", err)
	}

	newChild := func(model Model) *Agent {
		t.Helper()
		agent, err := NewAgent(AgentConfig{
			Definition: AgentDefinition{ID: "child"},
			Model:      model,
			Store:      store,
			Clock:      NewFixedClock(now),
		})
		if err != nil {
			t.Fatalf("new child: %v", err)
		}
		return agent
	}

	t.Run("isolated by default", func(t *testing.T) {
		model := &delegationEchoModel{reply: "ok"}
		sub, err := NewSubagent(SubagentConfig{ID: "isolated", Agent: newChild(model)})
		if err != nil {
			t.Fatalf("new subagent: %v", err)
		}
		parentCtx := withWorkflowInvocation(ctx, "parent-run", 1, "parent-step", threadID, map[string]string{"tenant": "acme"})
		if _, err := sub.Execute(parentCtx, json.RawMessage(`{"task":"do the work"}`)); err != nil {
			t.Fatalf("delegate: %v", err)
		}
		if len(model.requests) != 1 {
			t.Fatalf("child calls = %d, want 1", len(model.requests))
		}
		for _, message := range model.requests[0].Messages {
			if strings.Contains(message.Content, "parent-only secret") {
				t.Fatal("child transcript leaked parent thread context")
			}
		}
		if got := len(model.requests[0].Messages); got != 1 {
			t.Fatalf("child messages = %d, want 1 (task only)", got)
		}
	})

	t.Run("shared when configured", func(t *testing.T) {
		model := &delegationEchoModel{reply: "ok"}
		sub, err := NewSubagent(SubagentConfig{ID: "shared", Agent: newChild(model), ShareThread: true})
		if err != nil {
			t.Fatalf("new subagent: %v", err)
		}
		parentCtx := withWorkflowInvocation(ctx, "parent-run", 1, "parent-step", threadID, nil)
		if _, err := sub.Execute(parentCtx, json.RawMessage(`{"task":"do the work"}`)); err != nil {
			t.Fatalf("delegate: %v", err)
		}
		if len(model.requests) != 1 {
			t.Fatalf("child calls = %d, want 1", len(model.requests))
		}
		var sawPrior bool
		for _, message := range model.requests[0].Messages {
			if strings.Contains(message.Content, "parent-only secret") {
				sawPrior = true
			}
		}
		if !sawPrior {
			t.Fatal("child did not receive shared thread context")
		}
	})
}

func TestSubagentSharesMetadataOnlyWhenConfigured(t *testing.T) {
	parentMetadata := map[string]string{"tenant": "acme"}

	run := func(share bool) map[string]string {
		t.Helper()
		model := &metadataProbeModel{}
		child, err := NewAgent(AgentConfig{
			Definition: AgentDefinition{ID: "child"},
			Model:      model,
		})
		if err != nil {
			t.Fatalf("new child: %v", err)
		}
		sub, err := NewSubagent(SubagentConfig{ID: "probe", Agent: child, ShareMetadata: share})
		if err != nil {
			t.Fatalf("new subagent: %v", err)
		}
		ctx := withWorkflowInvocation(context.Background(), "parent-run", 1, "parent-step", "", parentMetadata)
		if _, err := sub.Execute(ctx, json.RawMessage(`{"task":"probe"}`)); err != nil {
			t.Fatalf("delegate: %v", err)
		}
		return model.metadata
	}

	if got := run(false); got["tenant"] != "" {
		t.Fatalf("isolated delegation leaked metadata %v", got)
	}
	if got := run(true); got["tenant"] != "acme" {
		t.Fatalf("shared delegation metadata = %v, want tenant=acme", got)
	}
}

// metadataProbeModel records the metadata visible to the child run.
type metadataProbeModel struct {
	metadata map[string]string
}

func (m *metadataProbeModel) Generate(ctx context.Context, _ ModelRequest) (ModelResponse, error) {
	m.metadata = workflowInvocationFromContext(ctx).metadata
	return ModelResponse{
		Message:      Message{Role: RoleAssistant, Content: "ok"},
		FinishReason: FinishReasonStop,
	}, nil
}

func TestSubagentCorrelatesParentAndChildEvents(t *testing.T) {
	childRecorder := NewRunRecorder()
	childModel := &delegationEchoModel{reply: "child done"}
	child, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "worker"},
		Model:      childModel,
		Listener:   childRecorder,
	})
	if err != nil {
		t.Fatalf("new child: %v", err)
	}

	sub, err := NewSubagent(SubagentConfig{ID: "worker.delegate", Agent: child})
	if err != nil {
		t.Fatalf("new subagent: %v", err)
	}

	parentRecorder := NewRunRecorder()
	supervisorModel := &delegationModel{toolID: "worker.delegate", task: "do it", finalTxt: "all done"}
	supervisor := newSupervisor(t, supervisorModel, sub, parentRecorder)

	if _, err := supervisor.Run(context.Background(), RunInput{
		Messages: []Message{{Role: RoleUser, Content: "start"}},
	}); err != nil {
		t.Fatalf("supervisor run: %v", err)
	}

	parentEvents := parentRecorder.Events()
	if len(parentEvents) == 0 {
		t.Fatal("no parent events recorded")
	}
	parentRunID := parentEvents[0].RunID
	if parentRunID == "" {
		t.Fatal("parent run ID is empty")
	}

	// Find the parent's tool_started event so its step and step ID can be
	// matched against what the child stamped.
	var toolStarted RunEvent
	var found bool
	for _, event := range parentEvents {
		if event.Type == RunEventToolStarted {
			toolStarted = event
			found = true
			break
		}
	}
	if !found {
		t.Fatal("parent recorded no tool_started event")
	}

	childEvents := childRecorder.Events()
	if len(childEvents) == 0 {
		t.Fatal("no child events recorded")
	}
	for _, event := range childEvents {
		if event.ParentRunID != parentRunID {
			t.Fatalf("child event %q ParentRunID = %q, want %q", event.Type, event.ParentRunID, parentRunID)
		}
		if event.ParentStepID != toolStarted.StepID {
			t.Fatalf("child event %q ParentStepID = %q, want %q", event.Type, event.ParentStepID, toolStarted.StepID)
		}
		if event.ParentStep != toolStarted.Step {
			t.Fatalf("child event %q ParentStep = %d, want %d", event.Type, event.ParentStep, toolStarted.Step)
		}
		// The child's own run is distinct from the parent's.
		if event.RunID == parentRunID {
			t.Fatalf("child event %q reuses the parent run ID %q", event.Type, parentRunID)
		}
	}

	// Parent events are not stamped with a parent of their own.
	for _, event := range parentEvents {
		if event.ParentRunID != "" {
			t.Fatalf("top-level parent event %q carries ParentRunID %q", event.Type, event.ParentRunID)
		}
	}
}

func TestSubagentRejectsInvalidInput(t *testing.T) {
	sub, err := NewSubagent(SubagentConfig{
		ID:    "delegate",
		Agent: newChildAgent(t, &delegationEchoModel{reply: "ok"}, "child"),
	})
	if err != nil {
		t.Fatalf("new subagent: %v", err)
	}

	tests := []struct {
		name  string
		input string
	}{
		{name: "empty task", input: `{"task":""}`},
		{name: "missing task", input: `{}`},
		{name: "malformed json", input: `{"task":`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := sub.Execute(context.Background(), json.RawMessage(test.input)); !errors.Is(err, ErrSubagentInvalidInput) {
				t.Fatalf("error = %v, want ErrSubagentInvalidInput", err)
			}
		})
	}
}

func TestSubagentSchemaBoundaryRejectsBadArgumentsBeforeChildRuns(t *testing.T) {
	childModel := &delegationEchoModel{reply: "should not run"}
	sub, err := NewSubagent(SubagentConfig{ID: "delegate", Agent: newChildAgent(t, childModel, "child")})
	if err != nil {
		t.Fatalf("new subagent: %v", err)
	}
	registry, err := NewToolRegistry(delegationSchemaCompiler{})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	if err := registry.Register(sub); err != nil {
		t.Fatalf("register subagent: %v", err)
	}

	// The registry validates against the delegation schema, which requires a
	// task, before Execute is reached.
	result := registry.Execute(context.Background(), "delegate", ToolExecutionRequest{
		Arguments: json.RawMessage(`{"unexpected":"value"}`),
	})
	if result.State != ToolExecutionInvalidInput {
		t.Fatalf("state = %q, want %q", result.State, ToolExecutionInvalidInput)
	}
	if len(childModel.requests) != 0 {
		t.Fatalf("child ran %d times despite invalid input", len(childModel.requests))
	}
}

func TestSubagentCancellationPropagates(t *testing.T) {
	sub, err := NewSubagent(SubagentConfig{
		ID:    "delegate",
		Agent: newChildAgent(t, blockingModel{}, "blocker"),
	})
	if err != nil {
		t.Fatalf("new subagent: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	_, err = sub.Execute(ctx, json.RawMessage(`{"task":"block"}`))
	// A cancelled parent surfaces the bare context error so the registry
	// boundary classifies the delegation as cancelled, not as a handler error.
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestSubagentDelegationPromptIncludesContext(t *testing.T) {
	model := &delegationEchoModel{reply: "ok"}
	sub, err := NewSubagent(SubagentConfig{ID: "delegate", Agent: newChildAgent(t, model, "child")})
	if err != nil {
		t.Fatalf("new subagent: %v", err)
	}
	if _, err := sub.Execute(context.Background(), json.RawMessage(`{"task":"summarize","context":"be brief"}`)); err != nil {
		t.Fatalf("delegate: %v", err)
	}
	got := model.requests[0].Messages[0].Content
	if !strings.Contains(got, "summarize") || !strings.Contains(got, "be brief") {
		t.Fatalf("prompt = %q, want task and context", got)
	}
}

func TestSubagentPassesThroughStructuredOutput(t *testing.T) {
	model := &structuredModel{payload: `{"finding":"result"}`}
	sub, err := NewSubagent(SubagentConfig{ID: "delegate", Agent: newChildAgent(t, model, "child")})
	if err != nil {
		t.Fatalf("new subagent: %v", err)
	}
	output, err := sub.Execute(context.Background(), json.RawMessage(`{"task":"analyze"}`))
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	var decoded subagentOutput
	if err := json.Unmarshal(output, &decoded); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if decoded.Output != `{"finding":"result"}` {
		t.Fatalf("output = %q, want the structured payload", decoded.Output)
	}
}

// structuredModel answers with a structured payload rather than text.
type structuredModel struct{ payload string }

func (m *structuredModel) Generate(context.Context, ModelRequest) (ModelResponse, error) {
	return ModelResponse{
		Message: Message{
			Role:             RoleAssistant,
			StructuredOutput: NewModelStructuredOutput(json.RawMessage(m.payload)),
		},
		FinishReason: FinishReasonStop,
	}, nil
}

func TestSubagentDefinitionDefaults(t *testing.T) {
	child := newChildAgent(t, &delegationEchoModel{reply: "ok"}, "researcher")
	sub, err := NewSubagent(SubagentConfig{ID: "research", Agent: child})
	if err != nil {
		t.Fatalf("new subagent: %v", err)
	}
	definition := sub.Definition()
	if definition.ID != "research" {
		t.Fatalf("ID = %q, want %q", definition.ID, "research")
	}
	if !json.Valid(definition.InputSchema) || !json.Valid(definition.OutputSchema) {
		t.Fatal("default schemas are not valid JSON")
	}
	// Definition returns a caller-owned copy.
	definition.InputSchema[0] = 'X'
	if sub.Definition().InputSchema[0] == 'X' {
		t.Fatal("Definition returned a shared schema slice")
	}
}

func TestNewSubagentValidation(t *testing.T) {
	child := newChildAgent(t, &delegationEchoModel{reply: "ok"}, "child")

	tests := []struct {
		name   string
		config SubagentConfig
	}{
		{name: "missing ID", config: SubagentConfig{Agent: child}},
		{name: "missing agent", config: SubagentConfig{ID: "delegate"}},
		{name: "nil agent interface", config: SubagentConfig{ID: "delegate", Agent: (*Agent)(nil)}},
		{name: "negative max steps", config: SubagentConfig{ID: "delegate", Agent: child, MaxSteps: -1}},
		{name: "negative deadline", config: SubagentConfig{ID: "delegate", Agent: child, Deadline: -time.Second}},
		{name: "invalid input schema", config: SubagentConfig{ID: "delegate", Agent: child, InputSchema: json.RawMessage(`{`)}},
		{name: "invalid output schema", config: SubagentConfig{ID: "delegate", Agent: child, OutputSchema: json.RawMessage(`{`)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewSubagent(test.config); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}
