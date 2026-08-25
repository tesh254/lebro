package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/tesh254/lebro"
)

// aiSDKStreamVersion is intentionally private: protocol selection belongs to
// the HTTP boundary, not to Agent or its provider-neutral stream contract.
type aiSDKStreamVersion string

const (
	aiSDKStreamV4 aiSDKStreamVersion = "v4"
	aiSDKStreamV5 aiSDKStreamVersion = "v5"
)

// handleAgentAISDKStream is an opt-in adapter for AI SDK clients. It does not
// share framing with handleAgentStream, preserving the native SSE contract.
func (s *Server) handleAgentAISDKStream(w http.ResponseWriter, r *http.Request) {
	agent, ok := s.lookupAgent(r.PathValue("id"))
	if !ok {
		writeError(w, r, ErrorCodeNotFound)
		return
	}
	version, ok := aiSDKVersion(r.URL.Query().Get("version"))
	if !ok {
		writeError(w, r, ErrorCodeInvalidRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
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
	defer run.Cancel()

	if version == aiSDKStreamV4 {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/event-stream")
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	writer := newAISDKStreamWriter(w, flusher, version)
	if !writer.start() {
		run.Cancel()
		for range run.Deltas {
		}
		_, _ = run.Wait()
		return
	}

	var total lebro.ModelUsage
	for delta := range run.Deltas {
		accumulateUsage(&total, delta.Usage)
		if !writer.delta(s.config.Redactor(delta)) {
			run.Cancel()
			for range run.Deltas {
			}
			_, _ = run.Wait()
			return
		}
	}
	result, runErr := run.Wait()
	writer.terminal(result, runErr, total)
}

func aiSDKVersion(raw string) (aiSDKStreamVersion, bool) {
	switch aiSDKStreamVersion(raw) {
	case aiSDKStreamV4, aiSDKStreamV5:
		return aiSDKStreamVersion(raw), true
	default:
		return "", false
	}
}

type aiSDKStreamWriter struct {
	w            http.ResponseWriter
	flusher      http.Flusher
	version      aiSDKStreamVersion
	textID       string
	finishReason lebro.FinishReason
}

func newAISDKStreamWriter(w http.ResponseWriter, flusher http.Flusher, version aiSDKStreamVersion) *aiSDKStreamWriter {
	return &aiSDKStreamWriter{w: w, flusher: flusher, version: version, textID: "text-0"}
}

func (w *aiSDKStreamWriter) start() bool {
	if w.version == aiSDKStreamV4 {
		return true
	}
	return w.writeV5(map[string]any{"type": "start"}) &&
		w.writeV5(map[string]any{"type": "start-step"}) &&
		w.writeV5(map[string]any{"type": "text-start", "id": w.textID})
}

func (w *aiSDKStreamWriter) delta(delta lebro.StreamDelta) bool {
	if delta.Text != "" && !w.text(delta.Text) {
		return false
	}
	if delta.Reasoning.Text != "" && !w.reasoning(delta.Reasoning.Text) {
		return false
	}
	if delta.ToolCall != nil && !w.toolCall(*delta.ToolCall) {
		return false
	}
	if delta.StructuredOutput != "" && !w.structuredOutput(delta.StructuredOutput.Raw()) {
		return false
	}
	if delta.FinishReason != "" {
		w.finishReason = delta.FinishReason
	}
	return true
}

// reasoning sends displayable reasoning text without opaque provider details.
// The data part is valid in both supported AI SDK protocols and avoids leaking
// replay-only signatures into browser clients.
func (w *aiSDKStreamWriter) reasoning(text string) bool {
	if w.version == aiSDKStreamV4 {
		return w.writeV4JSON("2", []any{map[string]string{"reasoning": text}})
	}
	return w.writeV5(map[string]any{"type": "data-lebro-reasoning", "data": text})
}

func (w *aiSDKStreamWriter) text(text string) bool {
	if w.version == aiSDKStreamV4 {
		return w.writeV4("0", text)
	}
	return w.writeV5(map[string]any{"type": "text-delta", "id": w.textID, "delta": text})
}

func (w *aiSDKStreamWriter) toolCall(call lebro.ModelToolCall) bool {
	input := aiSDKToolInput(call.Arguments)
	if w.version == aiSDKStreamV4 {
		return w.writeV4JSON("9", map[string]any{"toolCallId": call.ID, "toolName": call.ToolID, "args": input})
	}
	return w.writeV5(map[string]any{"type": "tool-input-start", "toolCallId": call.ID, "toolName": call.ToolID}) &&
		w.writeV5(map[string]any{"type": "tool-input-available", "toolCallId": call.ID, "toolName": call.ToolID, "input": input})
}

func aiSDKToolInput(raw json.RawMessage) any {
	if len(raw) != 0 && json.Valid(raw) {
		return raw
	}
	// The default Redactor removes arguments. Both protocol versions still need
	// an object-shaped tool input, so preserve call identity without leaking it.
	return map[string]any{}
}

func (w *aiSDKStreamWriter) structuredOutput(value json.RawMessage) bool {
	if w.version == aiSDKStreamV4 {
		return w.writeV4JSON("2", []json.RawMessage{value})
	}
	return w.writeV5(map[string]any{"type": "data-lebro-structured-output", "data": value})
}

func (w *aiSDKStreamWriter) terminal(result lebro.RunResult, runErr error, total lebro.ModelUsage) bool {
	if runErr != nil || result.Status == lebro.RunStatusFailed || result.Status == lebro.RunStatusCancelled {
		code := ErrorCodeInternal
		if runErr != nil {
			code = classify(runErr)
		} else if result.Status == lebro.RunStatusCancelled {
			code = ErrorCodeCancelled
		}
		if total != (lebro.ModelUsage{}) && !w.usage(total) {
			return false
		}
		if w.version == aiSDKStreamV4 {
			return w.writeV4("3", errorBody(code).Message)
		}
		return w.writeV5(map[string]any{"type": "error", "errorText": errorBody(code).Message})
	}
	if w.version == aiSDKStreamV4 {
		return w.writeV4JSON("d", map[string]any{"finishReason": w.aiSDKFinishReason(), "usage": aiSDKV4Usage(total)})
	}
	return w.usage(total) &&
		w.writeV5(map[string]any{"type": "data-lebro-finish-reason", "data": w.aiSDKFinishReason()}) &&
		w.writeV5(map[string]any{"type": "text-end", "id": w.textID}) &&
		w.writeV5(map[string]any{"type": "finish-step"}) &&
		w.writeV5(map[string]any{"type": "finish"})
}

func (w *aiSDKStreamWriter) usage(total lebro.ModelUsage) bool {
	if w.version == aiSDKStreamV4 {
		return w.writeV4JSON("2", []any{map[string]any{"usage": aiSDKV4Usage(total)}})
	}
	return w.writeV5(map[string]any{"type": "data-lebro-usage", "data": aiSDKV5Usage(total)})
}

func (w *aiSDKStreamWriter) aiSDKFinishReason() string {
	switch w.finishReason {
	case lebro.FinishReasonLength:
		return "length"
	case lebro.FinishReasonContent:
		return "content-filter"
	case lebro.FinishReasonCancelled:
		return "other"
	case lebro.FinishReasonUnspecified:
		return "other"
	case lebro.FinishReasonToolCalls:
		// A tool-call finish reason is intermediate in a successful agent run.
		// The final model step supplies the terminal reason, usually stop.
		return "stop"
	default:
		return "stop"
	}
}

func aiSDKV4Usage(usage lebro.ModelUsage) map[string]int64 {
	result := map[string]int64{"promptTokens": usage.InputTokens, "completionTokens": usage.OutputTokens}
	if usage.ReasoningTokens != 0 {
		result["reasoningTokens"] = usage.ReasoningTokens
	}
	return result
}

func aiSDKV5Usage(usage lebro.ModelUsage) map[string]int64 {
	result := map[string]int64{"inputTokens": usage.InputTokens, "outputTokens": usage.OutputTokens, "totalTokens": usage.TotalTokens}
	if usage.ReasoningTokens != 0 {
		result["reasoningTokens"] = usage.ReasoningTokens
	}
	return result
}

func (w *aiSDKStreamWriter) writeV4(kind, value string) bool {
	encoded, err := json.Marshal(value)
	if err != nil {
		return false
	}
	return w.write([]byte(kind + ":" + string(encoded) + "\n"))
}

func (w *aiSDKStreamWriter) writeV4JSON(kind string, value any) bool {
	encoded, err := json.Marshal(value)
	if err != nil {
		return false
	}
	return w.write([]byte(kind + ":" + string(encoded) + "\n"))
}

func (w *aiSDKStreamWriter) writeV5(value any) bool {
	encoded, err := json.Marshal(value)
	if err != nil {
		return false
	}
	return w.write(append([]byte("data: "), append(encoded, '\n', '\n')...))
}

func (w *aiSDKStreamWriter) write(frame []byte) bool {
	if _, err := w.w.Write(frame); err != nil {
		return false
	}
	w.flusher.Flush()
	return true
}
