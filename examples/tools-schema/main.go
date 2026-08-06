// tools-schema demonstrates safe local tool registration and execution with
// JSON Schema validation on both sides of the handler.
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

type weatherTool struct{}

func (weatherTool) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{
		ID:          "weather.lookup",
		Description: "Look up the current weather for a city",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"required":["city"],
			"properties":{"city":{"type":"string"}},
			"additionalProperties":false
		}`),
		OutputSchema: json.RawMessage(`{
			"type":"object",
			"required":["temperature_c"],
			"properties":{
				"temperature_c":{"type":"number"},
				"request_id":{"type":"string"}
			},
			"additionalProperties":false
		}`),
	}
}

func (weatherTool) Execute(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	metadata := lebro.ToolMetadataFromContext(ctx)
	return json.Marshal(struct {
		Temperature float64 `json:"temperature_c"`
		RequestID   string  `json:"request_id,omitempty"`
	}{Temperature: 24.5, RequestID: metadata["request_id"]})
}

func main() {
	registry := mustValue(lebro.NewToolRegistry(lebrojsonschema.NewCompiler()))
	must(registry.Register(weatherTool{}))

	result := registry.Execute(context.Background(), "weather.lookup", lebro.ToolExecutionRequest{
		Arguments: json.RawMessage(`{"city":"Nairobi"}`),
		Metadata:  map[string]string{"request_id": "req-42"},
	})
	must(result.Err)

	fmt.Printf("%s: %s\n", result.ToolID, result.Output)
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
