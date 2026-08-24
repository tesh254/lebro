// The request-resolvers example selects instructions and a model per tenant
// without mutating the shared agent definition.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/tesh254/lebro"
)

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(output io.Writer) error {
	standard := newFixtureModel([]string{"standard tenant response"})
	premium := newFixtureModel([]string{"premium tenant response"})
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
		return err
	}

	result, err := agent.Run(context.Background(), lebro.RunInput{
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "I need help."}},
		Metadata: map[string]string{"tier": "premium"}},
	)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, result.Messages[len(result.Messages)-1].Content)
	return err
}

// fixtureModel is a deterministic stand-in for a provider adapter: it replies
// with the next entry per call. A real deployment supplies openai.New or any
// other lebro.Model instead; resolvers are unchanged by the swap.
type fixtureModel struct {
	replies []string
	calls   int
}

func newFixtureModel(replies []string) *fixtureModel { return &fixtureModel{replies: replies} }

func (m *fixtureModel) Generate(_ context.Context, _ lebro.ModelRequest) (lebro.ModelResponse, error) {
	if m.calls >= len(m.replies) {
		return lebro.ModelResponse{}, errors.New("fixture model script exhausted")
	}
	reply := m.replies[m.calls]
	m.calls++
	return lebro.ModelResponse{
		Message:      lebro.Message{Role: lebro.RoleAssistant, Content: reply},
		FinishReason: lebro.FinishReasonStop,
	}, nil
}
