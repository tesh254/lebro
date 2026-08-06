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
	ContractResponse         ContractMode = "response"
	ContractStructuredOutput ContractMode = "structured_output"
	ContractFailure          ContractMode = "failure"
	ContractCancellation     ContractMode = "cancellation"
)

// ProviderCase is one adapter-neutral provider behavior.
type ProviderCase struct {
	Name     string
	Mode     ContractMode
	Request  lebro.ModelRequest
	Response lebro.ModelResponse
}

// ProviderFactory builds an adapter configured for one contract case. A real
// adapter can translate the case into a recorded HTTP response; the fake model
// translates it into a Fixture.
type ProviderFactory func(*testing.T, ProviderCase) lebro.Model

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
				Message:      lebro.Message{Role: lebro.RoleAssistant, Name: "lookup", ToolCallID: "contract-call-1", Content: `{"id":"42"}`},
				FinishReason: lebro.FinishReasonToolCalls,
			},
		},
		{
			Name:    "structured output",
			Mode:    ContractStructuredOutput,
			Request: lebro.ModelRequest{Model: "contract-model", ResponseFormat: "json", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "return JSON"}}},
			Response: lebro.ModelResponse{
				Message:      lebro.Message{Role: lebro.RoleAssistant, Content: `{"ok":true}`},
				FinishReason: lebro.FinishReasonStop,
			},
		},
		{
			Name:    "failure",
			Mode:    ContractFailure,
			Request: lebro.ModelRequest{Model: "contract-model", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "fail"}}},
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
			model := factory(t, cloneProviderCase(contractCase))
			ctx := context.Background()
			if contractCase.Mode == ContractCancellation {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}

			response, err := model.Generate(ctx, cloneRequest(contractCase.Request))
			switch contractCase.Mode {
			case ContractResponse:
				assertNoError(t, err)
				assertResponse(t, response, contractCase.Response)
			case ContractStructuredOutput:
				assertNoError(t, err)
				assertValidJSON(t, response.Message.Content)
				assertResponse(t, response, contractCase.Response)
			case ContractFailure:
				assertError(t, err)
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
