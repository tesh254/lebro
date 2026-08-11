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
// The arguments object carries messages, optional thread ID, and optional
// metadata. Message items mirror lebro.Message fields including tool_calls,
// structured_output, tool_call_id, and name so clients can provide complete
// prior transcripts.
var agentInputSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"messages": {
			"type": "array",
			"description": "Conversation messages to seed the agent run.",
			"items": {
				"type": "object",
				"properties": {
					"role": {
						"type": "string",
						"enum": ["system", "user", "assistant", "tool"]
					},
					"content": {"type": "string"},
					"name": {"type": "string"},
					"tool_call_id": {
						"type": "string",
						"description": "Required when role is 'tool'; links the tool result to the originating tool call."
					},
					"tool_calls": {
						"type": "array",
						"description": "Tool calls requested by an assistant message.",
						"items": {
							"type": "object",
							"properties": {
								"id": {"type": "string"},
								"tool_id": {"type": "string"},
								"arguments": {}
							},
							"required": ["id", "tool_id"]
						}
					},
					"structured_output": {
						"description": "Structured JSON output from an assistant message."
					}
				},
				"required": ["role", "content"]
			}
		},
		"thread_id": {
			"type": "string",
			"description": "Optional thread ID for persistent conversation history."
		},
		"metadata": {
			"type": "object",
			"description": "Optional run metadata.",
			"additionalProperties": {"type": "string"}
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
	Messages []lebro.Message   `json:"messages"`
	ThreadID lebro.ThreadID    `json:"thread_id,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// ExposeAgent registers a lebro Agent as an MCP tool. The tool name is
// "agent.<id>" where id is the agent's definition ID. MCP clients invoke the
// agent by calling the tool with messages, optional thread ID, and optional
// metadata; the tool returns the agent's terminal response.
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
		filteredMessages := make([]lebro.Message, 0, len(input.Messages))
		for _, msg := range input.Messages {
			if msg.Role == lebro.RoleSystem {
				continue
			}
			filteredMessages = append(filteredMessages, msg)
		}
		runInput := lebro.RunInput{
			Messages: filteredMessages,
			ThreadID: input.ThreadID,
			Metadata: input.Metadata,
		}
		result, err := agent.Run(ctx, runInput)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			mcpResult := &mcpsdk.CallToolResult{}
			mcpResult.SetError(err)
			return mcpResult, nil
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
	if text == "" && mcpResult.StructuredContent == nil {
		text = fmt.Sprintf(`{"run_id":%q,"status":%q}`, result.ID, result.Status)
	}
	mcpResult.Content = []mcpsdk.Content{
		&mcpsdk.TextContent{Text: text},
	}
	return mcpResult
}
