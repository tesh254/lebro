package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tesh254/lebro"
)

// ExposeTool registers a lebro RegisteredTool for MCP clients to discover and
// invoke. The tool's InputSchema must be a valid JSON Schema with type
// "object"; an empty InputSchema defaults to {"type":"object"}. Only
// explicitly exposed tools are visible to MCP clients.
func (s *Server) ExposeTool(tool *lebro.RegisteredTool) error {
	if tool == nil {
		return errors.New("lebro/mcp: tool is nil")
	}
	def := tool.Definition()

	inputSchema, err := normalizeInputSchema(def.InputSchema)
	if err != nil {
		return fmt.Errorf("lebro/mcp: tool %q: %w", def.ID, err)
	}

	var outputSchema json.RawMessage
	if len(def.OutputSchema) > 0 {
		outputSchema, err = normalizeOutputSchema(def.OutputSchema)
		if err != nil {
			return fmt.Errorf("lebro/mcp: tool %q: %w", def.ID, err)
		}
	}

	if err := s.registerName(string(def.ID)); err != nil {
		return err
	}

	mcpTool := &mcpsdk.Tool{
		Name:        string(def.ID),
		Description: def.Description,
		InputSchema: inputSchema,
	}
	if len(outputSchema) > 0 {
		mcpTool.OutputSchema = outputSchema
	}

	handler := func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		arguments := req.Params.Arguments
		if len(arguments) == 0 {
			arguments = json.RawMessage(`{}`)
		}
		result := tool.Execute(ctx, lebro.ToolExecutionRequest{
			Arguments: arguments,
		})
		return toolResultToMCP(result)
	}

	s.mcpServer.AddTool(mcpTool, handler)
	return nil
}

// toolResultToMCP converts a lebro ToolExecutionResult to an MCP CallToolResult.
// Succeeded results always carry text content and carry structured content only
// when the output is valid JSON. Tool-level errors (invalid input/output,
// handler errors, panics) are returned as CallToolResult with IsError=true so
// the LLM can observe and self-correct. Cancellation is returned as a
// protocol-level error.
func toolResultToMCP(result lebro.ToolExecutionResult) (*mcpsdk.CallToolResult, error) {
	switch result.State {
	case lebro.ToolExecutionSucceeded:
		mcpResult := &mcpsdk.CallToolResult{}
		if len(result.Output) > 0 {
			if json.Valid(result.Output) {
				mcpResult.StructuredContent = json.RawMessage(result.Output)
			}
			mcpResult.Content = []mcpsdk.Content{
				&mcpsdk.TextContent{Text: string(result.Output)},
			}
		} else {
			mcpResult.Content = []mcpsdk.Content{
				&mcpsdk.TextContent{Text: "{}"},
			}
		}
		return mcpResult, nil
	case lebro.ToolExecutionCancelled:
		if errors.Is(result.Err, context.DeadlineExceeded) {
			return nil, context.DeadlineExceeded
		}
		return nil, context.Canceled
	case lebro.ToolExecutionNotFound:
		return nil, fmt.Errorf("lebro/mcp: tool unavailable")
	case lebro.ToolExecutionInvalidInput:
		return toolError("invalid tool arguments"), nil
	case lebro.ToolExecutionInvalidOutput:
		return toolError("tool returned invalid output"), nil
	default:
		return toolError("tool execution failed"), nil
	}
}

// normalizeInputSchema ensures the schema is valid JSON with type "object".
// An empty schema defaults to {"type":"object"}.
func normalizeInputSchema(schema json.RawMessage) (json.RawMessage, error) {
	if len(schema) == 0 {
		return json.RawMessage(`{"type":"object"}`), nil
	}
	var m map[string]any
	if err := json.Unmarshal(schema, &m); err != nil {
		return nil, fmt.Errorf("input schema is invalid JSON: %w", err)
	}
	if m == nil {
		return json.RawMessage(`{"type":"object"}`), nil
	}
	if typ, ok := m["type"]; ok {
		if s, ok := typ.(string); ok && s == "object" {
			return schema, nil
		}
		return nil, fmt.Errorf("input schema type must be %q, got %v", "object", typ)
	}
	m["type"] = "object"
	normalized, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("normalize input schema: %w", err)
	}
	return normalized, nil
}

// normalizeOutputSchema validates that the schema is valid JSON. Any JSON
// Schema type is acceptable for output schemas.
func normalizeOutputSchema(schema json.RawMessage) (json.RawMessage, error) {
	if len(schema) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(schema, &v); err != nil {
		return nil, fmt.Errorf("output schema is invalid JSON: %w", err)
	}
	return schema, nil
}
