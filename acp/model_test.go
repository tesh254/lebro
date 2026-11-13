package acp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tesh254/lebro"
)

// streamingFixtureModel is a fixture model that streams its responses as
// deltas. The first step can optionally gate inside the stream, so tests can
// hold a turn open after updates have already flowed.
type streamingFixtureModel struct {
	mu    sync.Mutex
	steps []lebro.ModelResponse
	calls int
	// gate, when set, blocks after the first delta of the first call.
	gate    chan struct{}
	blocked bool
	// onCall, when set, observes each model call for debugging.
	onCall   func(call int, request lebro.ModelRequest)
	scripted bool
}

func newStreamingModel(steps ...lebro.ModelResponse) *streamingFixtureModel {
	return &streamingFixtureModel{steps: steps}
}

func (m *streamingFixtureModel) Model() string { return "acp-fixture" }

// Generate falls back to a single complete response.
func (m *streamingFixtureModel) Generate(ctx context.Context, request lebro.ModelRequest) (lebro.ModelResponse, error) {
	if m.scripted {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.calls >= len(m.steps) {
			return lebro.ModelResponse{}, fmt.Errorf("acp test: fixture model exhausted after %d calls", m.calls)
		}
		response := m.steps[m.calls]
		m.calls++
		return response, nil
	}
	pieces, err := m.streamAll(ctx, request)
	if err != nil {
		return lebro.ModelResponse{}, err
	}
	return pieces[len(pieces)-1].Result, nil
}

// Stream implements lebro.StreamingModel.
func (m *streamingFixtureModel) Stream(ctx context.Context, request lebro.ModelRequest) (lebro.StreamReader, error) {
	m.mu.Lock()
	call := m.calls
	gate := m.gate
	if gate != nil && call == 0 && !m.blocked {
		m.blocked = true
	}
	m.mu.Unlock()
	pieces, err := m.streamAll(ctx, request)
	if err != nil {
		return nil, err
	}
	reader := &lebro.StreamReaderFunc{
		NextFn: func() (lebro.StreamDelta, error) {
			if len(pieces) == 0 {
				return lebro.StreamDelta{}, fmt.Errorf("acp test: empty stream")
			}
			// After the first piece has been delivered, hold the stream on
			// the gate so tests observe a live turn before cancel/overlap.
			if gate != nil && call == 0 && len(pieces) == 1 {
				select {
				case <-gate:
				case <-ctx.Done():
					return lebro.StreamDelta{}, ctx.Err()
				}
			}
			piece := pieces[0]
			pieces = pieces[1:]
			delta := lebro.StreamDelta{Parts: piece.Parts}
			// Surface whole tool calls as ToolCall deltas.
			if calls := piece.Result.Message.ToolCalls.Values(); len(calls) > 0 {
				call := calls[0]
				delta.ToolCall = &call
			}
			if len(pieces) == 0 {
				// Final piece carries the terminal payload.
				delta.FinishReason = piece.Result.FinishReason
				delta.Usage = piece.Result.Usage
			}
			return delta, nil
		},
	}
	return reader, nil
}

type streamPiece struct {
	Parts  []lebro.StreamContentPart
	Result lebro.ModelResponse
}

// streamAll renders the scripted response as ordered delta pieces.
//
// The model is content-driven rather than call-count-driven so concurrent
// sessions behave independently: when the transcript's last user message
// contains "call", the model requests the protected tool; otherwise it
// echoes the transcript size. The optional steps slice, when non-empty, is
// consumed by single-session tests in order.
func (m *streamingFixtureModel) streamAll(_ context.Context, request lebro.ModelRequest) ([]streamPiece, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.onCall != nil {
		m.onCall(m.calls, request)
	}

	var response lebro.ModelResponse
	if len(m.steps) > 0 {
		if m.calls >= len(m.steps) {
			return nil, fmt.Errorf("acp test: fixture model exhausted after %d calls", m.calls)
		}
		response = m.steps[m.calls]
		m.calls++
		// Later calls echo the transcript size so multi-turn context is
		// visible, preserving any scripted tool calls alongside the echo.
		if m.calls > 1 {
			calls := response.Message.ToolCalls
			response.Message = lebro.Message{
				Role:      lebro.RoleAssistant,
				Content:   fmt.Sprintf("saw %d prior messages", len(request.Messages)-1),
				ToolCalls: calls,
			}
		}
	} else {
		// Content-driven: the newest user message decides the behavior. On
		// continuation calls the newest user message predates tool traffic,
		// so track whether this call already produced a tool call for the
		// same user turn via the calls counter per session thread.
		last := ""
		for i := len(request.Messages) - 1; i >= 0; i-- {
			if request.Messages[i].Role == lebro.RoleUser {
				last = request.Messages[i].Content
				break
			}
		}
		sawToolResult := false
		for _, msg := range request.Messages {
			if msg.Role == lebro.RoleTool {
				sawToolResult = true
				break
			}
		}
		switch {
		case strings.Contains(last, "call") && !sawToolResult:
			response = toolCallResponse()
		default:
			response = lebro.ModelResponse{
				Message: lebro.Message{
					Role:    lebro.RoleAssistant,
					Content: fmt.Sprintf("saw %d prior messages", len(request.Messages)-1),
				},
				FinishReason: lebro.FinishReasonStop,
			}
		}
	}

	var pieces []streamPiece
	content := response.Message.Content
	calls := response.Message.ToolCalls.Values()
	switch {
	case content != "":
		// Split the text into two chunks to exercise ordered streaming.
		half := len(content) / 2
		if half == 0 {
			half = len(content)
		}
		pieces = append(pieces, streamPiece{
			Parts: []lebro.StreamContentPart{{Kind: lebro.StreamContentPartText, Text: content[:half]}},
		})
		pieces = append(pieces, streamPiece{
			Parts:  []lebro.StreamContentPart{{Kind: lebro.StreamContentPartText, Text: content[half:]}},
			Result: lebro.ModelResponse{Message: lebro.Message{Role: lebro.RoleAssistant, Content: content}, FinishReason: lebro.FinishReasonStop},
		})
		// A response may pair text with tool calls: emit the whole-call
		// deltas after the text so mixed responses keep their order, then
		// the terminal delta.
		for _, call := range calls {
			calls := mustToolCalls(call)
			pieces = append(pieces, streamPiece{
				Result: lebro.ModelResponse{Message: lebro.Message{Role: lebro.RoleAssistant, ToolCalls: calls}, FinishReason: lebro.FinishReasonToolCalls},
			})
		}
		if len(calls) > 0 {
			pieces = append(pieces, streamPiece{
				Result: lebro.ModelResponse{Message: lebro.Message{Role: lebro.RoleAssistant}, FinishReason: lebro.FinishReasonToolCalls},
			})
		}
	case len(calls) > 0:
		// One whole-call delta per tool call, then a terminal delta carrying
		// the finish reason. The terminal delta has no parts so the runtime
		// validates the aggregated response cleanly.
		for _, call := range calls {
			calls := mustToolCalls(call)
			pieces = append(pieces, streamPiece{
				Result: lebro.ModelResponse{Message: lebro.Message{Role: lebro.RoleAssistant, ToolCalls: calls}, FinishReason: lebro.FinishReasonToolCalls},
			})
		}
		pieces = append(pieces, streamPiece{
			Result: lebro.ModelResponse{Message: lebro.Message{Role: lebro.RoleAssistant}, FinishReason: lebro.FinishReasonToolCalls},
		})
	default:
		pieces = append(pieces, streamPiece{Result: response})
	}
	return pieces, nil
}

// mustToolCalls wraps one call into ModelToolCalls.
func mustToolCalls(call lebro.ModelToolCall) lebro.ModelToolCalls {
	calls, err := lebro.NewModelToolCalls(call)
	if err != nil {
		panic(err)
	}
	return calls
}

// toolCallResponse builds a response that requests the protected tool.
func toolCallResponse() lebro.ModelResponse {
	calls, err := lebro.NewModelToolCalls(lebro.ModelToolCall{
		ID:        "call-protected-1",
		ToolID:    "acp_test.protected",
		Arguments: json.RawMessage(`{"value":"reviewed"}`),
	})
	if err != nil {
		panic(err)
	}
	return lebro.ModelResponse{
		Message:      lebro.Message{Role: lebro.RoleAssistant, ToolCalls: calls},
		FinishReason: lebro.FinishReasonToolCalls,
	}
}

// Compile-time checks.
var (
	_ lebro.StreamingModel = (*streamingFixtureModel)(nil)
	_ lebro.Model          = (*streamingFixtureModel)(nil)
)

// unused keeps time imported for future timing-sensitive helpers.
var _ = time.Now
