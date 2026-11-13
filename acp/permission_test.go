package acp_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/tesh254/lebro"
)

func TestResumedTextPrecedesNextToolCall(t *testing.T) {
	second := lebro.ModelToolCall{ID: "call-second", ToolID: "acp_test.protected", Arguments: json.RawMessage(`{"value":"second"}`)}
	response := lebro.ModelResponse{
		Message:      lebro.Message{Role: lebro.RoleAssistant, Content: "before second tool", ToolCalls: mustToolCalls(second)},
		FinishReason: lebro.FinishReasonToolCalls,
	}
	h := newHarness(t, toolCallResponse(), response, textResponse("finished"))
	h.model.scripted = true
	h.initialize(t.Context())
	session := h.newSession(t.Context())
	h.events.allow <- "allow"
	h.events.allow <- "allow"
	h.mustPrompt(t.Context(), session, "run both")
	updates := h.events.snapshot()
	textIndex, toolIndex := -1, -1
	for i, event := range updates {
		if chunk := event.Update.AgentMessageChunk; chunk != nil && chunk.Content.Text != nil && chunk.Content.Text.Text == "before second tool" {
			textIndex = i
		}
		if call := event.Update.ToolCall; call != nil && call.ToolCallId == "call-second" {
			toolIndex = i
		}
	}
	if textIndex < 0 || toolIndex <= textIndex {
		t.Fatalf("continuation text index %d, second tool index %d", textIndex, toolIndex)
	}
	if count := h.tool.executionCount(); count != 2 {
		t.Fatalf("tool executions = %d, want 2", count)
	}
}

// approvalHarness is a harness whose agent suspends on the protected tool.
func approvalHarness(t *testing.T, steps ...lebro.ModelResponse) *harness {
	t.Helper()
	h := newHarness(t, steps...)
	// No wrapped policy: every protected call requires ACP approval. The
	// default harness builds exactly that, so nothing extra is needed.
	return h
}

// TestApprovedToolExecutesOnceAndAgentContinues drives the full happy path:
// the model requests the protected tool, the client allows it, the handler
// runs exactly once with the reviewed arguments, and the run continues to a
// second model call before completing.
func TestApprovedToolExecutesOnceAndAgentContinues(t *testing.T) {
	t.Parallel()
	h := approvalHarness(t,
		toolCallResponse(),
		textResponse("the transfer is complete"),
	)
	h.initialize(t.Context())
	session := h.newSession(t.Context())

	h.events.allow <- "allow"

	response := h.mustPrompt(t.Context(), session, "transfer please")
	if response.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %q", response.StopReason)
	}

	// The tool executed exactly once with the reviewed arguments.
	if calls := h.tool.executionCount(); calls != 1 {
		t.Fatalf("tool executions = %d, want 1", calls)
	}
	// The executed arguments match the approved persisted call.
	var executed struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(h.tool.lastArgs()), &executed); err != nil {
		t.Fatalf("decode executed arguments: %v", err)
	}
	if executed.Value != "reviewed" {
		t.Fatalf("executed arguments = %s, want the reviewed value", h.tool.lastArgs())
	}
	// The tool call was presented with a stable ID through its lifecycle:
	// pending -> in_progress -> completed, all with the same ID.
	calls := h.events.toolCalls()
	if len(calls) < 3 {
		t.Fatalf("expected at least 3 tool updates, got %v", calls)
	}
	id := calls[0][0]
	if id != "call-protected-1" {
		t.Fatalf("tool call id = %q", id)
	}
	if calls[0][1] != "pending" || calls[1][1] != "in_progress" || calls[2][1] != "completed" {
		t.Fatalf("unexpected status sequence: %v", calls)
	}
	// Every update in the lifecycle carries the same stable ID.
	for i := 1; i < 3; i++ {
		if calls[i][0] != id {
			t.Fatalf("tool update %d id = %q, want stable %q", i, calls[i][0], id)
		}
	}
	// The second model call happened (agent continued after the tool).
	waitFor(t, waitTimeout, func() bool {
		return contains(h.events.text(), "saw ")
	})
}

// TestDeniedToolDoesNotExecute asserts the reject option produces no handler
// execution and a deterministic stop.
func TestDeniedToolDoesNotExecute(t *testing.T) {
	t.Parallel()
	h := approvalHarness(t,
		toolCallResponse(),
		textResponse("acknowledged"),
	)
	h.initialize(t.Context())
	session := h.newSession(t.Context())

	h.events.allow <- "reject"

	// The run fails with the approval denial; the protocol error is an
	// internal error wrapping the runtime's denial.
	_, err := h.prompt(t.Context(), session, "transfer please")
	if err == nil {
		t.Fatal("denied approval should end the turn with an error")
	}
	if calls := h.tool.executionCount(); calls != 0 {
		t.Fatalf("tool executions = %d, want 0", calls)
	}
}

// TestCancelledPermissionOutcomeDoesNotExecute asserts the cancelled outcome
// (client cancels its own permission dialog) never runs the handler.
func TestCancelledPermissionOutcomeDoesNotExecute(t *testing.T) {
	t.Parallel()
	h := approvalHarness(t,
		toolCallResponse(),
		textResponse("acknowledged"),
	)
	h.initialize(t.Context())
	session := h.newSession(t.Context())

	// Empty option id scripts the client's cancelled outcome.
	h.events.allow <- ""

	_, err := h.prompt(t.Context(), session, "transfer please")
	if err == nil {
		t.Fatal("cancelled permission should end the turn with an error")
	}
	if calls := h.tool.executionCount(); calls != 0 {
		t.Fatalf("tool executions = %d, want 0", calls)
	}
}

// TestUnknownOptionIsNotApproval asserts an unrecognized option ID follows
// the ACP security rule: an unrecognized outcome is not approval.
func TestUnknownOptionIsNotApproval(t *testing.T) {
	t.Parallel()
	h := approvalHarness(t,
		toolCallResponse(),
		textResponse("acknowledged"),
	)
	h.initialize(t.Context())
	session := h.newSession(t.Context())

	h.events.allow <- "allow-always-pls"

	_, err := h.prompt(t.Context(), session, "transfer please")
	if err == nil {
		t.Fatal("unknown option should not approve the tool")
	}
	if calls := h.tool.executionCount(); calls != 0 {
		t.Fatalf("tool executions = %d, want 0", calls)
	}
}

// TestApprovalExpiryFailsClosed asserts a TTL that lapses between request and
// decision fails the run through the runtime's expiry check, executing
// nothing. The bridge submits the decision late by gating the client.
func TestApprovalExpiryFailsClosed(t *testing.T) {
	t.Parallel()
	// Wrap the harness with a policy carrying a tiny TTL.
	h2 := newExpiryHarness(t,
		toolCallResponse(),
		textResponse("acknowledged"),
	)
	h2.initialize(t.Context())
	session := h2.newSession(t.Context())

	// Hold the client's permission response past the TTL.
	h2.events.allow <- "delayed-allow"

	_, err := h2.prompt(t.Context(), session, "transfer please")
	if err == nil {
		t.Fatal("expired approval should fail the turn")
	}
	if calls := h2.tool.executionCount(); calls != 0 {
		t.Fatalf("tool executions = %d, want 0", calls)
	}
}

// TestDisconnectDuringPermissionDoesNotExecute closes the client pipes while
// a permission request is pending and asserts the handler never runs.
func TestDisconnectDuringPermissionDoesNotExecute(t *testing.T) {
	t.Parallel()
	h := approvalHarness(t,
		toolCallResponse(),
	)
	h.initialize(t.Context())
	session := h.newSession(t.Context())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = h.prompt(t.Context(), session, "transfer please")
	}()
	waitFor(t, waitTimeout, func() bool {
		return len(h.events.toolCalls()) > 0
	})

	// Drop the connection while the permission request waits.
	h.hangUp()

	select {
	case <-done:
	case <-time.After(waitTimeout):
		t.Fatal("prompt did not terminate after disconnect")
	}
	if calls := h.tool.executionCount(); calls != 0 {
		t.Fatalf("tool executions = %d, want 0", calls)
	}
}

// TestCancelledApprovalIsDeniedNotStranded asserts a session cancel while an
// approval is pending records a terminal denial for the persisted request —
// it must not stay pending and resumable — and emits a failed tool-call
// update so the client never sees the call stuck in pending.
func TestCancelledApprovalIsDeniedNotStranded(t *testing.T) {
	t.Parallel()
	h := approvalHarness(t,
		toolCallResponse(),
	)
	h.initialize(t.Context())
	session := h.newSession(t.Context())

	done := make(chan struct{})
	var response acp.PromptResponse
	var err error
	go func() {
		defer close(done)
		response, err = h.prompt(t.Context(), session, "transfer please")
	}()
	waitFor(t, waitTimeout, func() bool {
		return len(h.events.toolCalls()) > 0
	})

	if err := h.client.Cancel(t.Context(), acp.CancelNotification{SessionId: session}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case <-done:
	case <-time.After(waitTimeout):
		t.Fatal("prompt did not terminate after cancel")
	}
	if err != nil {
		t.Fatalf("cancelled prompt returned error: %v", err)
	}
	if response.StopReason != acp.StopReasonCancelled {
		t.Fatalf("stop reason = %q, want cancelled", response.StopReason)
	}
	if calls := h.tool.executionCount(); calls != 0 {
		t.Fatalf("tool executions = %d, want 0", calls)
	}
	// The presented call must end in a terminal failed state: the adapter
	// closed the persisted approval when the turn ended.
	calls := h.events.updatesByToolCall()
	if len(calls) == 0 {
		t.Fatal("no tool-call updates recorded")
	}
	// The presented call must end in a terminal failed state: the adapter
	// closed the persisted approval when the turn ended.
	var sawFailed bool
	for _, call := range calls {
		if call.status == "failed" {
			sawFailed = true
		}
	}
	if !sawFailed {
		t.Fatalf("tool-call updates never reached failed after cancel: %v", calls)
	}
}
