package testkit

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tesh254/lebro"
)

// ProcessorDecisionContractSuite keeps processor decisions typed and normalized.
func ProcessorDecisionContractSuite(t *testing.T) {
	t.Helper()
	for _, item := range []struct {
		name string
		in   lebro.ProcessorDecision
		want lebro.ProcessorDecisionKind
	}{
		{"zero value allows", lebro.ProcessorDecision{}, lebro.ProcessorAllow},
		{"explicit allow", lebro.ProcessorDecision{Kind: lebro.ProcessorAllow}, lebro.ProcessorAllow},
		{"transform", lebro.ProcessorDecision{Kind: lebro.ProcessorTransform}, lebro.ProcessorTransform},
		{"block", lebro.ProcessorDecision{Kind: lebro.ProcessorBlock}, lebro.ProcessorBlock},
	} {
		t.Run(item.name, func(t *testing.T) {
			got, err := lebro.NormalizeProcessorDecision(item.in)
			if err != nil || got.Kind != item.want {
				t.Fatalf("NormalizeProcessorDecision(%#v) = %#v, %v", item.in, got, err)
			}
		})
	}
	if _, err := lebro.NormalizeProcessorDecision(lebro.ProcessorDecision{Kind: "unknown"}); !errors.Is(err, lebro.ErrProcessorInvalidDecision) {
		t.Fatalf("unknown decision error = %v, want ErrProcessorInvalidDecision", err)
	}
}

// NetworkPathContractSuite verifies bounded multi-hop routing carries the prior
// specialist handoff into the next specialist.
func NetworkPathContractSuite(t *testing.T) {
	t.Helper()
	var secondInput lebro.RunInput
	network, err := lebro.NewNetwork(lebro.NetworkConfig{
		Definition: lebro.WorkflowDefinition{ID: "network-contract"},
		Router: contractRouter(func(_ context.Context, request lebro.RoutingRequest) (lebro.RoutingDecision, error) {
			switch request.Hops {
			case 0:
				return lebro.RoutingDecision{SpecialistID: "research"}, nil
			case 1:
				return lebro.RoutingDecision{SpecialistID: "writer"}, nil
			default:
				return lebro.RoutingDecision{Complete: true}, nil
			}
		}),
		Specialists: []lebro.NetworkSpecialist{
			{ID: "research", Workflow: contractWorkflow{id: "research", output: "facts"}},
			{ID: "writer", Workflow: contractWorkflow{id: "writer", output: "answer", capture: func(input lebro.RunInput) { secondInput = input }}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := network.Run(context.Background(), lebro.RunInput{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "question"}}})
	if err != nil || result.Status != lebro.RunStatusSucceeded || result.Messages[len(result.Messages)-1].Content != "answer" {
		t.Fatalf("network result = %#v, %v", result, err)
	}
	if len(secondInput.Messages) != 1 || !strings.Contains(secondInput.Messages[0].Content, "Previous specialist output:\nfacts") {
		t.Fatalf("second specialist input = %#v", secondInput)
	}
}

// AISDKStreamContractCases centralizes stable protocol-selection expectations.
// HTTP tests own framing assertions because httpapi is intentionally optional.
func AISDKStreamContractCases() []AISDKStreamContractCase {
	return []AISDKStreamContractCase{
		{Version: "v4", ContentType: "text/plain; charset=utf-8"},
		{Version: "v5", ContentType: "text/event-stream"},
	}
}

type AISDKStreamContractCase struct {
	Version     string
	ContentType string
}

type contractRouter func(context.Context, lebro.RoutingRequest) (lebro.RoutingDecision, error)

func (f contractRouter) Route(ctx context.Context, request lebro.RoutingRequest) (lebro.RoutingDecision, error) {
	return f(ctx, request)
}

type contractWorkflow struct {
	id      lebro.WorkflowID
	output  string
	capture func(lebro.RunInput)
}

func (w contractWorkflow) Definition() lebro.WorkflowDefinition {
	return lebro.WorkflowDefinition{ID: w.id}
}
func (w contractWorkflow) Run(_ context.Context, input lebro.RunInput) (lebro.RunResult, error) {
	if w.capture != nil {
		w.capture(input)
	}
	return lebro.RunResult{ID: lebro.RunID(w.id + "-run"), Status: lebro.RunStatusSucceeded, Messages: []lebro.Message{{Role: lebro.RoleAssistant, Content: w.output}}}, nil
}
