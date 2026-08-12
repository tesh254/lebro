package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/tesh254/lebro"
)

// Stream event names. The delta and terminal names reuse the runtime's
// RunEventType vocabulary so a client consuming HTTP events and a listener
// consuming run events describe the same run with the same words.
const (
	eventDelta     = "model_delta"
	eventSucceeded = "run_succeeded"
	eventFailed    = "run_failed"
	eventCancelled = "run_cancelled"
)

// handleAgentStream runs an agent and streams ordered Server-Sent Events.
//
// The stream is terminated by exactly one of run_succeeded, run_failed, or
// run_cancelled, so a client can tell a completed run from a dropped
// connection. Errors discovered before the stream opens are reported as an
// ordinary JSON error response with a non-200 status; once the first byte is
// written the status is committed, so later failures can only be reported as a
// terminal event.
func (s *Server) handleAgentStream(w http.ResponseWriter, r *http.Request) {
	agent, ok := s.lookupAgent(r.PathValue("id"))
	if !ok {
		writeError(w, r, ErrorCodeNotFound)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Without flushing, events would buffer until the handler returned,
		// which is a batch response wearing a stream's content type.
		writeError(w, r, ErrorCodeInternal)
		return
	}

	request, err := decodeJSON[RunRequest](r)
	if err != nil {
		writeError(w, r, ErrorCodeInvalidRequest)
		return
	}

	input, ok := s.runInput(w, r, request)
	if !ok {
		return
	}

	run, err := agent.RunStream(r.Context(), input)
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	// Cancel unconditionally. Wait alone is not enough: a client that
	// disconnects mid-stream leaves this handler returning early, and without
	// Cancel the run goroutine stays parked writing to a channel nobody reads.
	defer run.Cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Proxies that buffer by default would defeat streaming; this is the
	// conventional opt-out and is ignored by proxies that do not honor it.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Drain every delta before calling Wait. Wait blocks until the run
	// goroutine finishes, and that goroutine cannot finish while it is blocked
	// sending a delta, so returning early here would deadlock.
	for delta := range run.Deltas {
		redacted := s.config.Redactor(delta)
		if isZeroDelta(redacted) {
			continue
		}
		if !writeEvent(w, flusher, eventDelta, streamEventFromDelta(redacted)) {
			// The connection is gone. Keep draining so the run goroutine can
			// finish; the deferred Cancel unblocks the provider stream, so the
			// remaining deltas arrive promptly rather than at model speed.
			run.Cancel()
			for range run.Deltas {
			}
			_, _ = run.Wait()
			return
		}
	}

	result, runErr := run.Wait()
	writeTerminalEvent(w, flusher, result, runErr)
}

// writeTerminalEvent writes the single event that closes the stream. A failed
// or cancelled run carries its public error code, so a client sees the same
// classification it would have received from the non-streaming route.
func writeTerminalEvent(w http.ResponseWriter, flusher http.Flusher, result lebro.RunResult, runErr error) {
	event := StreamEvent{
		RunID:  string(result.ID),
		Status: string(result.Status),
	}

	name := eventSucceeded
	switch {
	case runErr != nil:
		code := classify(runErr)
		body := errorBody(code)
		event.Error = &body
		name = eventFailed
		if code == ErrorCodeCancelled {
			name = eventCancelled
		}
	case result.Status == lebro.RunStatusCancelled:
		name = eventCancelled
	case result.Status == lebro.RunStatusFailed:
		name = eventFailed
		body := errorBody(ErrorCodeInternal)
		event.Error = &body
	}

	if runErr == nil {
		response := runResponseFromResult(result)
		event.Text = response.Content
		event.StructuredOutput = response.StructuredOutput
	}

	event.Type = name
	writeEvent(w, flusher, name, event)
}

// streamEventFromDelta projects a redacted delta onto the wire event.
func streamEventFromDelta(delta lebro.StreamDelta) StreamEvent {
	event := StreamEvent{
		Type:         eventDelta,
		Text:         delta.Text,
		FinishReason: string(delta.FinishReason),
	}
	if delta.StructuredOutput != "" {
		event.StructuredOutput = delta.StructuredOutput.Raw()
	}
	if delta.ToolCall != nil {
		event.ToolCall = &ToolCallEvent{
			ID:        delta.ToolCall.ID,
			ToolID:    string(delta.ToolCall.ToolID),
			Arguments: delta.ToolCall.Arguments,
		}
	}
	if delta.Usage != (lebro.ModelUsage{}) {
		usage := usageFromModel(delta.Usage)
		event.Usage = &usage
	}
	return event
}

// isZeroDelta reports whether a redactor suppressed a delta entirely. Err is
// excluded from the comparison because a StreamDelta carrying only an error is
// meaningful, and comparing an interface field would panic on an uncomparable
// dynamic type.
func isZeroDelta(delta lebro.StreamDelta) bool {
	return delta.Text == "" &&
		delta.ToolCall == nil &&
		delta.StructuredOutput == "" &&
		delta.FinishReason == "" &&
		delta.Usage == (lebro.ModelUsage{}) &&
		delta.Err == nil
}

// writeEvent writes one Server-Sent Event and flushes it. It reports false when
// the write fails, which is how a client disconnect surfaces mid-stream.
//
// Data is emitted as a single line: the payload is compact JSON, which cannot
// contain a raw newline, so no multi-line data framing is needed.
func writeEvent(w http.ResponseWriter, flusher http.Flusher, name string, event StreamEvent) bool {
	payload, err := json.Marshal(event)
	if err != nil {
		// A payload that cannot be marshalled would otherwise abort the stream
		// with no terminal event. Report it as an internal failure in the same
		// envelope so the client still sees a well-formed event.
		body := errorBody(ErrorCodeInternal)
		payload, err = json.Marshal(StreamEvent{Type: eventFailed, Error: &body})
		if err != nil {
			return false
		}
		name = eventFailed
	}

	var frame bytes.Buffer
	fmt.Fprintf(&frame, "event: %s\ndata: %s\n\n", name, payload)
	if _, err := w.Write(frame.Bytes()); err != nil {
		return false
	}
	flusher.Flush()
	return true
}
