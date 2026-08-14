package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type routerWorkflow struct {
	id     WorkflowID
	output string
	err    error
}

func (w routerWorkflow) Definition() WorkflowDefinition {
	return WorkflowDefinition{ID: w.id, Description: string(w.id)}
}
func (w routerWorkflow) Run(_ context.Context, _ RunInput) (RunResult, error) {
	if w.err != nil {
		return RunResult{ID: RunID(w.id + "-run"), Status: RunStatusFailed}, w.err
	}
	return RunResult{ID: RunID(w.id + "-run"), Status: RunStatusSucceeded, Messages: []Message{{Role: RoleAssistant, Content: w.output}}}, nil
}

type routeModel struct {
	response ModelResponse
	err      error
}

func (m routeModel) Generate(_ context.Context, _ ModelRequest) (ModelResponse, error) {
	return m.response, m.err
}

func newRouterSpecialist(t *testing.T, id ToolID, target Workflow) *Subagent {
	t.Helper()
	specialist, err := NewSubagent(SubagentConfig{ID: id, Agent: target})
	if err != nil {
		t.Fatalf("NewSubagent() error = %v", err)
	}
	return specialist
}

func TestRoutedSubagentFallsBackAfterSpecialistFailure(t *testing.T) {
	primary := newRouterSpecialist(t, "primary", routerWorkflow{id: "primary-agent", err: &ModelError{Kind: ModelErrorUnavailable}})
	fallback := newRouterSpecialist(t, "fallback", routerWorkflow{id: "fallback-agent", output: "handled"})
	router, err := NewRuleRouter(nil, "primary")
	if err != nil {
		t.Fatal(err)
	}
	routed, err := NewRoutedSubagent(RoutedSubagentConfig{ID: "delegate", Router: router, Specialists: []*Subagent{primary, fallback}, Fallback: []ToolID{"fallback"}})
	if err != nil {
		t.Fatal(err)
	}

	output, err := routed.Execute(context.Background(), json.RawMessage(`{"task":"work"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var result routedSubagentOutput
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal(err)
	}
	if result.AgentID != "fallback-agent" || result.Output != "handled" {
		t.Fatalf("result = %#v", result)
	}
	if result.Route.Selected != "primary" || len(result.Route.Attempted) != 2 || result.Route.Attempted[1] != "fallback" || len(result.Route.ChildRunIDs) != 2 || result.Route.ChildRunIDs[0] != "primary-agent-run" || result.Route.ChildRunIDs[1] != "fallback-agent-run" {
		t.Fatalf("route = %#v", result.Route)
	}
}

func TestRoutedSubagentDoesNotFallbackAfterApprovalRejection(t *testing.T) {
	primary := newRouterSpecialist(t, "primary", routerWorkflow{id: "primary-agent", err: ErrApprovalRejected})
	fallback := newRouterSpecialist(t, "fallback", routerWorkflow{id: "fallback-agent", output: "must not run"})
	router, err := NewRuleRouter(nil, "primary")
	if err != nil {
		t.Fatal(err)
	}
	routed, err := NewRoutedSubagent(RoutedSubagentConfig{ID: "delegate", Router: router, Specialists: []*Subagent{primary, fallback}, Fallback: []ToolID{"fallback"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = routed.Execute(context.Background(), json.RawMessage(`{"task":"approve"}`))
	if !errors.Is(err, ErrApprovalRejected) {
		t.Fatalf("error = %v, want ErrApprovalRejected", err)
	}
	var routeErr *RouteError
	if !errors.As(err, &routeErr) || len(routeErr.Attempted) != 1 || routeErr.Attempted[0] != "primary" {
		t.Fatalf("route error = %#v", routeErr)
	}
}

func TestRuleRouterUsesFirstMatchThenDefault(t *testing.T) {
	router, err := NewRuleRouter([]RouteRule{
		{SpecialistID: "research", Match: func(request RoutingRequest) bool { return request.Task == "research" }},
		{SpecialistID: "writer", Match: func(RoutingRequest) bool { return true }},
	}, "fallback")
	if err != nil {
		t.Fatal(err)
	}
	decision, err := router.Route(context.Background(), RoutingRequest{Task: "research"})
	if err != nil || decision.SpecialistID != "research" {
		t.Fatalf("matching decision = %#v, %v", decision, err)
	}
	decision, err = router.Route(context.Background(), RoutingRequest{Task: "other"})
	if err != nil || decision.SpecialistID != "writer" {
		t.Fatalf("fallback decision = %#v, %v", decision, err)
	}
}

func TestModelSpecialistRouterRejectsUnknownCandidate(t *testing.T) {
	router, err := NewModelSpecialistRouter(ModelSpecialistRouterConfig{Model: routeModel{response: ModelResponse{Message: Message{Role: RoleAssistant, Content: `{"specialist_id":"unknown"}`}, FinishReason: FinishReasonStop}}, ModelName: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = router.Route(context.Background(), RoutingRequest{Task: "work", Candidates: []RoutingCandidate{{ID: "known"}}})
	if err == nil || !strings.Contains(err.Error(), "not a candidate") {
		t.Fatalf("error = %v", err)
	}
}

func TestModelSpecialistRouterSelectsStructuredCandidate(t *testing.T) {
	router, err := NewModelSpecialistRouter(ModelSpecialistRouterConfig{Model: routeModel{response: ModelResponse{Message: Message{Role: RoleAssistant, StructuredOutput: NewModelStructuredOutput(json.RawMessage(`{"specialist_id":"known"}`))}, FinishReason: FinishReasonStop}}, ModelName: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := router.Route(context.Background(), RoutingRequest{Task: "work", Candidates: []RoutingCandidate{{ID: "known"}}})
	if err != nil || decision.SpecialistID != "known" {
		t.Fatalf("decision = %#v, error = %v", decision, err)
	}
}

func TestRoutedSubagentReportsExhaustedFallbacks(t *testing.T) {
	primary := newRouterSpecialist(t, "primary", routerWorkflow{id: "primary-agent", err: &ModelError{Kind: ModelErrorUnavailable}})
	fallback := newRouterSpecialist(t, "fallback", routerWorkflow{id: "fallback-agent", err: &ModelError{Kind: ModelErrorUnavailable}})
	router, err := NewRuleRouter(nil, "primary")
	if err != nil {
		t.Fatal(err)
	}
	routed, err := NewRoutedSubagent(RoutedSubagentConfig{ID: "delegate", Router: router, Specialists: []*Subagent{primary, fallback}, Fallback: []ToolID{"fallback"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = routed.Execute(context.Background(), json.RawMessage(`{"task":"work"}`))
	var routeErr *RouteError
	if !errors.As(err, &routeErr) || routeErr.Kind != RouteErrorExhausted || len(routeErr.Attempted) != 2 || routeErr.Attempted[0] != "primary" || routeErr.Attempted[1] != "fallback" {
		t.Fatalf("route error = %#v", routeErr)
	}
}

func TestModelSpecialistRouterHonorsCancellationAndGenerateFailure(t *testing.T) {
	router, err := NewModelSpecialistRouter(ModelSpecialistRouterConfig{Model: routeModel{}, ModelName: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = router.Route(ctx, RoutingRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
	router, err = NewModelSpecialistRouter(ModelSpecialistRouterConfig{Model: routeModel{err: errors.New("provider down")}, ModelName: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = router.Route(context.Background(), RoutingRequest{})
	if err == nil || !strings.Contains(err.Error(), "select specialist") {
		t.Fatalf("generate error = %v", err)
	}
}
