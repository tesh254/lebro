package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tesh254/lebro"
)

func TestExample(t *testing.T) {
	main()
}

func TestRunTimelineIsQueryable(t *testing.T) {
	ctx := context.Background()
	store := lebro.NewMemoryStore()

	agent := mustValue(lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:           "support-agent",
			Model:        "gpt-fixture",
			Instructions: "Answer support questions.",
			Tools:        []lebro.ToolID{"order_lookup"},
		},
		Model: newFixtureModel(),
		Tools: mustValue(newLookupRegistry()),
		Store: store,
	}))
	result, err := agent.Run(ctx, lebro.RunInput{
		ThreadID:    "thread-timeline",
		Messages:    []lebro.Message{{Role: lebro.RoleUser, Content: "Where is my order?"}},
		Annotations: lebro.Metadata{"app.customer_id": json.RawMessage(`"acme"`)},
	})
	if err != nil {
		t.Fatal(err)
	}

	attempts := mustValue(store.ModelAttempts().ListModelAttempts(ctx, lebro.ModelAttemptFilter{RunID: result.ID}, lebro.PageRequest{}))
	if len(attempts.Records) != 2 || attempts.Records[1].Usage.TotalTokens != 224 {
		t.Fatalf("attempts = %#v, want two attempts with usage on the terminal one", attempts.Records)
	}
	if len(attempts.Records[1].ProducedMessageIDs) != 1 {
		t.Fatalf("terminal attempt produced-message linkage = %v", attempts.Records[1].ProducedMessageIDs)
	}

	executions := mustValue(store.ToolExecutions().ListToolExecutions(ctx, lebro.ToolExecutionFilter{RunID: result.ID}, lebro.PageRequest{}))
	if len(executions.Records) != 1 || executions.Records[0].State != lebro.ToolExecutionSucceeded {
		t.Fatalf("tool executions = %#v", executions.Records)
	}
	raw, err := json.Marshal(executions.Records)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "order_id") {
		t.Fatalf("tool execution persisted arguments: %s", raw)
	}

	appendPluginDecision(ctx, store)

	events := mustValue(store.RunEvents().ListRunEvents(ctx, lebro.RunEventFilter{ThreadID: "thread-timeline"}, lebro.PageRequest{}))
	var sawPlugin bool
	for _, event := range events.Records {
		if event.Type == lebro.RunEventDelta {
			t.Fatal("delta events must not be persisted")
		}
		if event.Plugin != nil && event.Plugin.ID == "thread-compactor" {
			sawPlugin = true
		}
	}
	if !sawPlugin {
		t.Fatal("plugin decision event missing from the timeline")
	}
}
