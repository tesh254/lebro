package acp

import (
	"context"
	"fmt"
	"sync"

	"github.com/coder/acp-go-sdk"
	"github.com/tesh254/lebro"
)

// eventBridge translates one turn's ordered RunEvents into ACP session
// updates. The server registers it as the agent's RunListener for the turn by
// wrapping the agent config before construction; it filters on the turn's
// RunID so concurrent sessions never observe each other's events.
//
// Ordering guarantee: the runtime emits events sequentially per run, and the
// bridge forwards them synchronously in OnRunEvent, so ACP updates leave in
// event order. The SDK connection serializes writes, so concurrent sessions
// cannot interleave message bytes.
type eventBridge struct {
	server *Server
	sess   *session
	runID  string

	mu       sync.Mutex
	toolMeta map[string]toolMeta // tool call ID -> presented metadata
}

// toolMeta is the metadata presented on the initial tool_call update so
// tool_call_update notifications can stay partial.
type toolMeta struct {
	title string
	kind  acp.ToolKind
}

func newEventBridge(server *Server, sess *session, runID string) *eventBridge {
	return &eventBridge{
		server:   server,
		sess:     sess,
		runID:    runID,
		toolMeta: make(map[string]toolMeta),
	}
}

// OnRunEvent implements lebro.RunListener. It never blocks the run on
// protocol errors beyond recording them as diagnostics.
func (b *eventBridge) OnRunEvent(event lebro.RunEvent) {
	if string(event.RunID) != b.runID {
		return
	}
	switch event.Type {
	case lebro.RunEventDelta:
		b.delta(event)
	case lebro.RunEventToolRequested:
		b.toolRequested(event)
	case lebro.RunEventToolStarted:
		b.toolStarted(event)
	case lebro.RunEventToolFinished:
		b.toolFinished(event)
	case lebro.RunEventSuspended:
		// The approval bridge sends the permission request itself.
	default:
		// run_started, model_started/finished, processor, terminal events:
		// no ACP representation. Terminal outcomes surface through the
		// PromptResponse stop reason.
	}
}

// send delivers one update; failures are diagnostics, not run failures.
func (b *eventBridge) send(update acp.SessionUpdate) {
	conn := b.server.conn()
	if conn == nil {
		return
	}
	err := conn.SessionUpdate(context.Background(), acp.SessionNotification{
		SessionId: acp.SessionId(b.sess.id),
		Update:    update,
	})
	if err != nil {
		b.server.logger.Debug("acp: session update failed", "session", b.sess.id, "error", err)
	}
}

// delta forwards model content. Parts are canonical when present: each part
// becomes its own chunk in order, so text and reasoning never merge.
func (b *eventBridge) delta(event lebro.RunEvent) {
	parts := event.DeltaParts
	if len(parts) == 0 {
		// Fall back to the flat fields for non-streaming or legacy shapes.
		if event.DeltaText != "" {
			parts = append(parts, lebro.StreamContentPart{Kind: lebro.StreamContentPartText, Text: event.DeltaText})
		}
		if !event.DeltaReasoning.IsZero() {
			parts = append(parts, lebro.StreamContentPart{Kind: lebro.StreamContentPartReasoning, Text: event.DeltaReasoning.Text})
		}
	}
	for _, part := range parts {
		switch part.Kind {
		case lebro.StreamContentPartText:
			if part.Text == "" {
				continue
			}
			b.send(acp.UpdateAgentMessageText(part.Text))
		case lebro.StreamContentPartReasoning:
			if part.Text == "" {
				continue
			}
			b.send(acp.UpdateAgentThoughtText(part.Text))
		default:
			// Unknown displayable channels have no ACP mapping; skip.
		}
	}
	// Tool-call deltas are carried by the tool_requested/tool_started/
	// tool_finished events, not by delta parts.
	_ = event.ToolCallID
}

// toolRequested presents a new tool call. Called before approval evaluation,
// so the client sees the pending call whether or not permission is required.
func (b *eventBridge) toolRequested(event lebro.RunEvent) {
	if event.ToolCallID == "" {
		return
	}
	meta := toolMeta{
		title: toolTitle(event.ToolID, event.ToolCallID),
		kind:  toolKind(event.ToolID),
	}
	b.mu.Lock()
	b.toolMeta[event.ToolCallID] = meta
	b.mu.Unlock()
	b.send(acp.StartToolCall(
		acp.ToolCallId(event.ToolCallID),
		meta.title,
		acp.WithStartKind(meta.kind),
		acp.WithStartStatus(acp.ToolCallStatusPending),
	))
}

// toolStarted marks an approved call in progress.
func (b *eventBridge) toolStarted(event lebro.RunEvent) {
	if event.ToolCallID == "" {
		return
	}
	b.send(acp.UpdateToolCall(
		acp.ToolCallId(event.ToolCallID),
		acp.WithUpdateStatus(acp.ToolCallStatusInProgress),
	))
}

// toolFinished marks the call completed or failed. Result payloads are not
// forwarded: the observer policy governs payload exposure, and the ACP client
// already sees the tool's effect through its own UI surface.
func (b *eventBridge) toolFinished(event lebro.RunEvent) {
	if event.ToolCallID == "" {
		return
	}
	var status acp.ToolCallStatus
	switch event.ToolState {
	case lebro.ToolExecutionSucceeded:
		status = acp.ToolCallStatusCompleted
	case lebro.ToolExecutionCancelled:
		status = acp.ToolCallStatusFailed
	default:
		status = acp.ToolCallStatusFailed
	}
	b.send(acp.UpdateToolCall(
		acp.ToolCallId(event.ToolCallID),
		acp.WithUpdateStatus(status),
	))
}

// toolTitle renders a human title when the runtime gives us only IDs.
func toolTitle(toolID lebro.ToolID, callID string) string {
	name := string(toolID)
	if name == "" {
		name = callID
	}
	return fmt.Sprintf("Running %s", name)
}

// toolKind maps a lebro tool to a coarse ACP presentation category. Lebro has
// no kind metadata, so everything maps to "other" rather than guessing.
func toolKind(lebro.ToolID) acp.ToolKind {
	return acp.ToolKindOther
}
