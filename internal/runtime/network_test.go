package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

type networkRouterFunc func(context.Context, RoutingRequest) (RoutingDecision, error)

func (f networkRouterFunc) Route(ctx context.Context, request RoutingRequest) (RoutingDecision, error) {
	return f(ctx, request)
}

type workflowFunc func(context.Context, RunInput) (RunResult, error)

func (f workflowFunc) Definition() WorkflowDefinition { return WorkflowDefinition{ID: "workflow-func"} }
func (f workflowFunc) Run(ctx context.Context, input RunInput) (RunResult, error) {
	return f(ctx, input)
}

type failingNetworkStore struct {
	Store
	err error
}

func (s failingNetworkStore) WorkflowRuns() WorkflowRunRepository {
	return failingWorkflowRunRepository{WorkflowRunRepository: s.Store.WorkflowRuns(), err: s.err}
}

type failingWorkflowRunRepository struct {
	WorkflowRunRepository
	err error
}

func (r failingWorkflowRunRepository) SaveWorkflowRun(context.Context, WorkflowRunRecord) error {
	return r.err
}

type sequenceRouteModel struct {
	mu        sync.Mutex
	responses []ModelResponse
}

func (m *sequenceRouteModel) Generate(_ context.Context, _ ModelRequest) (ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.responses) == 0 {
		return ModelResponse{}, errors.New("no route response configured")
	}
	response := m.responses[0]
	m.responses = m.responses[1:]
	return response, nil
}

func TestNetworkRunsBoundedRouteAndPersistsRecords(t *testing.T) {
	store := NewMemoryStore()
	recorder := NewRunRecorder()
	network, err := NewNetwork(NetworkConfig{
		Definition: WorkflowDefinition{ID: "support-network", Version: "1"}, Store: store, Listener: recorder,
		Router: networkRouterFunc(func(_ context.Context, request RoutingRequest) (RoutingDecision, error) {
			switch request.Hops {
			case 0:
				return RoutingDecision{SpecialistID: "research"}, nil
			case 1:
				return RoutingDecision{SpecialistID: "writer"}, nil
			default:
				return RoutingDecision{Complete: true}, nil
			}
		}),
		Specialists: []NetworkSpecialist{{ID: "research", Workflow: routerWorkflow{id: "research-agent", output: "facts"}}, {ID: "writer", Workflow: routerWorkflow{id: "writer-agent", output: "answer"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := network.Run(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "help"}}, Metadata: map[string]string{"request": "r1"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunStatusSucceeded || networkOutput(result) != "answer" {
		t.Fatalf("result = %#v", result)
	}
	record, err := store.WorkflowRuns().GetWorkflowRun(context.Background(), result.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.WorkflowID != "support-network" || len(record.StepOutputs) != 2 || record.Status != RunStatusSucceeded {
		t.Fatalf("record = %#v", record)
	}
	var first NetworkRouteRecord
	if err := json.Unmarshal(record.StepOutputs[0], &first); err != nil {
		t.Fatal(err)
	}
	if first.Selected != "research" || first.ChildRunID != "research-agent-run" || len(first.Candidates) != 2 {
		t.Fatalf("route = %#v", first)
	}
	var routeEvents int
	for _, event := range recorder.Events() {
		if event.Type == RunEventRouteSelected {
			routeEvents++
		}
	}
	if routeEvents != 3 {
		t.Fatalf("route events = %d, want 3 including completion", routeEvents)
	}
}

func TestNetworkBuildsExplicitHandoff(t *testing.T) {
	var got RunInput
	second := routerWorkflow{id: "second", output: "done"}
	first := routerWorkflow{id: "first", output: "facts"}
	capturingSecond := workflowFunc(func(_ context.Context, input RunInput) (RunResult, error) {
		got = input
		return second.Run(context.Background(), input)
	})
	network, err := NewNetwork(NetworkConfig{Definition: WorkflowDefinition{ID: "handoff"}, Router: networkRouterFunc(func(_ context.Context, request RoutingRequest) (RoutingDecision, error) {
		if request.Hops == 0 {
			return RoutingDecision{SpecialistID: "first"}, nil
		}
		if request.Hops == 1 {
			return RoutingDecision{SpecialistID: "second"}, nil
		}
		return RoutingDecision{Complete: true}, nil
	}), Specialists: []NetworkSpecialist{{ID: "first", Workflow: first}, {ID: "second", Workflow: capturingSecond}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := network.Run(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "research topic"}}}); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != RoleUser || got.Messages[0].Content != "research topic\n\nPrevious specialist output:\nfacts" {
		t.Fatalf("handoff = %#v", got.Messages)
	}
}

func TestNetworkExcludesVisitedSpecialistsFromCandidates(t *testing.T) {
	var secondHop []RoutingCandidate
	network, err := NewNetwork(NetworkConfig{Definition: WorkflowDefinition{ID: "candidates"}, Router: networkRouterFunc(func(_ context.Context, request RoutingRequest) (RoutingDecision, error) {
		switch request.Hops {
		case 0:
			return RoutingDecision{SpecialistID: "first"}, nil
		case 1:
			secondHop = append([]RoutingCandidate(nil), request.Candidates...)
			return RoutingDecision{SpecialistID: "second"}, nil
		default:
			return RoutingDecision{Complete: true}, nil
		}
	}), Specialists: []NetworkSpecialist{{ID: "first", Workflow: routerWorkflow{id: "first", output: "facts"}}, {ID: "second", Workflow: routerWorkflow{id: "second", output: "answer"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := network.Run(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "work"}}}); err != nil {
		t.Fatal(err)
	}
	if len(secondHop) != 1 || secondHop[0].ID != "second" {
		t.Fatalf("second-hop candidates = %#v", secondHop)
	}
}

func TestNetworkRejectsCyclesAndCarriesRouteHistory(t *testing.T) {
	network, err := NewNetwork(NetworkConfig{Definition: WorkflowDefinition{ID: "cycle"}, Router: networkRouterFunc(func(context.Context, RoutingRequest) (RoutingDecision, error) {
		return RoutingDecision{SpecialistID: "same"}, nil
	}), Specialists: []NetworkSpecialist{{ID: "same", Workflow: routerWorkflow{id: "same-agent", output: "done"}}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = network.Run(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "work"}}})
	if !errors.Is(err, ErrNetworkCycle) {
		t.Fatalf("error = %v", err)
	}
	var networkErr *NetworkError
	if !errors.As(err, &networkErr) || len(networkErr.Routes) != 1 || networkErr.Routes[0].Selected != "same" {
		t.Fatalf("network error = %#v", networkErr)
	}
}

func TestNetworkStopsAtConfiguredHopLimit(t *testing.T) {
	network, err := NewNetwork(NetworkConfig{Definition: WorkflowDefinition{ID: "limited"}, MaxHops: 1, Router: networkRouterFunc(func(context.Context, RoutingRequest) (RoutingDecision, error) {
		return RoutingDecision{SpecialistID: "one"}, nil
	}), Specialists: []NetworkSpecialist{{ID: "one", Workflow: routerWorkflow{id: "one-agent", output: "done"}}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = network.Run(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "work"}}})
	if !errors.Is(err, ErrNetworkHopLimit) {
		t.Fatalf("error = %v", err)
	}
}

func TestNetworkClassifiesSpecialistFailures(t *testing.T) {
	for name, specialist := range map[string]Workflow{
		"run error": workflowFunc(func(context.Context, RunInput) (RunResult, error) {
			return RunResult{ID: "failed", Status: RunStatusFailed}, errors.New("specialist unavailable")
		}),
		"failed status": workflowFunc(func(context.Context, RunInput) (RunResult, error) {
			return RunResult{ID: "failed", Status: RunStatusFailed}, nil
		}),
		"missing handoff": workflowFunc(func(context.Context, RunInput) (RunResult, error) {
			return RunResult{ID: "empty", Status: RunStatusSucceeded}, nil
		}),
	} {
		t.Run(name, func(t *testing.T) {
			network, err := NewNetwork(NetworkConfig{Definition: WorkflowDefinition{ID: "specialist-failure"}, Router: networkRouterFunc(func(context.Context, RoutingRequest) (RoutingDecision, error) {
				return RoutingDecision{SpecialistID: "one"}, nil
			}), Specialists: []NetworkSpecialist{{ID: "one", Workflow: specialist}}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = network.Run(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "work"}}})
			if !errors.Is(err, ErrNetworkSpecialistFailed) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestNetworkClassifiesPersistFailure(t *testing.T) {
	store := failingNetworkStore{Store: NewMemoryStore(), err: errors.New("storage unavailable")}
	network, err := NewNetwork(NetworkConfig{Definition: WorkflowDefinition{ID: "persist"}, Store: store, Router: networkRouterFunc(func(_ context.Context, request RoutingRequest) (RoutingDecision, error) {
		if request.Hops == 0 {
			return RoutingDecision{SpecialistID: "one"}, nil
		}
		return RoutingDecision{Complete: true}, nil
	}), Specialists: []NetworkSpecialist{{ID: "one", Workflow: routerWorkflow{id: "one", output: "done"}}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = network.Run(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "work"}}})
	if !errors.Is(err, ErrNetworkPersist) || errors.Is(err, ErrNetworkSpecialistFailed) {
		t.Fatalf("error = %v", err)
	}
}

func TestNetworkPersistsCancelledRunWithDetachedContext(t *testing.T) {
	store := NewMemoryStore()
	network, err := NewNetwork(NetworkConfig{Definition: WorkflowDefinition{ID: "cancelled"}, Store: store, Router: networkRouterFunc(func(context.Context, RoutingRequest) (RoutingDecision, error) {
		t.Fatal("router called after cancellation")
		return RoutingDecision{}, nil
	}), Specialists: []NetworkSpecialist{{ID: "one", Workflow: routerWorkflow{id: "one", output: "done"}}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := network.Run(ctx, RunInput{Messages: []Message{{Role: RoleUser, Content: "work"}}})
	if !errors.Is(err, context.Canceled) || result.Status != RunStatusCancelled {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
	record, err := store.WorkflowRuns().GetWorkflowRun(context.Background(), result.ID)
	if err != nil || record.Status != RunStatusCancelled {
		t.Fatalf("record = %#v, error = %v", record, err)
	}
}

func TestNetworkRunsMultiHopModelSpecialistRouter(t *testing.T) {
	model := &sequenceRouteModel{responses: []ModelResponse{
		{Message: Message{Role: RoleAssistant, Content: `{"specialist_id":"first"}`}, FinishReason: FinishReasonStop},
		{Message: Message{Role: RoleAssistant, Content: `{"specialist_id":"second"}`}, FinishReason: FinishReasonStop},
		{Message: Message{Role: RoleAssistant, Content: `{"complete":true}`}, FinishReason: FinishReasonStop},
	}}
	router, err := NewModelSpecialistRouter(ModelSpecialistRouterConfig{Model: model, ModelName: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	network, err := NewNetwork(NetworkConfig{Definition: WorkflowDefinition{ID: "model-route"}, Router: router, Specialists: []NetworkSpecialist{{ID: "first", Workflow: routerWorkflow{id: "first", output: "facts"}}, {ID: "second", Workflow: routerWorkflow{id: "second", output: "answer"}}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := network.Run(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "work"}}})
	if err != nil || result.Status != RunStatusSucceeded || networkOutput(result) != "answer" {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
}

func TestNetworkAuthorizesBeforeRouting(t *testing.T) {
	policy := &actionPolicy{deny: ActionNetworkRun}
	network, err := NewNetwork(NetworkConfig{Definition: WorkflowDefinition{ID: "protected"}, Policy: policy, Router: networkRouterFunc(func(context.Context, RoutingRequest) (RoutingDecision, error) {
		t.Fatal("router called after denial")
		return RoutingDecision{}, nil
	}), Specialists: []NetworkSpecialist{{ID: "one", Workflow: routerWorkflow{id: "one-agent", output: "done"}}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = network.Run(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "work"}}})
	if !errors.Is(err, ErrNetworkUnauthorized) || !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("error = %v", err)
	}
}
