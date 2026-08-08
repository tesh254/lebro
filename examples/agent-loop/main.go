// The agent-loop example runs a bounded tool-using agent against a scripted
// fixture model and a local schema-backed tool, with no network access.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

type weatherTool struct{}

func (weatherTool) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{
		ID:          "weather.lookup",
		Description: "Look up the current temperature for a city",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"required":["city"],
			"properties":{"city":{"type":"string"}},
			"additionalProperties":false
		}`),
		OutputSchema: json.RawMessage(`{
			"type":"object",
			"required":["temperature_c"],
			"properties":{"temperature_c":{"type":"number"}},
			"additionalProperties":false
		}`),
	}
}

func (weatherTool) Execute(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
	var args struct {
		City string `json:"city"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, err
	}
	temperature := 24.5
	if args.City == "Kampala" {
		temperature = 25.0
	}
	return json.Marshal(struct {
		Temperature float64 `json:"temperature_c"`
	}{Temperature: temperature})
}

func main() {
	must(run(os.Stdout))
}

func run(output io.Writer) error {
	registry, err := lebro.NewToolRegistry(lebrojsonschema.NewCompiler())
	if err != nil {
		return err
	}
	if err := registry.Register(weatherTool{}); err != nil {
		return err
	}

	model := testkit.NewModel(
		testkit.ToolCallResponse(testkit.ToolCall{ToolID: "weather.lookup", Arguments: json.RawMessage(`{"city":"Nairobi"}`)}),
		testkit.Text("The temperature in Nairobi is 24.5C."),
	)
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:           "weather-agent",
			Name:         "Weather",
			Instructions: "Use weather.lookup to answer weather questions, then summarize.",
			Model:        "fixture-model",
			Tools:        []lebro.ToolID{"weather.lookup"},
		},
		Model: model,
		Tools: registry,
	})
	if err != nil {
		return err
	}

	result, err := agent.Run(context.Background(), lebro.RunInput{
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "What is the temperature in Nairobi?"}},
		Metadata: map[string]string{"request_id": "req-1"},
	})
	if err != nil {
		var agentErr *lebro.AgentError
		if errors.As(err, &agentErr) {
			return fmt.Errorf("agent %s at step %d: %w", agentErr.Kind, agentErr.Step, agentErr)
		}
		return err
	}
	writef(output, "status: %s\n", result.Status)
	writef(output, "run_id: %s\n", result.ID)
	for _, message := range result.Messages {
		switch message.Role {
		case lebro.RoleAssistant:
			writef(output, "assistant: %s\n", message.Content)
		case lebro.RoleTool:
			writef(output, "tool[%s]: %s\n", message.ToolCallID, message.Content)
		}
	}

	exhausted, exhaustedErr := runExhaustingAgent(registry)
	if exhaustedErr != nil {
		var agentErr *lebro.AgentError
		if errors.As(exhaustedErr, &agentErr) {
			writef(output, "exhausted: %s (%s)\n", exhausted.Status, agentErr.Kind)
		} else {
			return exhaustedErr
		}
	} else {
		writef(output, "exhausted: %s\n", exhausted.Status)
	}
	return nil
}

func runExhaustingAgent(registry *lebro.ToolRegistry) (lebro.RunResult, error) {
	fixtures := make([]testkit.Fixture, 0, 4)
	for i := 0; i < 4; i++ {
		fixtures = append(fixtures, testkit.ToolCallResponse(testkit.ToolCall{ToolID: "weather.lookup", Arguments: json.RawMessage(`{"city":"Nairobi"}`)}))
	}
	looping := testkit.NewModel(fixtures...)
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "loop", Model: "fixture-model", Tools: []lebro.ToolID{"weather.lookup"}},
		Model:      looping,
		Tools:      registry,
		MaxSteps:   2,
	})
	if err != nil {
		return lebro.RunResult{}, err
	}
	return agent.Run(context.Background(), lebro.RunInput{
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "loop forever"}},
	})
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
