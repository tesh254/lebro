package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tesh254/lebro"
)

// workflowInputSchema is the JSON Schema for the MCP tool wrapping a lebro
// workflow. The arguments object carries the workflow input.
var workflowInputSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"input": {
			"description": "The JSON input to the first workflow step."
		}
	},
	"additionalProperties": false
}`)

var workflowInputCompiled = mustCompileMCPInputSchema(workflowInputSchema)

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

	mcpTool := &mcpsdk.Tool{
		Name:        toolName,
		Description: def.Description,
		InputSchema: workflowInputSchema,
	}

	handler := func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		arguments := req.Params.Arguments
		if len(arguments) == 0 {
			arguments = json.RawMessage(`{}`)
		}
		if err := workflowInputCompiled.Validate(arguments); err != nil {
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
// the workflow suspended, the suspend details are included so the client can
// resume.
func workflowResultToMCP(result lebro.WorkflowRunResult) *mcpsdk.CallToolResult {
	mcpResult := &mcpsdk.CallToolResult{}
	if result.Status == lebro.RunStatusSuspended && result.Suspend != nil {
		suspendInfo, _ := json.Marshal(map[string]any{
			"run_id":   string(result.ID),
			"status":   string(result.Status),
			"step_id":  string(result.Suspend.StepID),
			"step":     result.Suspend.Step,
			"contract": json.RawMessage(result.Suspend.Contract),
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
