// mcp-client demonstrates consuming tools from a remote MCP server: discover
// what the server advertises, register the adapted tools in a lebro
// ToolRegistry, and invoke one through the schema-checked execution boundary.
//
// The "remote" server here runs in-process over an in-memory transport so the
// example is runnable without a network or a second binary. Swapping the
// transport for a StdioTransport or the Streamable HTTP transport is the only
// change needed to talk to a real server.
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

// newRemoteServer builds a stand-in for a third-party MCP server that
// advertises a single weather tool.
func newRemoteServer() *mcpsdk.Server {
	server := mcpsdk.NewServer(
		&mcpsdk.Implementation{Name: "weather-service", Version: "1.0.0"},
		nil,
	)
	server.AddTool(&mcpsdk.Tool{
		Name:        "lookup",
		Description: "Look up the current weather for a city",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"required":["city"],
			"properties":{"city":{"type":"string"}},
			"additionalProperties":false
		}`),
		OutputSchema: json.RawMessage(`{
			"type":"object",
			"required":["city","temperature_c","condition"],
			"properties":{
				"city":{"type":"string"},
				"temperature_c":{"type":"number"},
				"condition":{"type":"string"}
			},
			"additionalProperties":false
		}`),
	}, func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		var args struct {
			City string `json:"city"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		payload, err := json.Marshal(map[string]any{
			"city":          args.City,
			"temperature_c": 24.5,
			"condition":     "sunny",
		})
		if err != nil {
			return nil, err
		}
		return &mcpsdk.CallToolResult{
			Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: string(payload)}},
			StructuredContent: json.RawMessage(payload),
		}, nil
	})
	return server
}

func run(ctx context.Context) error {
	clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()

	remote := newRemoteServer()
	serverSession, err := remote.Connect(ctx, serverTransport, nil)
	if err != nil {
		return fmt.Errorf("connect remote server: %w", err)
	}
	defer func() { _ = serverSession.Close() }()

	// ServerName namespaces every discovered tool so several servers can share
	// one registry without their tool names colliding.
	client := mcp.NewClient(mcp.ClientConfig{
		Implementation: &mcpsdk.Implementation{Name: "mcp-client-example", Version: "1.0.0"},
		ServerName:     "weather",
	})
	if err := client.Connect(ctx, clientTransport); err != nil {
		return fmt.Errorf("connect client: %w", err)
	}
	defer func() { _ = client.Close() }()

	tools, err := client.DiscoverTools(ctx)
	if err != nil {
		return fmt.Errorf("discover tools: %w", err)
	}

	registry, err := lebro.NewToolRegistry(lebrojsonschema.NewCompiler())
	if err != nil {
		return fmt.Errorf("new tool registry: %w", err)
	}
	for _, tool := range tools {
		definition := tool.Definition()
		if err := registry.Register(tool); err != nil {
			return fmt.Errorf("register %q: %w", definition.ID, err)
		}
		fmt.Printf("discovered %s: %s\n", definition.ID, definition.Description)
	}

	// Remote tools go through the same validated boundary as local ones:
	// arguments are checked before they reach the wire, and the result is
	// checked against the schema the server advertised.
	result := registry.Execute(ctx, "weather.lookup", lebro.ToolExecutionRequest{
		Arguments: json.RawMessage(`{"city":"Nairobi"}`),
	})
	if result.State != lebro.ToolExecutionSucceeded {
		return fmt.Errorf("execute weather.lookup: %s: %w", result.State, result.Err)
	}
	fmt.Printf("result: %s\n", result.Output)

	// Invalid arguments never reach the remote server.
	invalid := registry.Execute(ctx, "weather.lookup", lebro.ToolExecutionRequest{
		Arguments: json.RawMessage(`{"city":42}`),
	})
	fmt.Printf("invalid arguments rejected locally: %s\n", invalid.State)

	return nil
}

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatal(err)
	}
}
