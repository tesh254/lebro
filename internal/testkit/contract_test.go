package testkit

import (
	"errors"
	"testing"

	"github.com/tesh254/lebro"
)

func TestScriptedModelPassesProviderContract(t *testing.T) {
	t.Parallel()
	RunProviderContract(t, func(_ *testing.T, contractCase ProviderCase) lebro.Model {
		switch contractCase.Mode {
		case ContractResponse:
			return NewModel(Response(contractCase.Response))
		case ContractFailure:
			return NewModel(Failure(errors.New("contract provider failure")))
		case ContractCancellation:
			return NewModel(Text("unused"))
		default:
			panic("unknown contract mode")
		}
	})
}

func TestProviderContractCasesAreDefensiveCopies(t *testing.T) {
	t.Parallel()
	cases := ProviderContractCases()
	cases[0].Request.Messages[0].Content = "mutated"
	cases[1].Request.Tools[0].InputSchema[0] = '['
	cases[0].Response.Message.Content = "mutated"

	fresh := ProviderContractCases()
	if fresh[0].Request.Messages[0].Content != "hello" || fresh[0].Response.Message.Content != "hello back" {
		t.Fatalf("message or response fixture was mutated: %#v", fresh[0])
	}
	if got := string(fresh[1].Request.Tools[0].InputSchema); got != `{"type":"object"}` {
		t.Fatalf("tool schema was mutated: %s", got)
	}
}
