package testkit

import (
	"testing"

	"github.com/tesh254/lebro"
)

func TestScriptedModelPassesProviderContract(t *testing.T) {
	t.Parallel()
	RunProviderContract(t, func(_ *testing.T, contractCase ProviderCase) ProviderHarness {
		model := (*Model)(nil)
		switch contractCase.Mode {
		case ContractResponse:
			model = NewModel(Response(contractCase.Response))
		case ContractFailure:
			model = NewModel(Failure(&lebro.ModelError{Kind: contractCase.ErrorKind, Message: "contract provider failure"}))
		case ContractCancellation:
			model = NewModel(Text("unused"))
		default:
			panic("unknown contract mode")
		}
		return ProviderHarness{
			Model: model,
			ObservedRequest: func() lebro.ModelRequest {
				calls := model.Calls()
				return calls[len(calls)-1].Request
			},
		}
	})
}

func TestProviderContractCasesAreDefensiveCopies(t *testing.T) {
	t.Parallel()
	cases := ProviderContractCases()
	cases[0].Request.Messages[0].Content = "mutated"
	cases[1].Request.Tools[0].InputSchema[0] = '['
	cases[1].Response.Message.ToolCalls = lebro.ModelToolCalls{}
	cases[0].Response.Message.Content = "mutated"
	cases[2].Request.OutputSchema.Schema[0] = '['
	cases[2].Request.Extension[0] = '['
	cases[2].Response.Message.StructuredOutput = "{"
	cases[2].Response.Extension[0] = '['

	fresh := ProviderContractCases()
	if fresh[0].Request.Messages[0].Content != "hello" || fresh[0].Response.Message.Content != "hello back" {
		t.Fatalf("message or response fixture was mutated: %#v", fresh[0])
	}
	if got := string(fresh[1].Request.Tools[0].InputSchema); got != `{"type":"object"}` {
		t.Fatalf("tool schema was mutated: %s", got)
	}
	freshCalls := fresh[1].Response.Message.ToolCalls.Values()
	if got := string(freshCalls[0].Arguments); got != `{"id":"42"}` {
		t.Fatalf("response tool call was mutated: %s", got)
	}
	if got := string(fresh[2].Request.OutputSchema.Schema); got != `{"type":"object"}` {
		t.Fatalf("output schema was mutated: %s", got)
	}
	if got := string(fresh[2].Response.Message.StructuredOutput.Raw()); got != `{"ok":true}` {
		t.Fatalf("structured response was mutated: %s", got)
	}
	if got := contractToolCalls(lebro.ModelToolCall{Arguments: []byte(`{`)}); !got.IsZero() {
		t.Fatalf("invalid contract tool calls = %#v", got)
	}
}
