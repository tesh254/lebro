package lebro

import "testing"

func TestModelContractSupportsToolAwareTurns(t *testing.T) {
	t.Parallel()

	request := ModelRequest{
		Model: "example/model",
		Tools: []ToolDefinition{{
			ID:          "lookup",
			Description: "Looks up a record.",
		}},
	}

	if got := request.Tools[0].ID; got != "lookup" {
		t.Fatalf("tool ID = %q, want lookup", got)
	}

	response := ModelResponse{FinishReason: FinishReasonToolCalls}
	if response.FinishReason != FinishReasonToolCalls {
		t.Fatalf("finish reason = %q, want %q", response.FinishReason, FinishReasonToolCalls)
	}
}
