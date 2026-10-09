package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func approvalPolicyRequire(_ context.Context, _ ToolApprovalInvocation) (ToolApprovalResolution, error) {
	return ToolApprovalResolution{Outcome: ToolApprovalRequire, Reason: "review external write", Requester: "payments-policy", Metadata: map[string]string{"queue": "operations"}, TTL: time.Hour}, nil
}

func TestAgentToolApprovalSuspendsAndResumesWithoutRepeatingModel(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	registry, handler := newAgentTestRegistry(t)
	var calls atomic.Int32
	var seen json.RawMessage
	handler.execute = func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
		calls.Add(1)
		seen = append(seen[:0], args...)
		return json.RawMessage(`{"written":true}`), nil
	}
	model := newScriptedModel(
		toolCallResponse(ModelToolCall{ID: "call-write", ToolID: "lookup", Arguments: json.RawMessage(`{"amount":100,"currency":"KES"}`)}),
		textResponse("write completed"),
	)
	newAgent := func() *Agent {
		agent, err := NewAgent(AgentConfig{
			Definition:         AgentDefinition{ID: "approval-agent", Instructions: "be careful", Tools: []ToolID{"lookup"}},
			Model:              model,
			Tools:              registry,
			Store:              store,
			ToolApprovalPolicy: ToolApprovalPolicyFunc(approvalPolicyRequire),
		})
		if err != nil {
			t.Fatal(err)
		}
		return agent
	}

	first := newAgent()
	suspended, err := first.Run(ctx, RunInput{RunID: "approval-run", Messages: []Message{{Role: RoleUser, Content: "send funds"}}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if suspended.Status != RunStatusSuspended || suspended.ToolApproval == nil {
		t.Fatalf("suspend = %#v, want pending approval", suspended)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("handler calls before approval = %d, want 0", got)
	}
	request := *suspended.ToolApproval
	if request.ToolID != "lookup" || request.ToolCallID != "call-write" || string(request.Arguments) != `{"amount":100,"currency":"KES"}` {
		t.Fatalf("request = %#v", request)
	}
	if request.Requester != "payments-policy" || request.Metadata["queue"] != "operations" {
		t.Fatalf("request audit metadata = %#v", request)
	}
	if len(model.calls) != 1 {
		t.Fatalf("model calls before approval = %d, want 1", len(model.calls))
	}

	// A new Agent instance simulates a process restart. It must load the same
	// request and continue from the persisted assistant tool call.
	restarted := newAgent()
	pending, err := restarted.PendingToolApproval(ctx, "approval-run")
	if err != nil {
		t.Fatalf("PendingToolApproval() error = %v", err)
	}
	if pending.ID != request.ID || string(pending.Arguments) != string(request.Arguments) {
		t.Fatalf("pending = %#v, want persisted request %#v", pending, request)
	}
	result, err := restarted.ResumeToolApproval(ctx, ToolApprovalDecision{RunID: "approval-run", RequestID: request.ID, Approved: true, Decider: "operator", DecidedAt: time.Now().UTC()})
	if err != nil {
		t.Fatalf("ResumeToolApproval() error = %v", err)
	}
	if result.Status != RunStatusSucceeded || result.Messages[len(result.Messages)-1].Content != "write completed" {
		t.Fatalf("result = %#v", result)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls after approval = %d, want 1", got)
	}
	if string(seen) != string(request.Arguments) {
		t.Fatalf("handler args = %s, want reviewed %s", seen, request.Arguments)
	}
	if len(model.calls) != 2 {
		t.Fatalf("model calls = %d, want 2; current tool call must not be regenerated", len(model.calls))
	}
	if got := model.calls[1].Messages; len(got) != 4 || got[2].ToolCalls.IsZero() || got[3].ToolCallID != "call-write" {
		t.Fatalf("resumed model transcript = %#v", got)
	}
	stored, err := store.WorkflowRuns().GetWorkflowRun(ctx, "approval-run")
	if err != nil || stored.Status != RunStatusSucceeded {
		t.Fatalf("stored run = %#v, err = %v", stored, err)
	}
	snapshots, err := store.WorkflowSnapshots().ListWorkflowSnapshots(ctx, "approval-run", PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var persisted toolApprovalSnapshot
	if err := json.Unmarshal(snapshots.Records[len(snapshots.Records)-1].State, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Decision == nil || persisted.Decision.Decider != "operator" || !persisted.Decision.Approved {
		t.Fatalf("persisted approval decision = %#v", persisted.Decision)
	}
}

func TestAgentToolApprovalRejectsAndRejectsDuplicateDecision(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	registry, handler := newAgentTestRegistry(t)
	var calls atomic.Int32
	handler.execute = func(context.Context, json.RawMessage) (json.RawMessage, error) {
		calls.Add(1)
		return json.RawMessage(`{}`), nil
	}
	agent, err := NewAgent(AgentConfig{
		Definition:         AgentDefinition{ID: "rejecting-agent", Tools: []ToolID{"lookup"}},
		Model:              newScriptedModel(toolCallResponse(ModelToolCall{ID: "call-reject", ToolID: "lookup", Arguments: json.RawMessage(`{}`)})),
		Tools:              registry,
		Store:              store,
		ToolApprovalPolicy: ToolApprovalPolicyFunc(approvalPolicyRequire),
	})
	if err != nil {
		t.Fatal(err)
	}
	suspended, err := agent.Run(ctx, RunInput{RunID: "reject-run", Messages: []Message{{Role: RoleUser, Content: "do it"}}})
	if err != nil {
		t.Fatal(err)
	}
	request := suspended.ToolApproval
	if request == nil {
		t.Fatal("missing pending approval")
	}
	_, err = agent.ResumeToolApproval(ctx, ToolApprovalDecision{RunID: "reject-run", RequestID: request.ID, Approved: false, DecidedAt: time.Now().UTC()})
	if !errors.Is(err, ErrToolApprovalDenied) {
		t.Fatalf("reject error = %v, want ErrToolApprovalDenied", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("handler calls after rejection = %d, want 0", got)
	}
	_, err = agent.ResumeToolApproval(ctx, ToolApprovalDecision{RunID: "reject-run", RequestID: request.ID, Approved: true, DecidedAt: time.Now().UTC()})
	if !errors.Is(err, ErrToolApprovalStale) {
		t.Fatalf("duplicate decision error = %v, want ErrToolApprovalStale", err)
	}
}

func TestAgentToolApprovalConcurrentDecisionExecutesExactlyOnce(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	registry, handler := newAgentTestRegistry(t)
	var calls atomic.Int32
	handler.execute = func(context.Context, json.RawMessage) (json.RawMessage, error) {
		calls.Add(1)
		return json.RawMessage(`{"ok":true}`), nil
	}
	agent, err := NewAgent(AgentConfig{
		Definition:         AgentDefinition{ID: "concurrent-approval", Tools: []ToolID{"lookup"}},
		Model:              newScriptedModel(toolCallResponse(ModelToolCall{ID: "call-concurrent", ToolID: "lookup", Arguments: json.RawMessage(`{}`)}), textResponse("completed")),
		Tools:              registry,
		Store:              store,
		ToolApprovalPolicy: ToolApprovalPolicyFunc(approvalPolicyRequire),
	})
	if err != nil {
		t.Fatal(err)
	}
	suspended, err := agent.Run(ctx, RunInput{RunID: "concurrent-approval-run", Messages: []Message{{Role: RoleUser, Content: "write"}}})
	if err != nil || suspended.ToolApproval == nil {
		t.Fatalf("suspended = %#v, err = %v", suspended, err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := agent.ResumeToolApproval(ctx, ToolApprovalDecision{RunID: suspended.ID, RequestID: suspended.ToolApproval.ID, Approved: true, DecidedAt: time.Now().UTC()})
			errs <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errs)

	var succeeded, stale int
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrToolApprovalStale):
			stale++
		default:
			t.Fatalf("concurrent decision error = %v", err)
		}
	}
	if succeeded != 1 || stale != 1 || calls.Load() != 1 {
		t.Fatalf("succeeded = %d, stale = %d, handler calls = %d", succeeded, stale, calls.Load())
	}
}

func TestAgentToolApprovalSuspendsEachProtectedCallInOrder(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	registry, handler := newAgentTestRegistry(t)
	var calls []string
	handler.execute = func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
		calls = append(calls, string(args))
		return append(json.RawMessage(nil), args...), nil
	}
	toolCalls, err := NewModelToolCalls(
		ModelToolCall{ID: "call-one", ToolID: "lookup", Arguments: json.RawMessage(`{"n":1}`)},
		ModelToolCall{ID: "call-two", ToolID: "lookup", Arguments: json.RawMessage(`{"n":2}`)},
	)
	if err != nil {
		t.Fatal(err)
	}
	model := newScriptedModel(
		scriptedResponse{response: ModelResponse{Message: Message{Role: RoleAssistant, ToolCalls: toolCalls}, FinishReason: FinishReasonToolCalls}},
		textResponse("both approved"),
	)
	agent, err := NewAgent(AgentConfig{
		Definition:         AgentDefinition{ID: "multi-approval", Tools: []ToolID{"lookup"}},
		Model:              model,
		Tools:              registry,
		Store:              store,
		ToolApprovalPolicy: ToolApprovalPolicyFunc(approvalPolicyRequire),
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := agent.Run(ctx, RunInput{RunID: "multi-approval-run", Messages: []Message{{Role: RoleUser, Content: "two writes"}}})
	if err != nil || first.ToolApproval == nil {
		t.Fatalf("first = %#v, err = %v", first, err)
	}
	second, err := agent.ResumeToolApproval(ctx, ToolApprovalDecision{RunID: first.ID, RequestID: first.ToolApproval.ID, Approved: true, DecidedAt: time.Now().UTC()})
	if err != nil || second.Status != RunStatusSuspended || second.ToolApproval == nil {
		t.Fatalf("second = %#v, err = %v", second, err)
	}
	if len(calls) != 1 || calls[0] != `{"n":1}` {
		t.Fatalf("calls after first approval = %#v", calls)
	}
	final, err := agent.ResumeToolApproval(ctx, ToolApprovalDecision{RunID: first.ID, RequestID: second.ToolApproval.ID, Approved: true, DecidedAt: time.Now().UTC()})
	if err != nil || final.Status != RunStatusSucceeded || len(calls) != 2 || calls[1] != `{"n":2}` {
		t.Fatalf("final = %#v, calls = %#v, err = %v", final, calls, err)
	}
	if len(model.calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(model.calls))
	}
	snapshots, err := store.WorkflowSnapshots().ListWorkflowSnapshots(ctx, first.ID, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots.Records) != 4 {
		t.Fatalf("approval snapshots = %d, want 4", len(snapshots.Records))
	}
	for i, snapshot := range snapshots.Records {
		if want := int64(i + 1); snapshot.Sequence != want {
			t.Fatalf("snapshot %d sequence = %d, want %d", i, snapshot.Sequence, want)
		}
	}
}

func TestAgentToolApprovalExpiryNeverInvokesHandler(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	clock := NewFixedClock(time.Unix(100, 0))
	registry, handler := newAgentTestRegistry(t)
	var calls atomic.Int32
	handler.execute = func(context.Context, json.RawMessage) (json.RawMessage, error) {
		calls.Add(1)
		return json.RawMessage(`{}`), nil
	}
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "expired-agent", Tools: []ToolID{"lookup"}},
		Model:      newScriptedModel(toolCallResponse(ModelToolCall{ID: "call-expired", ToolID: "lookup", Arguments: json.RawMessage(`{}`)})),
		Tools:      registry,
		Store:      store,
		Clock:      clock,
		ToolApprovalPolicy: ToolApprovalPolicyFunc(func(context.Context, ToolApprovalInvocation) (ToolApprovalResolution, error) {
			return ToolApprovalResolution{Outcome: ToolApprovalRequire, TTL: time.Minute}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	suspended, err := agent.Run(ctx, RunInput{RunID: "expired-run", Messages: []Message{{Role: RoleUser, Content: "do it"}}})
	if err != nil {
		t.Fatal(err)
	}
	request := suspended.ToolApproval
	_, err = agent.ResumeToolApproval(ctx, ToolApprovalDecision{RunID: "expired-run", RequestID: request.ID, Approved: true, DecidedAt: request.ExpiresAt.Add(time.Second)})
	if !errors.Is(err, ErrToolApprovalExpired) {
		t.Fatalf("expiry error = %v, want ErrToolApprovalExpired", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("handler calls after expiry = %d, want 0", got)
	}
}

func TestAgentToolApprovalExecutingMarkerNeverReplays(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	registry, handler := newAgentTestRegistry(t)
	var calls atomic.Int32
	handler.execute = func(context.Context, json.RawMessage) (json.RawMessage, error) {
		calls.Add(1)
		return json.RawMessage(`{}`), nil
	}
	agent, err := NewAgent(AgentConfig{
		Definition:         AgentDefinition{ID: "uncertain-approval", Tools: []ToolID{"lookup"}},
		Model:              newScriptedModel(toolCallResponse(ModelToolCall{ID: "call-uncertain", ToolID: "lookup", Arguments: json.RawMessage(`{}`)})),
		Tools:              registry,
		Store:              store,
		ToolApprovalPolicy: ToolApprovalPolicyFunc(approvalPolicyRequire),
	})
	if err != nil {
		t.Fatal(err)
	}
	suspended, err := agent.Run(ctx, RunInput{RunID: "uncertain-approval-run", Messages: []Message{{Role: RoleUser, Content: "write"}}})
	if err != nil || suspended.ToolApproval == nil {
		t.Fatalf("suspended = %#v, err = %v", suspended, err)
	}
	_, state, err := agent.loadToolApproval(ctx, suspended.ID)
	if err != nil {
		t.Fatal(err)
	}
	decision := ToolApprovalDecision{RunID: suspended.ID, RequestID: suspended.ToolApproval.ID, Approved: true, DecidedAt: time.Now().UTC()}
	if err := agent.transitionToolApproval(ctx, state, &decision, RunStatusRunning, toolApprovalExecuting, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.PendingToolApproval(ctx, suspended.ID); !errors.Is(err, ErrToolApprovalUncertain) {
		t.Fatalf("PendingToolApproval() error = %v, want ErrToolApprovalUncertain", err)
	}
	if _, err := agent.ResumeToolApproval(ctx, decision); !errors.Is(err, ErrToolApprovalUncertain) {
		t.Fatalf("ResumeToolApproval() error = %v, want ErrToolApprovalUncertain", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("handler calls after executing marker = %d, want 0", got)
	}
}

func TestAgentToolApprovalSQLiteSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "tool-approval.db")
	registry, handler := newAgentTestRegistry(t)
	var calls atomic.Int32
	handler.execute = func(context.Context, json.RawMessage) (json.RawMessage, error) {
		calls.Add(1)
		return json.RawMessage(`{"ok":true}`), nil
	}
	model := newScriptedModel(
		toolCallResponse(ModelToolCall{ID: "call-sqlite", ToolID: "lookup", Arguments: json.RawMessage(`{"target":"sqlite"}`)}),
		textResponse("resumed from SQLite"),
	)
	build := func(store Store) *Agent {
		agent, err := NewAgent(AgentConfig{
			Definition:         AgentDefinition{ID: "sqlite-approval", Tools: []ToolID{"lookup"}},
			Model:              model,
			Tools:              registry,
			Store:              store,
			ToolApprovalPolicy: ToolApprovalPolicyFunc(approvalPolicyRequire),
		})
		if err != nil {
			t.Fatal(err)
		}
		return agent
	}

	first, err := NewSQLiteStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	suspended, err := build(first).Run(ctx, RunInput{RunID: "sqlite-approval-run", Messages: []Message{{Role: RoleUser, Content: "write"}}})
	if err != nil {
		t.Fatal(err)
	}
	request := suspended.ToolApproval
	if request == nil || calls.Load() != 0 {
		t.Fatalf("suspended = %#v, handler calls = %d", suspended, calls.Load())
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	restarted := build(reopened)
	pending, err := restarted.PendingToolApproval(ctx, suspended.ID)
	if err != nil || pending.ID != request.ID || string(pending.Arguments) != string(request.Arguments) {
		t.Fatalf("pending = %#v, err = %v", pending, err)
	}
	result, err := restarted.ResumeToolApproval(ctx, ToolApprovalDecision{RunID: suspended.ID, RequestID: pending.ID, Approved: true, DecidedAt: time.Now().UTC()})
	if err != nil || result.Status != RunStatusSucceeded || calls.Load() != 1 {
		t.Fatalf("result = %#v, calls = %d, err = %v", result, calls.Load(), err)
	}
}

func TestAgentToolApprovalPostgresContract(t *testing.T) {
	store := newTestPostgresStore(t)
	ctx := context.Background()
	registry, handler := newAgentTestRegistry(t)
	var calls atomic.Int32
	handler.execute = func(context.Context, json.RawMessage) (json.RawMessage, error) {
		calls.Add(1)
		return json.RawMessage(`{}`), nil
	}
	agent, err := NewAgent(AgentConfig{
		Definition:         AgentDefinition{ID: "postgres-approval", Tools: []ToolID{"lookup"}},
		Model:              newScriptedModel(toolCallResponse(ModelToolCall{ID: "call-postgres", ToolID: "lookup", Arguments: json.RawMessage(`{}`)})),
		Tools:              registry,
		Store:              store,
		ToolApprovalPolicy: ToolApprovalPolicyFunc(approvalPolicyRequire),
	})
	if err != nil {
		t.Fatal(err)
	}
	suspended, err := agent.Run(ctx, RunInput{RunID: "postgres-approval-run", Messages: []Message{{Role: RoleUser, Content: "write"}}})
	if err != nil || suspended.Status != RunStatusSuspended || calls.Load() != 0 {
		t.Fatalf("suspended = %#v, calls = %d, err = %v", suspended, calls.Load(), err)
	}
	pending, err := agent.PendingToolApproval(ctx, suspended.ID)
	if err != nil || pending.ID != suspended.ToolApproval.ID {
		t.Fatalf("pending = %#v, err = %v", pending, err)
	}
}

func TestAgentRunStreamToolApprovalSuspendsBeforeHandler(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	registry, handler := newAgentTestRegistry(t)
	var calls atomic.Int32
	handler.execute = func(context.Context, json.RawMessage) (json.RawMessage, error) {
		calls.Add(1)
		return json.RawMessage(`{}`), nil
	}
	model := newStreamScriptedModel(toolCallDeltaStream(ModelToolCall{ID: "call-stream", ToolID: "lookup", Arguments: json.RawMessage(`{"write":true}`)}))
	agent, err := NewAgent(AgentConfig{
		Definition:         AgentDefinition{ID: "stream-approval", Tools: []ToolID{"lookup"}},
		Model:              model,
		Tools:              registry,
		Store:              store,
		ToolApprovalPolicy: ToolApprovalPolicyFunc(approvalPolicyRequire),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := agent.RunStream(ctx, RunInput{RunID: "stream-approval-run", Messages: []Message{{Role: RoleUser, Content: "write"}}})
	if err != nil {
		t.Fatal(err)
	}
	for range run.Deltas {
	}
	result, err := run.Wait()
	if err != nil || result.Status != RunStatusSuspended || result.ToolApproval == nil || calls.Load() != 0 {
		t.Fatalf("result = %#v, calls = %d, err = %v", result, calls.Load(), err)
	}
}
