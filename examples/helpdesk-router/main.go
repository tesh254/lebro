// helpdesk-router is the tier-1 front-desk build: every incoming employee
// request enters through one entry point, a deterministic router picks the
// right specialist — IT, HR, or facilities — under bounded traversal, and each
// handoff is retained as an auditable route record in the store.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
)

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(output io.Writer) error {
	store := lebro.NewMemoryStore()

	network, err := lebro.NewNetwork(lebro.NetworkConfig{
		Definition: lebro.WorkflowDefinition{ID: "helpdesk-front-desk", Name: "Helpdesk Front Desk"},
		Router: mustValue(lebro.NewRuleRouter([]lebro.RouteRule{
			{SpecialistID: "it", Match: mentions("vpn", "password", "laptop", "login")},
			{SpecialistID: "hr", Match: mentions("leave", "benefits", "payroll", "policy")},
			{SpecialistID: "facilities", Match: mentions("desk", "chair", "office", "badge")},
		}, "it")),
		Specialists: []lebro.NetworkSpecialist{
			{ID: "it", Description: "Accounts, devices, access.", Workflow: specialistAgent("it-specialist", "Reset the credentials and confirm.")},
			{ID: "hr", Description: "Leave, benefits, people ops.", Workflow: specialistAgent("hr-specialist", "Unused leave carries over automatically; nothing to file.")},
			{ID: "facilities", Description: "Desks, rooms, building issues.", Workflow: specialistAgent("facilities-specialist", "A replacement chair is scheduled for tomorrow.")},
		},
		// Two hops bound the traversal: one specialist handoff plus the
		// router's completion check.
		MaxHops: 2,
		Store:   store,
	})
	if err != nil {
		return err
	}

	for _, ticket := range []struct {
		id      string
		request string
		want    string
	}{
		{"TCK-1", "My VPN login is rejected after the password reset.", "it"},
		{"TCK-2", "Do unused leave days expire at year end?", "hr"},
		{"TCK-3", "My desk chair is broken.", "facilities"},
	} {
		result, err := network.Run(context.Background(), lebro.RunInput{
			Messages: []lebro.Message{{Role: lebro.RoleUser, Content: ticket.request}},
		})
		if err != nil {
			return err
		}
		record, err := store.WorkflowRuns().GetWorkflowRun(context.Background(), result.ID)
		if err != nil {
			return err
		}
		routes, err := routeRecords(record.StepOutputs)
		if err != nil {
			return err
		}
		writef(output, "%s routed to %s (%d hop(s))\n", ticket.id, routes[0].Selected, len(routes))
		writef(output, "  reply: %s\n", result.Messages[len(result.Messages)-1].Content)

		if string(routes[0].Selected) != ticket.want {
			return fmt.Errorf("%s routed to %q, want %q", ticket.id, routes[0].Selected, ticket.want)
		}
	}
	return nil
}

// specialistAgent builds one bounded specialist whose scripted reply stands in
// for real domain work.
func specialistAgent(id, reply string) *lebro.Agent {
	return mustValue(lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: lebro.AgentID(id), Name: id},
		Model:      testkit.NewModel(testkit.Text(reply)),
	}))
}

// mentions matches when any keyword appears in the task text.
func mentions(keywords ...string) func(lebro.RoutingRequest) bool {
	return func(request lebro.RoutingRequest) bool {
		task := strings.ToLower(request.Task)
		for _, keyword := range keywords {
			if strings.Contains(task, keyword) {
				return true
			}
		}
		return false
	}
}

// routeRecords decodes the durable hop history the network wrote into the run
// record, so an auditor can reconstruct every routing decision.
func routeRecords(outputs []json.RawMessage) ([]lebro.NetworkRouteRecord, error) {
	records := make([]lebro.NetworkRouteRecord, 0, len(outputs))
	for _, output := range outputs {
		var record lebro.NetworkRouteRecord
		if err := json.Unmarshal(output, &record); err != nil {
			return nil, fmt.Errorf("decode route record: %w", err)
		}
		records = append(records, record)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("no route records persisted")
	}
	return records, nil
}

func writef(output io.Writer, format string, args ...any) {
	if _, err := fmt.Fprintf(output, format, args...); err != nil {
		panic(err)
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func mustValue[T any](value T, err error) T {
	must(err)
	return value
}
