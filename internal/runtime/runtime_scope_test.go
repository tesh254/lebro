package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestMemoryStoreFiltersWorkflowAndScheduleScopes(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now().UTC()
	for _, namespace := range []string{"org-a", "org-b"} {
		if err := store.WorkflowRuns().SaveWorkflowRun(ctx, WorkflowRunRecord{ID: RunID("run-" + namespace), WorkflowID: "flow", Namespace: namespace, OwnerID: "user", Status: RunStatusSucceeded, Input: json.RawMessage(`{}`), StartedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := store.Schedules().SaveSchedule(ctx, ScheduleRecord{ID: ScheduleID("schedule-" + namespace), WorkflowID: "flow", Namespace: namespace, OwnerID: "user", Spec: "@every 1h", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := store.WorkflowRuns().ListWorkflowRuns(ctx, WorkflowRunFilter{Namespace: "org-a"}, PageRequest{})
	if err != nil || len(runs.Records) != 1 || runs.Records[0].Namespace != "org-a" {
		t.Fatalf("workflow scope filter = %#v, %v", runs, err)
	}
	schedules, err := store.Schedules().ListSchedules(ctx, ScheduleFilter{Namespace: "org-b"}, PageRequest{})
	if err != nil || len(schedules.Records) != 1 || schedules.Records[0].Namespace != "org-b" {
		t.Fatalf("schedule scope filter = %#v, %v", schedules, err)
	}
}

func TestPolicyStoreRejectsClaimedScopeOutsideVerifiedScope(t *testing.T) {
	store, err := NewPolicyStore(NewMemoryStore(), AllowAllPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithRuntimeScope(context.Background(), RuntimeScope{Namespace: "org-a", OwnerID: "user-a"})
	err = store.Schedules().SaveSchedule(ctx, ScheduleRecord{ID: "schedule", WorkflowID: "flow", Namespace: "org-b", OwnerID: "user-a", Spec: "@every 1h"})
	if err == nil || errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("SaveSchedule scope mismatch = %v, want scope validation error", err)
	}
}

func TestWorkflowSuppliedRunIDCannotReplaceStoredRun(t *testing.T) {
	store := NewMemoryStore()
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{Definition: WorkflowDefinition{ID: "flow"}, Store: store, Steps: []Step{{Definition: StepDefinition{ID: "one"}, Handler: StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) { return input, nil })}}})
	if err != nil {
		t.Fatal(err)
	}
	input := WorkflowRunInput{RunID: "control_plane_run", Input: json.RawMessage(`{}`)}
	if _, err := wf.Run(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if _, err := wf.Run(context.Background(), input); !errors.Is(err, ErrRunIDAlreadyExists) {
		t.Fatalf("second Run() error = %v, want ErrRunIDAlreadyExists", err)
	}
}

type cancelDuringWorkflowRunLookup struct {
	WorkflowRunRepository
	cancel context.CancelFunc
}

func (r cancelDuringWorkflowRunLookup) GetWorkflowRun(context.Context, RunID) (WorkflowRunRecord, error) {
	r.cancel()
	return WorkflowRunRecord{}, errors.New("lookup interrupted")
}

type cancelDuringWorkflowRunLookupStore struct {
	Store
	runs WorkflowRunRepository
}

func (s cancelDuringWorkflowRunLookupStore) WorkflowRuns() WorkflowRunRepository { return s.runs }

func TestWorkflowSuppliedRunIDCancellationDuringExistenceCheck(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := NewMemoryStore()
	store := cancelDuringWorkflowRunLookupStore{Store: base, runs: cancelDuringWorkflowRunLookup{WorkflowRunRepository: base.WorkflowRuns(), cancel: cancel}}
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{Definition: WorkflowDefinition{ID: "flow"}, Store: store, Steps: []Step{{Definition: StepDefinition{ID: "one"}, Handler: StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) { return input, nil })}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = wf.Run(ctx, WorkflowRunInput{RunID: "control_plane_run", Input: json.RawMessage(`{}`)})
	var workflowErr *WorkflowError
	if !errors.As(err, &workflowErr) || workflowErr.Kind != WorkflowErrorCancelled {
		t.Fatalf("Run() error = %#v, want WorkflowErrorCancelled", err)
	}
}

func TestRunStreamExposesStableRunIDImmediately(t *testing.T) {
	agent, err := NewAgent(AgentConfig{Definition: AgentDefinition{ID: "agent"}, Model: echoModel{}})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := agent.RunStream(context.Background(), RunInput{RunID: "control_run_1", Messages: []Message{{Role: RoleUser, Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	if stream.RunID != "control_run_1" {
		t.Fatalf("immediate RunID = %q", stream.RunID)
	}
	result, err := stream.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != stream.RunID {
		t.Fatalf("result ID = %q, stream ID = %q", result.ID, stream.RunID)
	}
}

func TestRuntimeStoreConfigurationsAreMutuallyExclusive(t *testing.T) {
	store := NewMemoryStore()
	_, err := NewLinearWorkflow(LinearWorkflowConfig{Definition: WorkflowDefinition{ID: "flow"}, Store: store, RuntimeStore: store, Steps: []Step{{Definition: StepDefinition{ID: "one"}, Handler: StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) { return input, nil })}}})
	if err == nil {
		t.Fatal("workflow accepted Store and RuntimeStore")
	}
	_, err = NewScheduler(SchedulerConfig{Store: store, RuntimeStore: store, Resolver: WorkflowMap{}})
	if err == nil {
		t.Fatal("scheduler accepted Store and RuntimeStore")
	}
}
