package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

// agentInputSchema is the JSON Schema for the MCP tool wrapping a lebro agent.
// Clients may supply only user text. Runtime message roles and transcript-only
// fields remain application-controlled, preventing schema drift from
// lebro.Message and prompt or tool-call injection.
var agentInputSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"messages": {
			"type": "array",
			"description": "Conversation messages to seed the agent run.",
			"items": {
				"type": "object",
				"properties": {
					"content": {"type": "string"}
				},
				"required": ["content"],
				"additionalProperties": false
			}
		}
	},
	"additionalProperties": false
}`)

// agentInputCompiled is the compiled agent input schema, used to validate
// MCP tool arguments before the agent runs.
var agentInputCompiled = mustCompileSchema(agentInputSchema)

func mustCompileSchema(schema json.RawMessage) lebro.CompiledSchema {
	compiler := lebrojsonschema.NewCompiler()
	compiled, err := compiler.Compile(schema)
	if err != nil {
		panic(fmt.Sprintf("lebro/mcp: compile agent input schema: %v", err))
	}
	return compiled
}

// agentCallInput is the typed arguments for an agent MCP tool.
type agentCallInput struct {
	Messages []agentMessageInput `json:"messages"`
}

type agentMessageInput struct {
	Content string `json:"content"`
}

// ExposeAgent registers a lebro Agent as an MCP tool. The tool name is
// "agent.<id>" where id is the agent's definition ID. MCP clients invoke the
// agent by calling the tool with user messages; the tool returns the agent's
// terminal response.
func (s *Server) ExposeAgent(agent *lebro.Agent) error {
	if agent == nil {
		return errors.New("lebro/mcp: agent is nil")
	}
	def := agent.Definition()
	toolName := "agent." + string(def.ID)
	if err := s.registerName(toolName); err != nil {
		return err
	}

	mcpTool := &mcpsdk.Tool{
		Name:        toolName,
		Description: def.Description,
		InputSchema: agentInputSchema,
	}

	handler := func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		arguments := req.Params.Arguments
		if len(arguments) == 0 {
			arguments = json.RawMessage(`{}`)
		}
		if err := agentInputCompiled.Validate(arguments); err != nil {
			mcpResult := &mcpsdk.CallToolResult{}
			mcpResult.SetError(fmt.Errorf("lebro/mcp: invalid agent arguments: %w", err))
			return mcpResult, nil
		}
		var input agentCallInput
		if err := json.Unmarshal(arguments, &input); err != nil {
			mcpResult := &mcpsdk.CallToolResult{}
			mcpResult.SetError(fmt.Errorf("lebro/mcp: invalid agent arguments: %w", err))
			return mcpResult, nil
		}
		messages := make([]lebro.Message, 0, len(input.Messages))
		for _, msg := range input.Messages {
			messages = append(messages, lebro.Message{Role: lebro.RoleUser, Content: msg.Content})
		}
		runInput := lebro.RunInput{
			Messages: messages,
		}
		result, err := agent.Run(ctx, runInput)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			return toolError("agent execution failed"), nil
		}
		return agentResultToMCP(result), nil
	}

	s.mcpServer.AddTool(mcpTool, handler)
	return nil
}

// agentResultToMCP converts a lebro RunResult to an MCP CallToolResult. The
// terminal assistant message content is returned as text content, and when
// structured output is present it is included as structured content.
func agentResultToMCP(result lebro.RunResult) *mcpsdk.CallToolResult {
	mcpResult := &mcpsdk.CallToolResult{}
	var text string
	for i := len(result.Messages) - 1; i >= 0; i-- {
		msg := result.Messages[i]
		if msg.Role == lebro.RoleAssistant {
			text = msg.Content
			if msg.StructuredOutput != "" {
				mcpResult.StructuredContent = json.RawMessage(msg.StructuredOutput)
			}
			break
		}
	}
	if text == "" {
		if mcpResult.StructuredContent != nil {
			text = string(mcpResult.StructuredContent.(json.RawMessage))
		} else {
			text = fmt.Sprintf(`{"run_id":%q,"status":%q}`, result.ID, result.Status)
		}
	}
	mcpResult.Content = []mcpsdk.Content{
		&mcpsdk.TextContent{Text: text},
	}
	return mcpResult
}
