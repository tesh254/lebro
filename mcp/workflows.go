package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tesh254/lebro"
)

var workflowCallInputSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"input": {
			"description": "The JSON input to the first workflow step."
		}
	},
	"additionalProperties": false
}`)

var workflowCallInputCompiled = mustCompileMCPInputSchema(workflowCallInputSchema)

func workflowToolInputSchema(toolName string, inputSchema json.RawMessage) json.RawMessage {
	input := json.RawMessage(`{"description":"The JSON input to the first workflow step."}`)
	if len(inputSchema) > 0 {
		input = workflowInputPropertySchema(toolName, inputSchema)
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]json.RawMessage{
			"input": input,
		},
		"additionalProperties": false,
	}
	if len(inputSchema) > 0 {
		schema["required"] = []string{"input"}
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		panic(fmt.Sprintf("lebro/mcp: encode workflow input schema: %v", err))
	}
	return encoded
}

// workflowInputPropertySchema gives embedded schemas their own resource root,
// so local references such as #/$defs/request keep resolving inside the entry
// schema rather than against the surrounding MCP tool schema.
func workflowInputPropertySchema(toolName string, inputSchema json.RawMessage) json.RawMessage {
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(inputSchema, &schema); err != nil {
		return append(json.RawMessage(nil), inputSchema...)
	}
	if _, hasID := schema["$id"]; hasID {
		return append(json.RawMessage(nil), inputSchema...)
	}
	id, err := json.Marshal(fmt.Sprintf("urn:lebro:mcp:workflow-input:%x", toolName))
	if err != nil {
		panic(fmt.Sprintf("lebro/mcp: encode workflow input schema ID: %v", err))
	}
	schema["$id"] = id
	encoded, err := json.Marshal(schema)
	if err != nil {
		panic(fmt.Sprintf("lebro/mcp: encode workflow input schema: %v", err))
	}
	return encoded
}

// workflowCallInput is the typed arguments for a workflow MCP tool.
type workflowCallInput struct {
	Input json.RawMessage `json:"input,omitempty"`
}

// ExposeWorkflow registers a lebro LinearWorkflow as an MCP tool. The tool
// name is "workflow.<id>" where id is the workflow's definition ID. MCP clients
// invoke the workflow by calling the tool with input; the tool returns the
// workflow's final output.
func (s *Server) ExposeWorkflow(wf *lebro.LinearWorkflow) error {
	if wf == nil {
		return errors.New("lebro/mcp: workflow is nil")
	}
	def := wf.Definition()
	toolName := "workflow." + string(def.ID)
	if err := s.registerName(toolName); err != nil {
		return err
	}
	inputSchema := workflowToolInputSchema(toolName, wf.InputSchema())

	mcpTool := &mcpsdk.Tool{
		Name:        toolName,
		Description: def.Description,
		InputSchema: inputSchema,
	}

	handler := func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		arguments := req.Params.Arguments
		if len(arguments) == 0 {
			arguments = json.RawMessage(`{}`)
		}
		if err := workflowCallInputCompiled.Validate(arguments); err != nil {
			mcpResult := &mcpsdk.CallToolResult{}
			mcpResult.SetError(fmt.Errorf("lebro/mcp: invalid workflow arguments: %w", err))
			return mcpResult, nil
		}
		var input workflowCallInput
		if err := json.Unmarshal(arguments, &input); err != nil {
			mcpResult := &mcpsdk.CallToolResult{}
			mcpResult.SetError(fmt.Errorf("lebro/mcp: invalid workflow arguments: %w", err))
			return mcpResult, nil
		}
		if err := wf.ValidateInput(input.Input); err != nil {
			mcpResult := &mcpsdk.CallToolResult{}
			mcpResult.SetError(fmt.Errorf("lebro/mcp: invalid workflow input: %w", err))
			return mcpResult, nil
		}
		runInput := lebro.WorkflowRunInput{
			Input: input.Input,
		}
		result, err := wf.Run(ctx, runInput)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			return toolError("workflow execution failed"), nil
		}
		return workflowResultToMCP(result), nil
	}

	s.mcpServer.AddTool(mcpTool, handler)
	return nil
}

// workflowResultToMCP converts a lebro WorkflowRunResult to an MCP
// CallToolResult. The output is returned as structured content and text. When
// the workflow suspends, the suspend details identify the durable run, but
// resume is not available through MCP.
func workflowResultToMCP(result lebro.WorkflowRunResult) *mcpsdk.CallToolResult {
	mcpResult := &mcpsdk.CallToolResult{}
	if result.Status == lebro.RunStatusSuspended && result.Suspend != nil {
		suspendInfo, _ := json.Marshal(map[string]any{
			"run_id":           string(result.ID),
			"status":           string(result.Status),
			"step_id":          string(result.Suspend.StepID),
			"step":             result.Suspend.Step,
			"contract":         json.RawMessage(result.Suspend.Contract),
			"resume_available": false,
		})
		mcpResult.Content = []mcpsdk.Content{
			&mcpsdk.TextContent{Text: string(suspendInfo)},
		}
		mcpResult.StructuredContent = json.RawMessage(suspendInfo)
		return mcpResult
	}
	if len(result.Output) > 0 {
		if json.Valid(result.Output) {
			mcpResult.StructuredContent = json.RawMessage(result.Output)
		}
		mcpResult.Content = []mcpsdk.Content{
			&mcpsdk.TextContent{Text: string(result.Output)},
		}
	} else {
		info, _ := json.Marshal(map[string]any{
			"run_id": string(result.ID),
			"status": string(result.Status),
		})
		mcpResult.Content = []mcpsdk.Content{
			&mcpsdk.TextContent{Text: string(info)},
		}
	}
	return mcpResult
}
