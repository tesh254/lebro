package acp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/tesh254/lebro"
)

// Prompt runs one ACP turn against the exposed agent.
//
// The turn is driven by the runtime's RunEvent stream rather than StreamRun
// deltas: approval resumes execute through Agent.Run, which emits events but
// no deltas, so events are the one ordering surface that covers both the
// initial run and every resumed continuation. The listener bridge routes
// events for this turn's RunID into the ACP updates below.
func (s *Server) Prompt(ctx context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	sess, ok := s.lookupSession(params.SessionId)
	if !ok {
		return acp.PromptResponse{}, unknownSession(params.SessionId)
	}
	messages, err := promptMessages(params.Prompt)
	if err != nil {
		return acp.PromptResponse{}, acp.NewInvalidParams(map[string]any{"error": err.Error()})
	}

	s.mu.Lock()
	if sess.running {
		s.mu.Unlock()
		return acp.PromptResponse{}, acp.NewInvalidParams(map[string]any{
			"error": "a prompt is already running for this session",
		})
	}
	sess.running = true
	ctx, cancel := context.WithCancel(ctx)
	sess.cancel = cancel
	sess.turn++
	runID := lebro.RunID(fmt.Sprintf("acp-%s-%d", sess.id, sess.turn))
	bridge := newEventBridge(s, sess, string(runID), ctx)
	sess.bridge = bridge
	s.mu.Unlock()

	defer func() {
		cancel()
		s.mu.Lock()
		sess.running = false
		sess.bridge = nil
		sess.cancel = nil
		s.mu.Unlock()
	}()

	input := lebro.RunInput{
		RunID:    runID,
		ThreadID: sess.thread,
		Messages: messages,
	}
	agent := sess.agent
	stream, err := agent.RunStream(ctx, input)
	if err != nil {
		if ctx.Err() != nil {
			return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
		}
		return acp.PromptResponse{}, sdkError(err)
	}
	// The event bridge drives the ACP updates, but the runtime blocks its
	// stream loop unless the deltas channel is drained, so discard deltas
	// concurrently and collect the terminal result from Wait. Cancel on
	// abandonment so goroutines never leak.
	defer stream.Cancel()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range stream.Deltas {
		}
	}()
	result, waitErr := stream.Wait()
	<-drained

	// Present each persisted approval request and resume it before handling
	// any subsequent suspension in this turn. A cancelled turn or dropped
	// connection still owes the persisted request a terminal decision, so
	// the loop records a denial with a non-cancelable cleanup context
	// instead of leaving the approval stranded in its pending state.
	for waitErr == nil && result.Status == lebro.RunStatusSuspended && result.ToolApproval != nil {
		if ctx.Err() != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			result, waitErr = s.denySuspended(cleanupCtx, sess.agent, result, "acp session ended before approval")
			cleanupCancel()
			break
		}
		resumed, resumeErr := s.resumeSuspended(ctx, sess.agent, result)
		result, waitErr = resumed, resumeErr
	}

	// Deltas are not consumed: the bridge covers ordered output. But the
	// runtime requires the stream's terminal error to come from Wait.
	if ctx.Err() != nil || result.Status == lebro.RunStatusCancelled {
		// Cancellation is a stop reason, not a protocol error.
		return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
	}
	if waitErr != nil {
		return acp.PromptResponse{}, sdkError(waitErr)
	}
	switch result.Status {
	case lebro.RunStatusSucceeded:
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	case lebro.RunStatusCancelled:
		return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
	case lebro.RunStatusSuspended:
		return acp.PromptResponse{StopReason: acp.StopReasonRefusal}, nil
	default:
		return acp.PromptResponse{StopReason: acp.StopReasonRefusal}, nil
	}
}

// promptMessages converts baseline ACP prompt content into lebro messages.
// Only text and resource_link are accepted — the v1 baseline — and the server
// advertises no optional content capabilities, so image, audio, and resource
// blocks are protocol violations and rejected explicitly.
func promptMessages(blocks []acp.ContentBlock) ([]lebro.Message, error) {
	var builder strings.Builder
	for _, block := range blocks {
		switch {
		case block.Text != nil:
			builder.WriteString(block.Text.Text)
		case block.ResourceLink != nil:
			link := block.ResourceLink
			// A resource link is a reference, not a grant: pass it as
			// descriptive text and never read the URI.
			name := link.Name
			if name == "" {
				name = link.Uri
			}
			fmt.Fprintf(&builder, "[resource link: %s (%s)]", name, link.Uri)
		case block.Image != nil, block.Audio != nil, block.Resource != nil:
			return nil, fmt.Errorf("unsupported content block type; this agent supports only text and resource_link")
		default:
			return nil, fmt.Errorf("empty or unknown content block")
		}
	}
	if strings.TrimSpace(builder.String()) == "" {
		return nil, fmt.Errorf("prompt contains no text content")
	}
	return []lebro.Message{{Role: lebro.RoleUser, Content: builder.String()}}, nil
}

// resumeSuspended presents the persisted request and resumes its decision.
func (s *Server) resumeSuspended(ctx context.Context, agent *lebro.Agent, suspended lebro.RunResult) (lebro.RunResult, error) {
	bridge := s.permissionBridge()
	if bridge == nil {
		return suspended, nil
	}
	request := suspended.ToolApproval
	approved, reason := bridge.requestPermission(ctx, request)
	if !approved {
		s.failToolCall(request)
	}
	resume := lebro.ToolApprovalDecision{
		RunID:     request.RunID,
		RequestID: request.ID,
		Approved:  approved,
		Decider:   "acp-client",
		Reason:    reason,
		DecidedAt: s.agentClock().Now(),
	}
	if agent == nil {
		return suspended, nil
	}
	result, err := agent.ResumeToolApproval(ctx, resume)
	if err != nil {
		// Denied, expired, stale, and uncertain are terminal runtime
		// failures for the run; surface them as protocol errors with the
		// result's terminal state intact. The presented call must not stay
		// pending in the client: denied and expired decisions emit their
		// own failed update below, and stale or uncertain ones are equally
		// terminal for this turn.
		s.failToolCall(request)
		return result, sdkError(err)
	}
	return result, nil
}

// denySuspended records a denial for a persisted approval whose presenting
// turn was cancelled or disconnected. The runtime treats the request as
// failed, so it cannot be resumed later, and the handler never runs. The
// context is a short non-cancelable derivation maintained by the caller.
func (s *Server) denySuspended(ctx context.Context, agent *lebro.Agent, suspended lebro.RunResult, reason string) (lebro.RunResult, error) {
	request := suspended.ToolApproval
	s.failToolCall(request)
	if agent == nil {
		return suspended, nil
	}
	resume := lebro.ToolApprovalDecision{
		RunID:     request.RunID,
		RequestID: request.ID,
		Approved:  false,
		Decider:   "acp-server",
		Reason:    reason,
		DecidedAt: s.agentClock().Now(),
	}
	result, err := agent.ResumeToolApproval(ctx, resume)
	if err != nil {
		// A denied decision is the expected cleanup outcome; the runtime
		// reports it as a ToolApprovalError. Any other failure means the
		// terminal transition did not land (for example a store conflict
		// after a concurrent decision), which is worth a diagnostic.
		var approvalErr *lebro.ToolApprovalError
		if !errors.As(err, &approvalErr) {
			s.logger.Debug("acp: approval cleanup failed", "run", request.RunID, "request", request.ID, "error", err)
		}
	}
	return result, nil
}

// agentClock returns the exposed agent's configured clock so decision
// timestamps agree with the runtime's expiry checks even when tests inject a
// fixed clock.
func (s *Server) agentClock() lebro.Clock {
	if agent := s.agent(); agent != nil {
		if clock := agent.Clock(); clock != nil {
			return clock
		}
	}
	return lebro.SystemClock
}

// failToolCall emits a terminal failed update for a presented tool call whose
// approval was rejected, expired, cancelled, or otherwise ended without
// execution, so the client never sees the call stuck in pending after the
// turn has ended. It uses the connection's context rather than the bridge's
// prompt context: on cancellation that context is already done, and a
// still-connected client should still observe the terminal status. When the
// connection itself is gone the write fails and is logged as a diagnostic.
func (s *Server) failToolCall(request *lebro.ToolApprovalRequest) {
	bridge := s.activeBridge(string(request.RunID))
	if bridge == nil {
		return
	}
	bridge.sendWith(s.serveContext(), acp.UpdateToolCall(
		acp.ToolCallId(request.ToolCallID),
		acp.WithUpdateStatus(acp.ToolCallStatusFailed),
	))
}

// unknownSession builds the ACP resource-not-found error for a session that
// this connection never created.
func unknownSession(id acp.SessionId) *acp.RequestError {
	return &acp.RequestError{
		Code:    -32002,
		Message: "Resource not found",
		Data:    map[string]any{"sessionId": string(id)},
	}
}

// sdkError converts a lebro error into a JSON-RPC error without leaking
// internal details beyond the error's own message.
func sdkError(err error) *acp.RequestError {
	if err == nil {
		return nil
	}
	var reqErr *acp.RequestError
	if errors.As(err, &reqErr) {
		return reqErr
	}
	return acp.NewInternalError(map[string]any{"error": err.Error()})
}

// cleanupTimeout bounds the non-cancelable approval cleanup that records a
// denial after cancellation or disconnect.
const cleanupTimeout = 5 * time.Second
