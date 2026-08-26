// run-timeline demonstrates MAD-83's durable observability records: a store-
// bound agent run persists model attempts (provider identity, token usage,
// finish reason), tool-execution lifecycle, and ordered run events. After the
// run, the example queries the thread timeline — attempts, tool executions,
// and events, including a plugin decision record appended through the
// repository — and prints it as one correlated view.
//
// The same queries work against SQLiteStore and PostgresStore; only the
// constructor changes.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

func main() {
	ctx := context.Background()
	store := lebro.NewMemoryStore()

	agent := mustValue(lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:           "support-agent",
			Name:         "support",
			Instructions: "Answer support questions.",
			Model:        "gpt-fixture",
			Tools:        []lebro.ToolID{"order_lookup"},
		},
		Model: newFixtureModel(),
		Tools: mustValue(newLookupRegistry()),
		Store: store,
	}))

	result, err := agent.Run(ctx, lebro.RunInput{
		ThreadID: "support-42",
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "Where is my order?"}},
		Annotations: lebro.Metadata{
			"app.customer_id": json.RawMessage(`"acme"`),
			"app.channel":     json.RawMessage(`"web"`),
		},
	})
	must(err)

	fmt.Printf("run %s %s\n", result.ID, result.Status)
	printAttempts(ctx, store, result.ID)
	printToolTimeline(ctx, store, result.ID)
	appendPluginDecision(ctx, store)
	printEvents(ctx, store)
}

// printAttempts shows which provider/model produced each outcome and what it
// cost in tokens.
func printAttempts(ctx context.Context, store *lebro.MemoryStore, runID lebro.RunID) {
	page := mustValue(store.ModelAttempts().ListModelAttempts(ctx, lebro.ModelAttemptFilter{RunID: runID}, lebro.PageRequest{}))
	for _, attempt := range page.Records {
		fmt.Printf("attempt #%d provider=%q model=%q status=%s finish=%s tokens=%d/%d duration<1s produced=%v\n",
			attempt.Index, string(attempt.Provider), attempt.Model, attempt.Status, attempt.FinishReason,
			attempt.Usage.InputTokens, attempt.Usage.OutputTokens, attempt.ProducedMessageIDs)
	}
}

// printToolTimeline correlates tool calls to the run through stable IDs.
func printToolTimeline(ctx context.Context, store *lebro.MemoryStore, runID lebro.RunID) {
	page := mustValue(store.ToolExecutions().ListToolExecutions(ctx, lebro.ToolExecutionFilter{RunID: runID}, lebro.PageRequest{}))
	for _, execution := range page.Records {
		fmt.Printf("tool call=%s id=%s state=%s started=%s finished=%s error=%q\n",
			execution.ToolCallID, string(execution.ToolID), execution.State,
			execution.StartedAt.Format(time.RFC3339), execution.FinishedAt.Format(time.RFC3339), execution.ErrorMessage)
	}
}

// appendPluginDecision writes the kind of plugin lifecycle record that
// runtime plugins emit through the same repository.
func appendPluginDecision(ctx context.Context, store *lebro.MemoryStore) {
	events := mustValue(store.RunEvents().ListRunEvents(ctx, lebro.RunEventFilter{}, lebro.PageRequest{}))
	last := events.Records[len(events.Records)-1]
	must(store.RunEvents().AppendRunEvents(ctx, []lebro.RunEventRecord{{
		ID:        fmt.Sprintf("%s-plugin-decision", last.RunID),
		RunID:     last.RunID,
		ThreadID:  last.ThreadID,
		Sequence:  last.Sequence + 1000,
		Type:      lebro.RunEventPlugin,
		Timestamp: time.Now().UTC(),
		Payload:   json.RawMessage(`{"policy":"rolling-window","messages_considered":4}`),
		Plugin:    &lebro.PluginAttribution{ID: "thread-compactor", Version: "0.1.0", Action: "compact.evaluate", Outcome: "approved"},
		Metadata:  lebro.Metadata{"plugin.reason": json.RawMessage(`"thread under budget"`)},
	}}))
}

// printEvents replays the durable event stream in sequence order.
func printEvents(ctx context.Context, store *lebro.MemoryStore) {
	page := mustValue(store.RunEvents().ListRunEvents(ctx, lebro.RunEventFilter{ThreadID: "support-42"}, lebro.PageRequest{}))
	for _, event := range page.Records {
		line := fmt.Sprintf("event seq=%d type=%s", event.Sequence, event.Type)
		if event.Provider != "" || event.ProviderModel != "" {
			line += fmt.Sprintf(" provider=%q model=%q", string(event.Provider), event.ProviderModel)
		}
		if event.Plugin != nil {
			line += fmt.Sprintf(" plugin=%s/%s action=%s outcome=%s", event.Plugin.ID, event.Plugin.Version, event.Plugin.Action, event.Plugin.Outcome)
		}
		fmt.Println(line)
	}
}

// fixtureModel answers with one tool request, then one terminal response, so
// the run exercises both the tool and model lifecycle.
type fixtureModel struct{}

var _ lebro.Model = (*fixtureModel)(nil)

func newFixtureModel() *fixtureModel { return &fixtureModel{} }

func (m *fixtureModel) Generate(ctx context.Context, request lebro.ModelRequest) (lebro.ModelResponse, error) {
	for _, message := range request.Messages {
		if message.Role == lebro.RoleTool {
			return lebro.ModelResponse{
				Message:      lebro.Message{Role: lebro.RoleAssistant, Content: "Your order shipped yesterday."},
				FinishReason: lebro.FinishReasonStop,
				Usage:        lebro.ModelUsage{InputTokens: 210, OutputTokens: 14, TotalTokens: 224},
			}, nil
		}
	}
	calls := mustValue(lebro.NewModelToolCalls(lebro.ModelToolCall{
		ID:        "call-order-status",
		ToolID:    "order_lookup",
		Arguments: json.RawMessage(`{"order_id":"ord-9"}`),
	}))
	return lebro.ModelResponse{
		Message:      lebro.Message{Role: lebro.RoleAssistant, ToolCalls: calls},
		FinishReason: lebro.FinishReasonToolCalls,
		Usage:        lebro.ModelUsage{InputTokens: 120, OutputTokens: 8, TotalTokens: 128},
	}, nil
}

func newLookupRegistry() (*lebro.ToolRegistry, error) {
	registry, err := lebro.NewToolRegistry(lebrojsonschema.NewCompiler())
	if err != nil {
		return nil, err
	}
	err = registry.Register(orderLookupTool{})
	return registry, err
}

// orderLookupTool is the schema-backed capability the model requests.
type orderLookupTool struct{}

var _ lebro.Tool = (*orderLookupTool)(nil)

func (orderLookupTool) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{
		ID:          "order_lookup",
		Description: "Look up the shipping status of an order.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"order_id":{"type":"string"}},
			"required":["order_id"],
			"additionalProperties":false
		}`),
		OutputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"status":{"type":"string"}},
			"required":["status"],
			"additionalProperties":false
		}`),
	}
}

func (orderLookupTool) Execute(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"status":"shipped"}`), nil
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
