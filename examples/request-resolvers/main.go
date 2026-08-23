// The request-resolvers example selects instructions and a model per tenant
// without mutating the shared agent definition.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
)

func main() {
	standard := testkit.NewModel(testkit.Text("standard tenant response"))
	premium := testkit.NewModel(testkit.Text("premium tenant response"))
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:           "support",
			Instructions: "Give standard support.",
			Model:        "standard-model",
		},
		Model: standard,
		InstructionsResolver: func(_ context.Context, input lebro.RunInput) (string, error) {
			if input.Metadata["tier"] == "premium" {
				return "Give priority support.", nil
			}
			return "Give standard support.", nil
		},
		ModelResolver: func(_ context.Context, input lebro.RunInput) (lebro.ModelSelection, error) {
			if input.Metadata["tier"] == "premium" {
				return lebro.ModelSelection{Model: premium, ModelName: "premium-model"}, nil
			}
			return lebro.ModelSelection{Model: standard, ModelName: "standard-model"}, nil
		},
	})
	if err != nil {
		panic(err)
	}

	result, err := agent.Run(context.Background(), lebro.RunInput{
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "I need help."}},
		Metadata: map[string]string{"tier": "premium"},
	})
	if err != nil {
		panic(err)
	}
	fmt.Fprintln(os.Stdout, result.Messages[len(result.Messages)-1].Content)
}
