package testkit

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tesh254/lebro"
)

// ContractMode tells a provider factory what behavior to arrange.
type ContractMode string

const (
	ContractResponse     ContractMode = "response"
	ContractFailure      ContractMode = "failure"
	ContractCancellation ContractMode = "cancellation"
)

// ProviderCase is one adapter-neutral provider behavior.
type ProviderCase struct {
	Name      string
	Mode      ContractMode
	Request   lebro.ModelRequest
	Response  lebro.ModelResponse
	ErrorKind lebro.ModelErrorKind
}

// ProviderHarness couples an adapter with an observation of the neutral request
// that reached its provider boundary.
type ProviderHarness struct {
	Model           lebro.Model
	ObservedRequest func() lebro.ModelRequest
}

// ProviderFactory builds a harness configured for one contract case. A real
// adapter can translate the case into a recorded HTTP exchange and report the
// neutral equivalent observed at that boundary.
type ProviderFactory func(*testing.T, ProviderCase) ProviderHarness

// ProviderContractCases returns defensive copies of the canonical cases.
func ProviderContractCases() []ProviderCase {
	cases := []ProviderCase{
		{
			Name:    "text",
			Mode:    ContractResponse,
			Request: lebro.ModelRequest{Model: "contract-model", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hello"}}},
			Response: lebro.ModelResponse{
				Message:      lebro.Message{Role: lebro.RoleAssistant, Content: "hello back"},
				Usage:        lebro.ModelUsage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
				FinishReason: lebro.FinishReasonStop,
			},
		},
		{
			Name: "tool call",
			Mode: ContractResponse,
			Request: lebro.ModelRequest{
				Model:    "contract-model",
				Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "look it up"}},
				Tools:    []lebro.ToolDefinition{{ID: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}},
			},
			Response: lebro.ModelResponse{
				Message: lebro.Message{Role: lebro.RoleAssistant, ToolCalls: contractToolCalls(lebro.ModelToolCall{
					ID: "contract-call-1", ToolID: "lookup", Arguments: json.RawMessage(`{"id":"42"}`),
				})},
				FinishReason: lebro.FinishReasonToolCalls,
			},
		},
		{
			Name: "structured output",
			Mode: ContractResponse,
			Request: lebro.ModelRequest{
				Model: "contract-model", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "return JSON"}},
				OutputSchema: &lebro.ModelOutputSchema{Name: "result", Schema: json.RawMessage(`{"type":"object"}`), Strict: true},
				Extension:    json.RawMessage(`{"seed":7}`),
			},
			Response: lebro.ModelResponse{
				Message:      lebro.Message{Role: lebro.RoleAssistant, StructuredOutput: lebro.NewModelStructuredOutput(json.RawMessage(`{"ok":true}`))},
				FinishReason: lebro.FinishReasonStop,
				Extension:    json.RawMessage(`{"request_id":"req-1"}`),
			},
		},
		{
			Name:      "failure",
			Mode:      ContractFailure,
			Request:   lebro.ModelRequest{Model: "contract-model", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "fail"}}},
			ErrorKind: lebro.ModelErrorUnavailable,
		},
		{
			Name:    "cancellation",
			Mode:    ContractCancellation,
			Request: lebro.ModelRequest{Model: "contract-model", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "cancel"}}},
		},
	}
	return cloneProviderCases(cases)
}

// RunProviderContract runs the canonical behavior suite against factory.
func RunProviderContract(t *testing.T, factory ProviderFactory) {
	t.Helper()
	for _, contractCase := range ProviderContractCases() {
		contractCase := contractCase
		t.Run(contractCase.Name, func(t *testing.T) {
			assertNoError(t, contractCase.Request.Validate())
			harness := factory(t, cloneProviderCase(contractCase))
			ctx := context.Background()
			if contractCase.Mode == ContractCancellation {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}

			response, err := harness.Model.Generate(ctx, cloneRequest(contractCase.Request))
			switch contractCase.Mode {
			case ContractResponse:
				assertNoError(t, err)
				assertNoError(t, response.Validate())
				assertResponse(t, response, contractCase.Response)
				assertObservedRequest(t, harness.ObservedRequest, contractCase.Request)
			case ContractFailure:
				assertModelError(t, err, contractCase.ErrorKind)
			case ContractCancellation:
				AssertCancellation(t, err)
			}
		})
	}
}

func cloneProviderCases(cases []ProviderCase) []ProviderCase {
	result := make([]ProviderCase, len(cases))
	for i, contractCase := range cases {
		result[i] = cloneProviderCase(contractCase)
	}
	return result
}

func cloneProviderCase(contractCase ProviderCase) ProviderCase {
	contractCase.Request = cloneRequest(contractCase.Request)
	contractCase.Response = cloneResponse(contractCase.Response)
	return contractCase
}

func contractToolCalls(calls ...lebro.ModelToolCall) lebro.ModelToolCalls {
	encoded, err := lebro.NewModelToolCalls(calls...)
	if err != nil {
		return lebro.ModelToolCalls{}
	}
	return encoded
}
