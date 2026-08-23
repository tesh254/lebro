// The workflow-agents-tools example combines an ordinary Go step, a
// schema-backed tool step, and an agent step in one linear workflow.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

type weatherTool struct{}

func (weatherTool) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{
		ID:           "weather.lookup",
		InputSchema:  json.RawMessage(`{"type":"object","required":["city"],"properties":{"city":{"type":"string"}},"additionalProperties":false}`),
		OutputSchema: json.RawMessage(`{"type":"object","required":["city","temperature_c"],"properties":{"city":{"type":"string"},"temperature_c":{"type":"number"}},"additionalProperties":false}`),
	}
}

func (weatherTool) Execute(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
	var request struct {
		City string `json:"city"`
	}
	if err := json.Unmarshal(input, &request); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		City        string  `json:"city"`
		Temperature float64 `json:"temperature_c"`
	}{City: request.City, Temperature: 24.5})
}

func main() { must(run(os.Stdout)) }

func run(output io.Writer) error {
	registry, err := lebro.NewToolRegistry(lebrojsonschema.NewCompiler())
	if err != nil {
		return err
	}
	if err := registry.Register(weatherTool{}); err != nil {
		return err
	}
	registered, ok := registry.Resolve("weather.lookup")
	if !ok {
		return fmt.Errorf("weather tool was not registered")
	}
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "summary", Model: "fixture"},
		Model:      newFixtureModel("Nairobi is 24.5C."),
	})
	if err != nil {
		return err
	}
	agentStep, err := lebro.NewAgentStep(agent)
	if err != nil {
		return err
	}
	toolStep, err := lebro.NewToolStep(registered)
	if err != nil {
		return err
	}
	wf, err := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition: lebro.WorkflowDefinition{ID: "weather-summary"},
		Steps: []lebro.Step{
			{Definition: lebro.StepDefinition{ID: "request"}, Handler: lebro.StepHandlerFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`{"city":"Nairobi"}`), nil
			})},
			{Definition: lebro.StepDefinition{ID: "weather"}, Handler: toolStep},
			{Definition: lebro.StepDefinition{ID: "summarize"}, Handler: agentStep},
		},
	})
	if err != nil {
		return err
	}
	result, err := wf.Run(context.Background(), lebro.WorkflowRunInput{Input: json.RawMessage(`null`)})
	if err != nil {
		return err
	}
	var summary string
	if err := result.DecodeOutput(&summary); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "status: %s\nsummary: %s\n", result.Status, summary)
	return err
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// fixtureModel is a deterministic stand-in for a provider adapter: it always
// replies with the single scripted turn. A real deployment supplies openai.New
// or any other lebro.Model instead.
type fixtureModel struct {
	reply string
}

func newFixtureModel(reply string) *fixtureModel { return &fixtureModel{reply: reply} }

func (m *fixtureModel) Generate(_ context.Context, _ lebro.ModelRequest) (lebro.ModelResponse, error) {
	return lebro.ModelResponse{
		Message:      lebro.Message{Role: lebro.RoleAssistant, Content: m.reply},
		FinishReason: lebro.FinishReasonStop,
	}, nil
}
