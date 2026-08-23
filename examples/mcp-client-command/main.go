// mcp-client-command connects a lebro MCP client to a real stdio MCP server
// process. It discovers remote tools, registers their validated adapters, and
// optionally invokes one. Build examples/mcp-server first to try it locally.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
	"github.com/tesh254/lebro/mcp"
)

func main() {
	command := flag.String("command", "", "MCP server executable")
	serverName := flag.String("server-name", "remote", "local namespace for discovered tools")
	toolID := flag.String("tool", "", "remote tool ID to invoke")
	arguments := flag.String("arguments", "{}", "JSON object supplied to -tool")
	flag.Parse()
	if *command == "" {
		log.Fatal("-command is required")
	}
	if err := run(context.Background(), os.Stdout, *command, flag.Args(), *serverName, *toolID, json.RawMessage(*arguments)); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, output io.Writer, command string, args []string, serverName, toolID string, arguments json.RawMessage) error {
	client := mcp.NewClient(mcp.ClientConfig{
		Implementation: &mcpsdk.Implementation{Name: "lebro-mcp-command-client", Version: "1.0.0"},
		ServerName:     serverName,
	})
	if err := client.Connect(ctx, &mcpsdk.CommandTransport{Command: exec.CommandContext(ctx, command, args...)}); err != nil {
		return fmt.Errorf("connect to %q: %w", command, err)
	}
	defer func() { _ = client.Close() }()

	tools, err := client.DiscoverTools(ctx)
	if err != nil {
		return fmt.Errorf("discover tools: %w", err)
	}
	registry, err := lebro.NewToolRegistry(lebrojsonschema.NewCompiler())
	if err != nil {
		return fmt.Errorf("new registry: %w", err)
	}
	for _, tool := range tools {
		definition := tool.Definition()
		if err := registry.Register(tool); err != nil {
			return fmt.Errorf("register %q: %w", definition.ID, err)
		}
		if _, err := fmt.Fprintf(output, "discovered %s\n", definition.ID); err != nil {
			return fmt.Errorf("write discovery output: %w", err)
		}
	}
	if toolID == "" {
		return nil
	}

	localToolID := lebro.ToolID(serverName + "." + toolID)
	result := registry.Execute(ctx, localToolID, lebro.ToolExecutionRequest{Arguments: arguments})
	if result.State != lebro.ToolExecutionSucceeded {
		return fmt.Errorf("execute remote %q as %q: %s: %w", toolID, localToolID, result.State, result.Err)
	}
	fmt.Fprintf(output, "result: %s\n", result.Output)
	return nil
}
