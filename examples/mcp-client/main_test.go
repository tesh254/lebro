package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
	"github.com/tesh254/lebro/mcp"
)

func connectExampleClient(t *testing.T) *mcp.Client {
	t.Helper()
	ctx := context.Background()
	clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()

	serverSession, err := newRemoteServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect remote server: %v", err)
	}

	client := mcp.NewClient(mcp.ClientConfig{
		Implementation: &mcpsdk.Implementation{Name: "mcp-client-example", Version: "test"},
		ServerName:     "weather",
	})
	if err := client.Connect(ctx, clientTransport); err != nil {
		t.Fatalf("connect client: %v", err)
	}

	t.Cleanup(func() {
		_ = client.Close()
		_ = serverSession.Close()
	})
	return client
}

func TestExampleRuns(t *testing.T) {
	if err := run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestExampleDiscoversAndInvokesRemoteTool(t *testing.T) {
	ctx := context.Background()
	client := connectExampleClient(t)

	tools, err := client.DiscoverTools(ctx)
	if err != nil {
		t.Fatalf("DiscoverTools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("discovered %d tools, want 1", len(tools))
	}
	if got := tools[0].Definition().ID; got != "weather.lookup" {
		t.Fatalf("ID = %q, want %q", got, "weather.lookup")
	}

	registry, err := lebro.NewToolRegistry(lebrojsonschema.NewCompiler())
	if err != nil {
		t.Fatalf("NewToolRegistry: %v", err)
	}
	if err := registry.Register(tools[0]); err != nil {
		t.Fatalf("Register: %v", err)
	}

	result := registry.Execute(ctx, "weather.lookup", lebro.ToolExecutionRequest{
		Arguments: json.RawMessage(`{"city":"Nairobi"}`),
	})
	if result.State != lebro.ToolExecutionSucceeded {
		t.Fatalf("State = %q, want %q (err: %v)", result.State, lebro.ToolExecutionSucceeded, result.Err)
	}

	var output struct {
		City         string  `json:"city"`
		TemperatureC float64 `json:"temperature_c"`
		Condition    string  `json:"condition"`
	}
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if output.City != "Nairobi" {
		t.Errorf("city = %q, want %q", output.City, "Nairobi")
	}
	if output.Condition != "sunny" {
		t.Errorf("condition = %q, want %q", output.Condition, "sunny")
	}
}

func TestExampleRejectsInvalidArgumentsLocally(t *testing.T) {
	ctx := context.Background()
	client := connectExampleClient(t)

	tools, err := client.DiscoverTools(ctx)
	if err != nil {
		t.Fatalf("DiscoverTools: %v", err)
	}

	registry, err := lebro.NewToolRegistry(lebrojsonschema.NewCompiler())
	if err != nil {
		t.Fatalf("NewToolRegistry: %v", err)
	}
	if err := registry.Register(tools[0]); err != nil {
		t.Fatalf("Register: %v", err)
	}

	result := registry.Execute(ctx, "weather.lookup", lebro.ToolExecutionRequest{
		Arguments: json.RawMessage(`{"city":42}`),
	})
	if result.State != lebro.ToolExecutionInvalidInput {
		t.Fatalf("State = %q, want %q", result.State, lebro.ToolExecutionInvalidInput)
	}
	if errors.Is(result.Err, mcp.ErrRemoteInvocation) || errors.Is(result.Err, mcp.ErrRemoteToolError) {
		t.Error("local validation failure must not be reported as a remote failure")
	}
}
