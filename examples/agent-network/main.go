// The agent-network example routes one request to a named specialist and
// persists its route record. RuleRouter completes after its first handoff;
// use a router that inspects RoutingRequest.Hops for multi-hop networks.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	specialist, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "researcher", Name: "Researcher", Instructions: "Answer concisely."},
		Model:      testkit.NewModel(testkit.Text("Lebro is written in Go.")),
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
	fmt.Printf("status: %s\nroutes: %d\nanswer: %s\n", result.Status, len(record.StepOutputs), result.Messages[len(result.Messages)-1].Content)
	return nil
}
