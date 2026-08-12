package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// approvalWorkflow assembles a workflow whose single protected step is guarded
// by an approval gate. inner counts its invocations so a test can assert the
// protected handler never runs before a valid approval. The decision schema is
// permissive (any object) so approve and reject both pass resume validation and
// the gate's own logic decides the outcome.
func approvalWorkflow(t *testing.T, store Store, clock Clock, ids IDSource, listener RunListener, req ApprovalRequirement, inner StepHandler) *LinearWorkflow {
	t.Helper()
	if len(req.DecisionSchema) == 0 {
		req.DecisionSchema = json.RawMessage(`{"type":"object"}`)
	}
	gate, err := NewApprovalGate("await-approval", "run-tool", inner, req, contractCompiler(), store, clock)
	if err != nil {
		t.Fatal(err)
	}
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition:     WorkflowDefinition{ID: "approval-wf", Version: "v1"},
		SchemaCompiler: contractCompiler(),
		Clock:          clock,
		IDSource:       ids,
		Listener:       listener,
		Store:          store,
		Steps:          []Step{gate.Request, gate.Guard},
	})
	if err != nil {
		t.Fatal(err)
	}
	return wf
}

func decisionJSON(t *testing.T, d ApprovalDecision) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// requestFromSuspend decodes the ApprovalRequest a suspended run published so a
// test resumes with a decision echoing the exact reviewed request.
func requestFromSuspend(t *testing.T, res WorkflowRunResult) ApprovalRequest {
	t.Helper()
	if res.Suspend == nil {
		t.Fatal("run did not suspend")
	}
	var req ApprovalRequest
	if err := json.Unmarshal(res.Suspend.Payload, &req); err != nil {
		t.Fatalf("decode approval request: %v", err)
	}
	return req
}

func TestApprovalGateSuspendsBeforeProtectedToolRuns(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var innerCalls int32
	inner := StepHandlerFunc(func(_ context.Context, in json.RawMessage) (json.RawMessage, error) {
		atomic.AddInt32(&innerCalls, 1)
		return in, nil
	})
	wf := approvalWorkflow(t, store, NewFixedClock(time.Unix(1000, 0)), NewFixedIDSource([]RunID{"gate-run-1"}, nil), nil,
		ApprovalRequirement{Action: ActionToolCall, Resource: Resource{Kind: ResourceKindTool, ID: "wire.transfer"}, Reason: "sends money"}, inner)

	res, err := wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`{"amount":100}`)})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.Status != RunStatusSuspended {
		t.Fatalf("Status = %q, want suspended", res.Status)
	}
	if got := atomic.LoadInt32(&innerCalls); got != 0 {
		t.Fatalf("protected handler ran %d times before approval, want 0", got)
	}
	req := requestFromSuspend(t, res)
	if req.Action != ActionToolCall || req.Resource.ID != "wire.transfer" {
		t.Fatalf("request = %+v, want the declared action/resource", req)
	}
	if string(req.Arguments) != `{"amount":100}` {
		t.Fatalf("request Arguments = %s, want the reviewed input", req.Arguments)
	}
}

func TestApprovalGateApproveRunsProtectedToolOnce(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	clock := NewFixedClock(time.Unix(2000, 0))
	var innerCalls int32
	var seenArgs json.RawMessage
	inner := StepHandlerFunc(func(_ context.Context, in json.RawMessage) (json.RawMessage, error) {
		atomic.AddInt32(&innerCalls, 1)
		seenArgs = in
		return json.RawMessage(`{"transferred":true}`), nil
	})
	wf := approvalWorkflow(t, store, clock, NewFixedIDSource([]RunID{"approve-run-1"}, nil), nil,
		ApprovalRequirement{Action: ActionToolCall, TTL: time.Hour}, inner)

	suspended, err := wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`{"amount":100}`)})
	if err != nil {
		t.Fatal(err)
	}
	req := requestFromSuspend(t, suspended)

	decision := decisionJSON(t, ApprovalDecision{Approved: true, Decider: "ops", DecidedAt: clock.Now(), Request: req})
	resumed, err := wf.Resume(ctx, WorkflowResumeInput{RunID: suspended.ID, Input: decision})
	if err != nil {
		t.Fatalf("Resume returned error: %v", err)
	}
	if resumed.Status != RunStatusSucceeded {
		t.Fatalf("resumed Status = %q, want succeeded", resumed.Status)
	}
	if got := atomic.LoadInt32(&innerCalls); got != 1 {
		t.Fatalf("protected handler ran %d times, want 1", got)
	}
	if string(seenArgs) != `{"amount":100}` {
		t.Fatalf("protected handler saw args %s, want the reviewed input", seenArgs)
	}
	if string(resumed.Output) != `{"transferred":true}` {
		t.Fatalf("Output = %s, want the protected handler output", resumed.Output)
	}
}

func TestApprovalGateRejectFailsRunAndNeverRunsTool(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	clock := NewFixedClock(time.Unix(3000, 0))
	recorder := NewRunRecorder()
	var innerCalls int32
	inner := StepHandlerFunc(func(_ context.Context, in json.RawMessage) (json.RawMessage, error) {
		atomic.AddInt32(&innerCalls, 1)
		return in, nil
	})
	wf := approvalWorkflow(t, store, clock, NewFixedIDSource([]RunID{"reject-run-1"}, nil), recorder,
		ApprovalRequirement{Action: ActionToolCall}, inner)

	suspended, err := wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`{"amount":100}`)})
	if err != nil {
		t.Fatal(err)
	}
	req := requestFromSuspend(t, suspended)

	decision := decisionJSON(t, ApprovalDecision{Approved: false, Decider: "ops", Reason: "too large", DecidedAt: clock.Now(), Request: req})
	_, err = wf.Resume(ctx, WorkflowResumeInput{RunID: suspended.ID, Input: decision})
	if !errors.Is(err, ErrApprovalRejected) {
		t.Fatalf("err = %v, want ErrApprovalRejected", err)
	}
	if got := atomic.LoadInt32(&innerCalls); got != 0 {
		t.Fatalf("protected handler ran %d times on reject, want 0", got)
	}
	stored, err := store.WorkflowRuns().GetWorkflowRun(ctx, suspended.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != RunStatusFailed {
		t.Fatalf("stored status = %q, want failed (reject recorded)", stored.Status)
	}
	if !hasEventType(recorder.Events(), RunEventFailed) {
		t.Fatal("no run_failed event recorded for rejection")
	}
}

func TestApprovalGateTimeoutFailsRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// TTL is one minute; the decision is stamped an hour after the request, so
	// it lands outside the window and must be rejected as a timeout.
	clock := NewFixedClock(time.Unix(4000, 0))
	var innerCalls int32
	inner := StepHandlerFunc(func(_ context.Context, in json.RawMessage) (json.RawMessage, error) {
		atomic.AddInt32(&innerCalls, 1)
		return in, nil
	})
	wf := approvalWorkflow(t, store, clock, NewFixedIDSource([]RunID{"timeout-run-1"}, nil), nil,
		ApprovalRequirement{Action: ActionToolCall, TTL: time.Minute}, inner)

	suspended, err := wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`{"amount":100}`)})
	if err != nil {
		t.Fatal(err)
	}
	req := requestFromSuspend(t, suspended)
	if req.ExpiresAt.IsZero() {
		t.Fatal("request ExpiresAt is zero, want a TTL deadline")
	}

	lateDecision := decisionJSON(t, ApprovalDecision{Approved: true, DecidedAt: req.ExpiresAt.Add(time.Hour), Request: req})
	_, err = wf.Resume(ctx, WorkflowResumeInput{RunID: suspended.ID, Input: lateDecision})
	if !errors.Is(err, ErrApprovalExpired) {
		t.Fatalf("err = %v, want ErrApprovalExpired", err)
	}
	if got := atomic.LoadInt32(&innerCalls); got != 0 {
		t.Fatalf("protected handler ran %d times on timeout, want 0", got)
	}
	stored, err := store.WorkflowRuns().GetWorkflowRun(ctx, suspended.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != RunStatusFailed {
		t.Fatalf("stored status = %q, want failed (timeout recorded)", stored.Status)
	}
}

func TestApprovalGateLateDenialIsTimeoutNotRejection(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	clock := NewFixedClock(time.Unix(4500, 0))
	var innerCalls int32
	inner := StepHandlerFunc(func(_ context.Context, in json.RawMessage) (json.RawMessage, error) {
		atomic.AddInt32(&innerCalls, 1)
		return in, nil
	})
	wf := approvalWorkflow(t, store, clock, NewFixedIDSource([]RunID{"late-denial-run-1"}, nil), nil,
		ApprovalRequirement{Action: ActionToolCall, TTL: time.Minute}, inner)

	suspended, err := wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`{"amount":100}`)})
	if err != nil {
		t.Fatal(err)
	}
	req := requestFromSuspend(t, suspended)

	// A denial recorded after the window must surface as a timeout, not a
	// rejection: expiry is evaluated before the approval outcome.
	lateDenial := decisionJSON(t, ApprovalDecision{Approved: false, Reason: "no", DecidedAt: req.ExpiresAt.Add(time.Hour), Request: req})
	_, err = wf.Resume(ctx, WorkflowResumeInput{RunID: suspended.ID, Input: lateDenial})
	if !errors.Is(err, ErrApprovalExpired) {
		t.Fatalf("err = %v, want ErrApprovalExpired (expiry precedes rejection)", err)
	}
	if errors.Is(err, ErrApprovalRejected) {
		t.Fatalf("err = %v, must not be a rejection", err)
	}
}

func TestApprovalGateInvalidDecisionFailsRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	clock := NewFixedClock(time.Unix(5000, 0))
	var innerCalls int32
	inner := StepHandlerFunc(func(_ context.Context, in json.RawMessage) (json.RawMessage, error) {
		atomic.AddInt32(&innerCalls, 1)
		return in, nil
	})
	wf := approvalWorkflow(t, store, clock, NewFixedIDSource([]RunID{"invalid-run-1"}, nil), nil,
		ApprovalRequirement{Action: ActionToolCall}, inner)

	suspended, err := wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`{"amount":100}`)})
	if err != nil {
		t.Fatal(err)
	}
	req := requestFromSuspend(t, suspended)

	// Schema-valid object (passes resume validation) but no DecidedAt stamp:
	// the gate rejects it as an invalid decision rather than approving.
	invalid := decisionJSON(t, ApprovalDecision{Approved: true, Request: req})
	_, err = wf.Resume(ctx, WorkflowResumeInput{RunID: suspended.ID, Input: invalid})
	if !errors.Is(err, ErrApprovalInvalidDecision) {
		t.Fatalf("err = %v, want ErrApprovalInvalidDecision", err)
	}
	if got := atomic.LoadInt32(&innerCalls); got != 0 {
		t.Fatalf("protected handler ran %d times on invalid decision, want 0", got)
	}
}

func TestApprovalGateTamperedRequestFailsRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	clock := NewFixedClock(time.Unix(5500, 0))
	var innerCalls int32
	inner := StepHandlerFunc(func(_ context.Context, in json.RawMessage) (json.RawMessage, error) {
		atomic.AddInt32(&innerCalls, 1)
		return in, nil
	})
	wf := approvalWorkflow(t, store, clock, NewFixedIDSource([]RunID{"tamper-run-1"}, nil), nil,
		ApprovalRequirement{Action: ActionToolCall, Resource: Resource{Kind: ResourceKindTool, ID: "wire.transfer"}}, inner)

	suspended, err := wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`{"amount":100}`)})
	if err != nil {
		t.Fatal(err)
	}
	req := requestFromSuspend(t, suspended)

	// Tamper: approve, but swap the reviewed arguments for a larger transfer the
	// human never saw. The echoed request no longer matches the one persisted at
	// suspend, so the gate must reject it and never run the protected handler.
	tampered := req
	tampered.Arguments = json.RawMessage(`{"amount":1000000}`)
	decision := decisionJSON(t, ApprovalDecision{Approved: true, Decider: "attacker", DecidedAt: clock.Now(), Request: tampered})

	_, err = wf.Resume(ctx, WorkflowResumeInput{RunID: suspended.ID, Input: decision})
	if !errors.Is(err, ErrApprovalInvalidDecision) {
		t.Fatalf("err = %v, want ErrApprovalInvalidDecision", err)
	}
	if got := atomic.LoadInt32(&innerCalls); got != 0 {
		t.Fatalf("protected handler ran %d times on tampered request, want 0", got)
	}
	stored, err := store.WorkflowRuns().GetWorkflowRun(ctx, suspended.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != RunStatusFailed {
		t.Fatalf("stored status = %q, want failed (tamper recorded)", stored.Status)
	}
}

func TestApprovalGatePendingSurvivesRestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "approval.db")
	clock := NewFixedClock(time.Unix(6000, 0))

	var innerCalls int32
	inner := StepHandlerFunc(func(_ context.Context, in json.RawMessage) (json.RawMessage, error) {
		atomic.AddInt32(&innerCalls, 1)
		return json.RawMessage(`{"ok":true}`), nil
	})
	build := func(store Store) *LinearWorkflow {
		return approvalWorkflow(t, store, clock, NewFixedIDSource([]RunID{"restart-run-1"}, nil), nil,
			ApprovalRequirement{Action: ActionToolCall, TTL: time.Hour}, inner)
	}

	firstStore, err := NewSQLiteStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := firstStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	suspended, err := build(firstStore).Run(ctx, WorkflowRunInput{Input: json.RawMessage(`{"amount":100}`)})
	if err != nil {
		t.Fatal(err)
	}
	if suspended.Status != RunStatusSuspended {
		t.Fatalf("Status = %q, want suspended", suspended.Status)
	}
	req := requestFromSuspend(t, suspended)
	if err := firstStore.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen the same database from a fresh process and confirm the pending
	// approval is still resumable.
	secondStore, err := NewSQLiteStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = secondStore.Close() }()
	if err := secondStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	reloaded, err := secondStore.WorkflowRuns().GetWorkflowRun(ctx, suspended.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != RunStatusSuspended {
		t.Fatalf("reloaded status = %q, want suspended (pending approval must survive restart)", reloaded.Status)
	}

	decision := decisionJSON(t, ApprovalDecision{Approved: true, Decider: "ops", DecidedAt: clock.Now(), Request: req})
	resumed, err := build(secondStore).Resume(ctx, WorkflowResumeInput{RunID: suspended.ID, Input: decision})
	if err != nil {
		t.Fatalf("Resume after restart returned error: %v", err)
	}
	if resumed.Status != RunStatusSucceeded {
		t.Fatalf("resumed Status = %q, want succeeded", resumed.Status)
	}
	if got := atomic.LoadInt32(&innerCalls); got != 1 {
		t.Fatalf("protected handler ran %d times across restart, want 1", got)
	}
}

func hasEventType(events []RunEvent, want RunEventType) bool {
	for _, ev := range events {
		if ev.Type == want {
			return true
		}
	}
	return false
}
