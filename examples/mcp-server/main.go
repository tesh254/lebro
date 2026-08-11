// mcp-server demonstrates exposing lebro tools, agents, and workflows through
// an MCP server that any MCP-compatible client can connect to over stdio.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
	"github.com/tesh254/lebro/mcp"
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
			"required":["temperature_c","condition","city"],
			"properties":{
				"temperature_c":{"type":"number"},
				"condition":{"type":"string"},
				"city":{"type":"string"}
			},
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
	return json.Marshal(map[string]any{
		"temperature_c": 24.5,
		"condition":     "sunny",
		"city":          args.City,
	})
}

type calculatorTool struct{}

func (calculatorTool) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{
		ID:          "calc.add",
		Description: "Add two numbers",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"required":["a","b"],
			"properties":{"a":{"type":"number"},"b":{"type":"number"}},
			"additionalProperties":false
		}`),
		OutputSchema: json.RawMessage(`{
			"type":"object",
			"required":["result"],
			"properties":{"result":{"type":"number"}},
			"additionalProperties":false
		}`),
	}
}

func (calculatorTool) Execute(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
	var args struct {
		A float64 `json:"a"`
		B float64 `json:"b"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"result": args.A + args.B})
}

func main() {
	registry := mustValue(lebro.NewToolRegistry(lebrojsonschema.NewCompiler()))
	must(registry.Register(weatherTool{}))
	must(registry.Register(calculatorTool{}))

	weather, ok := registry.Resolve("weather.lookup")
	if !ok {
		log.Fatal("resolve weather.lookup")
	}
	calc, ok := registry.Resolve("calc.add")
	if !ok {
		log.Fatal("resolve calc.add")
	}

	server := mcp.NewServer(mcp.ServerConfig{
		Implementation: &mcpsdk.Implementation{
			Name:    "lebro-mcp-server",
			Version: "1.0.0",
		},
		Instructions: "lebro MCP server exposing tools, agents, and workflows",
	})
	must(server.ExposeTool(weather))
	must(server.ExposeTool(calc))

	fmt.Fprintln(log.Writer(), "MCP server ready on stdio — connect with any MCP client")
	if err := server.Run(context.Background(), &mcpsdk.StdioTransport{}); err != nil {
		log.Fatal(err)
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
