// tenant-policy demonstrates a tenant-scoped policy around a shared agent.
// It authorizes the run before a model call and keeps identity in context, so
// an application can enforce the same tenant boundary across nested work.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
)

type tenantPolicy struct{}

func (tenantPolicy) Authorize(_ context.Context, identity lebro.Identity, action lebro.Action, _ lebro.Resource) lebro.Decision {
	if action == lebro.ActionAgentRun && identity.Tenant != "acme" {
		return lebro.Deny("tenant is not allowed to run this agent")
	}
	return lebro.Allow()
}

func main() { must(run(os.Stdout)) }

func run(output io.Writer) error {
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "support", Name: "Support"},
		Model:      testkit.NewModel(testkit.Text("Your ticket is open.")),
		Policy:     tenantPolicy{},
	})
	if err != nil {
		return err
	}

	denied := lebro.WithIdentity(context.Background(), lebro.Identity{Subject: "sam", Tenant: "other"})
	_, err = agent.Run(denied, lebro.RunInput{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "Need help"}}})
	if !errors.Is(err, lebro.ErrPolicyDenied) {
		return fmt.Errorf("denied tenant error = %v, want ErrPolicyDenied", err)
	}
	fmt.Fprintln(output, "other tenant: denied")

	allowed := lebro.WithIdentity(context.Background(), lebro.Identity{Subject: "ava", Tenant: "acme"})
	result, err := agent.Run(allowed, lebro.RunInput{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "Need help"}}})
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "acme tenant: %s\n", result.Messages[len(result.Messages)-1].Content)
	return nil
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
