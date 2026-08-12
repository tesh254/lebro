package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestAgentRunDeniedByPolicy(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(textResponse("should never run"))
	recorder := NewRunRecorder()
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "weather", Model: "fixture-model"},
		Model:      model,
		Listener:   recorder,
		Policy:     &staticPolicy{allow: false, reason: "tenant blocked"},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := WithIdentity(context.Background(), Identity{Subject: "user-1", Tenant: "acme"})
	result, runErr := agent.Run(ctx, RunInput{Messages: []Message{{Role: RoleUser, Content: "hi"}}})

	if runErr == nil {
		t.Fatal("expected run to be denied")
	}
	if !errors.Is(runErr, ErrAgentUnauthorized) || !errors.Is(runErr, ErrPolicyDenied) {
		t.Fatalf("denial must match ErrAgentUnauthorized and ErrPolicyDenied: %v", runErr)
	}
	var agentErr *AgentError
	if !errors.As(runErr, &agentErr) || agentErr.Kind != AgentErrorUnauthorized {
		t.Fatalf("run error kind = %v, want unauthorized", runErr)
	}
	if result.Status != RunStatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	// The model must never be called for a denied run.
	if len(model.calls) != 0 {
		t.Fatalf("model was called %d times on a denied run", len(model.calls))
	}
	// The denial is auditable: a terminal failed event carries the typed error.
	terminal, ok := recorder.TerminalEvent()
	if !ok || terminal.Type != RunEventFailed {
		t.Fatalf("expected terminal run_failed event, got %+v", terminal)
	}
	if !errors.Is(terminal.Error, ErrPolicyDenied) {
		t.Fatalf("terminal event lost the denial: %v", terminal.Error)
	}
}

func TestAgentRunAllowedByPolicyUnchanged(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(textResponse("hello"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "weather", Model: "fixture-model"},
		Model:      model,
		Policy:     AllowAllPolicy{},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, runErr := agent.Run(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if runErr != nil {
		t.Fatalf("allow-all run error = %v", runErr)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
}

func TestAgentToolCallDeniedByPolicy(t *testing.T) {
	t.Parallel()

	registry, handler := newAgentTestRegistry(t)
	handlerCalled := false
	handler.execute = func(context.Context, json.RawMessage) (json.RawMessage, error) {
		handlerCalled = true
		return json.RawMessage(`{}`), nil
	}

	model := newScriptedModel(toolCallResponse(ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{}`)}))
	recorder := NewRunRecorder()
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "weather", Model: "fixture-model", Tools: []ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
		Listener:   recorder,
		// Allow the run, deny the tool call. actionPolicy denies exactly one action.
		Policy: &actionPolicy{deny: ActionToolCall},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, runErr := agent.Run(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "lookup"}}})

	if runErr == nil {
		t.Fatal("expected tool-call denial to fail the run")
	}
	if !errors.Is(runErr, ErrAgentUnauthorized) || !errors.Is(runErr, ErrPolicyDenied) {
		t.Fatalf("denial must match ErrAgentUnauthorized and ErrPolicyDenied: %v", runErr)
	}
	if handlerCalled {
		t.Fatal("denied tool handler must not run")
	}
	if result.Status != RunStatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	// The tool result recorded in the transcript is the denial, distinguishable
	// from other tool failures via its state in the tool_finished event.
	var toolFinished *RunEvent
	for i := range recorder.Events() {
		e := recorder.Events()[i]
		if e.Type == RunEventToolFinished {
			toolFinished = &e
			break
		}
	}
	if toolFinished == nil || toolFinished.ToolState != ToolExecutionUnauthorized {
		t.Fatalf("expected tool_finished with unauthorized state, got %+v", toolFinished)
	}
}

// denyAfterFirstPolicy allows the first authorization and denies every one
// after it, so a workflow can start (and suspend) under one caller yet be denied
// on resume.
type denyAfterFirstPolicy struct {
	calls int
}

func (p *denyAfterFirstPolicy) Authorize(_ context.Context, _ Identity, _ Action, _ Resource) Decision {
	p.calls++
	if p.calls == 1 {
		return Allow()
	}
	return Deny("resumed under a denied identity")
}

func TestWorkflowResumeDeniedByPolicy(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	suspendHandler := StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
		return nil, &SuspendError{Signal: SuspendSignal{
			StepID:   "await",
			Contract: json.RawMessage(`{"approved":true}`),
		}}
	})
	policy := &denyAfterFirstPolicy{}
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition:     WorkflowDefinition{ID: "resume-wf"},
		SchemaCompiler: contractCompiler(),
		Store:          store,
		Policy:         policy,
		Steps: []Step{
			{Definition: StepDefinition{ID: "await", SuspendSchema: json.RawMessage(`{"const":{"approved":true}}`)}, Handler: suspendHandler},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	suspended, err := wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`"start"`)})
	if err != nil {
		t.Fatalf("initial run returned error: %v", err)
	}
	if suspended.Status != RunStatusSuspended {
		t.Fatalf("status = %q, want suspended", suspended.Status)
	}

	_, resumeErr := wf.Resume(ctx, WorkflowResumeInput{RunID: suspended.ID, Input: json.RawMessage(`{"approved":true}`)})
	if resumeErr == nil {
		t.Fatal("expected resume to be denied")
	}
	if !errors.Is(resumeErr, ErrWorkflowUnauthorized) || !errors.Is(resumeErr, ErrPolicyDenied) {
		t.Fatalf("resume denial must match ErrWorkflowUnauthorized and ErrPolicyDenied: %v", resumeErr)
	}
}

func TestWorkflowRunDeniedByPolicy(t *testing.T) {
	t.Parallel()

	echoHandler := StepHandlerFunc(func(_ context.Context, in json.RawMessage) (json.RawMessage, error) {
		return in, nil
	})
	recorder := NewRunRecorder()
	workflow, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "flow"},
		Steps:      []Step{{Definition: StepDefinition{ID: "only"}, Handler: echoHandler}},
		Listener:   recorder,
		Policy:     &staticPolicy{allow: false, reason: "no workflow access"},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := WithIdentity(context.Background(), Identity{Subject: "user-1"})
	result, runErr := workflow.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`{}`)})

	if runErr == nil {
		t.Fatal("expected workflow run to be denied")
	}
	if !errors.Is(runErr, ErrWorkflowUnauthorized) || !errors.Is(runErr, ErrPolicyDenied) {
		t.Fatalf("denial must match ErrWorkflowUnauthorized and ErrPolicyDenied: %v", runErr)
	}
	if result.Status != RunStatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	terminal, ok := recorder.TerminalEvent()
	if !ok || terminal.Type != RunEventFailed {
		t.Fatalf("expected terminal run_failed event, got %+v", terminal)
	}
	if !errors.Is(terminal.Error, ErrPolicyDenied) {
		t.Fatalf("terminal event lost the denial: %v", terminal.Error)
	}
}
