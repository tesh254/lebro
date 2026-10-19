// The decision-compat example shows that a decision surface sits beside the
// generative model protocol instead of changing it. The generative client
// keeps its existing interface, request, and response; the decision model is
// an additional, separate contract that the application chooses to use.
//
// Both models are in-file stand-ins, so the example runs without a network
// call or an API key.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/tesh254/lebro"
)

func main() {
	must(run(os.Stdout))
}

func run(output io.Writer) error {
	// The generative client is unchanged: the same interface, the same
	// request, the same response.
	generator := lebro.Model(&fixtureModel{reply: "Your refund is on the way."})
	response, err := generator.Generate(context.Background(), lebro.ModelRequest{
		Model:    "fixture-model",
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "Where is my refund?"}},
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "assistant: %s\n", response.Message.Content)
	fmt.Fprintf(output, "finish: %s\n", response.FinishReason)

	// The decision model is a separate contract with its own interface.
	var decision lebro.DecisionModel = fixtureDecisionModel{}
	decisionResponse, err := decision.Decide(context.Background(), lebro.DecisionRequest{
		State: response.Message.Content,
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"sentiment": lebro.NoulQuestion{
				Instructions: "Is the customer satisfied?",
			},
		},
	})
	if err != nil {
		return err
	}
	answer, ok := decisionResponse.Answers["sentiment"].(lebro.NoulAnswer)
	if !ok {
		return fmt.Errorf("expected a noul answer, got %T", decisionResponse.Answers["sentiment"])
	}
	fmt.Fprintf(output, "satisfied: %.2f\n", answer.ProbabilityYes)

	// Both contracts compose in one application without either one changing.
	fmt.Fprint(output, "compatibility: the generative client needed no change\n")
	return nil
}

// fixtureModel is a generative model that returns a fixed reply.
type fixtureModel struct {
	reply string
}

func (m *fixtureModel) Generate(_ context.Context, _ lebro.ModelRequest) (lebro.ModelResponse, error) {
	return lebro.ModelResponse{
		Message:      lebro.Message{Role: lebro.RoleAssistant, Content: m.reply},
		FinishReason: lebro.FinishReasonStop,
		Usage:        lebro.ModelUsage{InputTokens: 12, OutputTokens: 6, TotalTokens: 18},
	}, nil
}

// fixtureDecisionModel answers a noul question with a fixed probability.
type fixtureDecisionModel struct{}

func (fixtureDecisionModel) ProviderID() lebro.ProviderID { return "example" }

func (fixtureDecisionModel) Capabilities() lebro.DecisionCapabilities {
	return lebro.DecisionCapabilities{Noul: true}
}

// CapabilitiesFor reports the capabilities of the fixture's only model.
func (m fixtureDecisionModel) CapabilitiesFor(model string) (lebro.DecisionCapabilities, bool) {
	if model != "" && model != "example-model" {
		return lebro.DecisionCapabilities{}, false
	}
	return m.Capabilities(), true
}

func (m fixtureDecisionModel) Decide(_ context.Context, request lebro.DecisionRequest) (lebro.DecisionResponse, error) {
	answers := make(map[lebro.DecisionQuestionID]lebro.DecisionAnswer, len(request.Questions))
	for id := range request.Questions {
		answers[id] = lebro.NoulAnswer{ProbabilityYes: 0.88}
	}
	return lebro.DecisionResponse{
		Provider: m.ProviderID(),
		Model:    "example-model",
		Answers:  answers,
		Attempts: 1,
	}, nil
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
