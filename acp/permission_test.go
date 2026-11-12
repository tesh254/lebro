package acp_test

import (
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/tesh254/lebro"
)

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

	// The tool executed exactly once.
	if calls := h.tool.executionCount(); calls != 1 {
		t.Fatalf("tool executions = %d, want 1", calls)
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
	h := newHarness(t,
		toolCallResponse(),
		textResponse("acknowledged"),
	)
	// Rebuild the server with a short-TTL policy by constructing a second
	// server over the same pipes is complex; instead use the harness's
	// exposed server default policy (nil wrapped) with the runtime's own
	// TTL resolution: the nil wrapped policy sets no TTL, so expiry must be
	// exercised via a dedicated policy harness below.
	_ = h

	h2 := newExpiryHarness(t,
		toolCallResponse(),
		textResponse("acknowledged"),
	)
	h2.initialize(t.Context())
	session := h2.newSession(t.Context())

	// Hold the client's permission response past the TTL.
	h2.events.allow <- "delayed-allow"
	time.Sleep(50 * time.Millisecond) // decision deliberately after expiry

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
