package main

import (
	"context"
	"encoding/json"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
	"github.com/tesh254/lebro/mcp"
)

func TestExampleExposesWeatherTool(t *testing.T) {
	registry, err := lebro.NewToolRegistry(lebrojsonschema.NewCompiler())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(weatherTool{}); err != nil {
		t.Fatal(err)
	}
	weather, ok := registry.Resolve("weather.lookup")
	if !ok {
		t.Fatal("resolve weather.lookup")
	}

	server := mcp.NewServer(mcp.ServerConfig{
		Implementation: &mcpsdk.Implementation{Name: "example", Version: "test"},
	})
	if err := server.ExposeTool(weather); err != nil {
		t.Fatal(err)
	}

	clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "weather.lookup" {
		t.Fatalf("tools = %#v, want weather.lookup", tools.Tools)
	}
	result, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "weather.lookup",
		Arguments: map[string]any{"city": "Nairobi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("weather lookup returned an error: %#v", result.Content)
	}
	var output struct {
		TemperatureC float64 `json:"temperature_c"`
		Condition    string  `json:"condition"`
		City         string  `json:"city"`
	}
	if len(result.Content) != 1 {
		t.Fatalf("weather content = %#v", result.Content)
	}
	textContent, ok := result.Content[0].(*mcpsdk.TextContent)
	if !ok {
		t.Fatalf("weather content type = %T, want TextContent", result.Content[0])
	}
	text := textContent.Text
	if err := json.Unmarshal([]byte(text), &output); err != nil {
		t.Fatalf("decode weather output: %v", err)
	}
	if output.TemperatureC != 24.5 || output.Condition != "sunny" || output.City != "Nairobi" {
		t.Fatalf("weather output = %#v", output)
	}
}
