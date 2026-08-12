package lebro_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

// TestApprovalGateWithRealSchemaCompiler exercises the gate end to end with the
// real JSON Schema compiler, not the runtime's permissive test stub. A strict
// decision schema ("type":"object") would reject the empty contract the gate
// publishes at suspend if the gate routed decision validation through the
// executor's SuspendSchema; this test proves the suspend boundary and the resume
// both pass, and that the guard still enforces the decision schema.
func TestApprovalGateWithRealSchemaCompiler(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := lebro.NewMemoryStore()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	compiler := lebrojsonschema.NewCompiler()
	clock := lebro.NewFixedClock(time.Unix(7000, 0))

	var innerCalls int
	inner := lebro.StepHandlerFunc(func(_ context.Context, in json.RawMessage) (json.RawMessage, error) {
		innerCalls++
		return in, nil
	})

	gate, err := lebro.NewApprovalGate("await", "run", inner, lebro.ApprovalRequirement{
		Action:         lebro.ActionToolCall,
		Resource:       lebro.Resource{Kind: lebro.ResourceKindTool, ID: "wire.transfer"},
		TTL:            time.Hour,
		DecisionSchema: json.RawMessage(`{"type":"object","required":["approved","decided_at"]}`),
	}, compiler, store, clock)
	if err != nil {
		t.Fatal(err)
	}

	wf, err := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition:     lebro.WorkflowDefinition{ID: "approval-real", Version: "v1"},
		SchemaCompiler: compiler,
		Store:          store,
		Clock:          clock,
		IDSource:       lebro.NewFixedIDSource([]lebro.RunID{"real-run-1"}, nil),
		Steps:          []lebro.Step{gate.Request, gate.Guard},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Suspends cleanly despite the strict decision schema — the empty suspend
	// contract that would fail a naive SuspendSchema validation is accepted.
	suspended, err := wf.Run(ctx, lebro.WorkflowRunInput{Input: json.RawMessage(`{"amount":100}`)})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if suspended.Status != lebro.RunStatusSuspended {
		t.Fatalf("Status = %q, want suspended", suspended.Status)
	}
	var req lebro.ApprovalRequest
	if err := json.Unmarshal(suspended.Suspend.Payload, &req); err != nil {
		t.Fatal(err)
	}

	// A schema-valid approval echoing the persisted request runs the tool once.
	decision, _ := json.Marshal(lebro.ApprovalDecision{Approved: true, Decider: "ops", DecidedAt: clock.Now(), Request: req})
	resumed, err := wf.Resume(ctx, lebro.WorkflowResumeInput{RunID: suspended.ID, Input: decision})
	if err != nil {
		t.Fatalf("Resume returned error: %v", err)
	}
	if resumed.Status != lebro.RunStatusSucceeded {
		t.Fatalf("resumed Status = %q, want succeeded", resumed.Status)
	}
	if innerCalls != 1 {
		t.Fatalf("protected handler ran %d times, want 1", innerCalls)
	}
}

// TestApprovalGateGuardEnforcesDecisionSchema proves the guard rejects a
// decision that violates the caller's DecisionSchema (compiled by the real JSON
// Schema compiler) and never runs the protected handler.
func TestApprovalGateGuardEnforcesDecisionSchema(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := lebro.NewMemoryStore()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	compiler := lebrojsonschema.NewCompiler()
	clock := lebro.NewFixedClock(time.Unix(7100, 0))

	var innerCalls int
	inner := lebro.StepHandlerFunc(func(_ context.Context, in json.RawMessage) (json.RawMessage, error) {
		innerCalls++
		return in, nil
	})
	gate, err := lebro.NewApprovalGate("await", "run", inner, lebro.ApprovalRequirement{
		Action:         lebro.ActionToolCall,
		DecisionSchema: json.RawMessage(`{"type":"object","required":["approved","decided_at"]}`),
	}, compiler, store, clock)
	if err != nil {
		t.Fatal(err)
	}
	wf, err := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition:     lebro.WorkflowDefinition{ID: "approval-schema", Version: "v1"},
		SchemaCompiler: compiler,
		Store:          store,
		Clock:          clock,
		IDSource:       lebro.NewFixedIDSource([]lebro.RunID{"schema-run-1"}, nil),
		Steps:          []lebro.Step{gate.Request, gate.Guard},
	})
	if err != nil {
		t.Fatal(err)
	}

	suspended, err := wf.Run(ctx, lebro.WorkflowRunInput{Input: json.RawMessage(`{"amount":100}`)})
	if err != nil {
		t.Fatal(err)
	}
	var req lebro.ApprovalRequest
	if err := json.Unmarshal(suspended.Suspend.Payload, &req); err != nil {
		t.Fatal(err)
	}

	// Missing the schema-required decided_at: rejected as an invalid decision.
	bad, _ := json.Marshal(map[string]any{"approved": true, "request": req})
	if _, err := wf.Resume(ctx, lebro.WorkflowResumeInput{RunID: suspended.ID, Input: bad}); !errors.Is(err, lebro.ErrApprovalInvalidDecision) {
		t.Fatalf("schema-violating decision err = %v, want ErrApprovalInvalidDecision", err)
	}
	if innerCalls != 0 {
		t.Fatalf("protected handler ran %d times on schema-violating decision, want 0", innerCalls)
	}
}
