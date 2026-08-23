// The agent-network example routes one request to a named specialist and
// persists its route record. RuleRouter completes after its first handoff;
// use a router that inspects RoutingRequest.Hops for multi-hop networks.
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
	specialist, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "researcher", Name: "Researcher", Instructions: "Answer concisely."},
		Model:      newFixtureModel([]string{"Lebro is written in Go."}),
	})
	if err != nil {
		return err
	}
	router, err := lebro.NewRuleRouter(nil, "research")
	if err != nil {
		return err
	}
	store := lebro.NewMemoryStore()
	network, err := lebro.NewNetwork(lebro.NetworkConfig{
		Definition:  lebro.WorkflowDefinition{ID: "knowledge-network", Name: "Knowledge network"},
		Router:      router,
		Specialists: []lebro.NetworkSpecialist{{ID: "research", Workflow: specialist, Description: "Research facts."}},
		Store:       store,
	})
	if err != nil {
		return err
	}
	result, err := network.Run(context.Background(), lebro.RunInput{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "What is Lebro?"}}})
	if err != nil {
		return err
	}
	record, err := store.WorkflowRuns().GetWorkflowRun(context.Background(), result.ID)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "status: %s\nroutes: %d\nanswer: %s\n", result.Status, len(record.StepOutputs), result.Messages[len(result.Messages)-1].Content)
	return err
}

// fixtureModel is a deterministic stand-in for a provider adapter: it replies
// with the next entry per call. A real deployment supplies openai.New or any
// other lebro.Model instead.
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
