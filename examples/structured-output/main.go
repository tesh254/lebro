// The structured-output example runs an agent that requests a schema-constrained
// JSON result from a fixture model, validates it locally, and decodes it into a
// typed Go value. No network or API key required.
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

func main() {
	must(run(os.Stdout))
}

func run(output io.Writer) error {
	model := testkit.NewModel(
		testkit.StructuredOutput(json.RawMessage(`{"temperature_c":24.5,"city":"Nairobi"}`)),
	)
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:           "weather-agent",
			Instructions: "Return the weather as structured JSON.",
			Model:        "fixture-model",
		},
		Model:          model,
		SchemaCompiler: lebrojsonschema.NewCompiler(),
		OutputSchema: &lebro.ModelOutputSchema{
			Name:   "weather_result",
			Schema: json.RawMessage(`{"type":"object","required":["temperature_c","city"],"properties":{"temperature_c":{"type":"number"},"city":{"type":"string"}},"additionalProperties":false}`),
			Strict: true,
		},
	})
	if err != nil {
		return err
	}

	result, err := agent.Run(context.Background(), lebro.RunInput{
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "Weather in Nairobi?"}},
	})
	if err != nil {
		var agentErr *lebro.AgentError
		if errors.As(err, &agentErr) {
			return fmt.Errorf("agent %s at step %d: %w", agentErr.Kind, agentErr.Step, agentErr)
		}
		return err
	}

	var report struct {
		TemperatureC float64 `json:"temperature_c"`
		City         string  `json:"city"`
	}
	if err := result.DecodeStructuredOutput(&report); err != nil {
		return err
	}
	writef(output, "status: %s\n", result.Status)
	writef(output, "structured: %s\n", result.StructuredOutput().Raw())
	writef(output, "decoded: %.1fC in %s\n", report.TemperatureC, report.City)
	return nil
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
