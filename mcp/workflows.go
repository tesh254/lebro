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
// workflow. The arguments object carries the workflow input, optional thread
// ID, and optional metadata.
var workflowInputSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"input": {
			"description": "The JSON input to the first workflow step."
		},
		"thread_id": {
			"type": "string",
			"description": "Optional thread ID for durable workflow runs."
		},
		"metadata": {
			"type": "object",
			"description": "Optional run metadata.",
			"additionalProperties": {"type": "string"}
		}
	},
	"additionalProperties": false
}`)

// workflowResumeInputSchema is the JSON Schema for the MCP tool wrapping a
// suspended workflow resume operation.
var workflowResumeInputSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"run_id": {
			"type": "string",
			"description": "The ID of the suspended workflow run to resume."
		},
		"input": {
			"description": "The resume input validated against the suspend contract."
		},
		"metadata": {
			"type": "object",
			"description": "Optional metadata merged into the resumed run.",
			"additionalProperties": {"type": "string"}
		}
	},
	"required": ["run_id"],
	"additionalProperties": false
}`)

// workflowCallInput is the typed arguments for a workflow MCP tool.
type workflowCallInput struct {
	Input    json.RawMessage   `json:"input,omitempty"`
	ThreadID lebro.ThreadID    `json:"thread_id,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// workflowResumeCallInput is the typed arguments for a workflow resume MCP tool.
type workflowResumeCallInput struct {
	RunID    lebro.RunID       `json:"run_id"`
	Input    json.RawMessage   `json:"input,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// ExposeWorkflow registers a lebro LinearWorkflow as an MCP tool. The tool
// name is "workflow.<id>" where id is the workflow's definition ID. MCP clients
// invoke the workflow by calling the tool with input, optional thread ID, and
// optional metadata; the tool returns the workflow's final output.
//
// Only the run tool is registered. Use ExposeWorkflowResume to separately
// register the resume tool for workflows configured with a Store.
func (s *Server) ExposeWorkflow(wf *lebro.LinearWorkflow) error {
	if wf == nil {
		return errors.New("lebro/mcp: workflow is nil")
	}
	def := wf.Definition()
	toolName := "workflow." + string(def.ID)
	if err := s.registerName(toolName); err != nil {
		return err
	}

	s.mu.Lock()
	s.workflows[toolName] = wf
	s.mu.Unlock()

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
		var input workflowCallInput
		if err := json.Unmarshal(arguments, &input); err != nil {
			mcpResult := &mcpsdk.CallToolResult{}
			mcpResult.SetError(fmt.Errorf("lebro/mcp: invalid workflow arguments: %w", err))
			return mcpResult, nil
		}
		runInput := lebro.WorkflowRunInput{
			Input:    input.Input,
			ThreadID: input.ThreadID,
			Metadata: input.Metadata,
		}
		result, err := wf.Run(ctx, runInput)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			mcpResult := &mcpsdk.CallToolResult{}
			mcpResult.SetError(err)
			return mcpResult, nil
		}
		return workflowResultToMCP(result), nil
	}

	s.mcpServer.AddTool(mcpTool, handler)
	return nil
}

// ExposeWorkflowResume registers a resume tool for a lebro LinearWorkflow
// configured with a Store. The tool name is "workflow.<id>.resume". MCP
// clients invoke the tool with a run ID, resume input, and optional metadata
// to continue a previously suspended workflow run.
//
// ExposeWorkflowResume must be called after ExposeWorkflow for the same
// workflow. Calling it for a workflow without a bound Store will succeed at
// registration time but every invocation will return
// ErrWorkflowResumeRequiresStore.
func (s *Server) ExposeWorkflowResume(wf *lebro.LinearWorkflow) error {
	if wf == nil {
		return errors.New("lebro/mcp: workflow is nil")
	}
	def := wf.Definition()
	runName := "workflow." + string(def.ID)
	resumeName := runName + ".resume"

	s.mu.Lock()
	exposedWf := s.workflows[runName]
	if exposedWf == nil {
		s.mu.Unlock()
		return fmt.Errorf("lebro/mcp: ExposeWorkflowResume requires ExposeWorkflow to be called first for %q", def.ID)
	}
	if exposedWf != wf {
		s.mu.Unlock()
		return fmt.Errorf("lebro/mcp: ExposeWorkflowResume received a different workflow instance for %q", def.ID)
	}
	s.mu.Unlock()

	if err := s.registerName(resumeName); err != nil {
		return err
	}

	resumeTool := &mcpsdk.Tool{
		Name:        resumeName,
		Description: fmt.Sprintf("Resume suspended workflow %s", def.ID),
		InputSchema: workflowResumeInputSchema,
	}
	resumeHandler := func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		arguments := req.Params.Arguments
		if len(arguments) == 0 {
			arguments = json.RawMessage(`{}`)
		}
		var input workflowResumeCallInput
		if err := json.Unmarshal(arguments, &input); err != nil {
			mcpResult := &mcpsdk.CallToolResult{}
			mcpResult.SetError(fmt.Errorf("lebro/mcp: invalid resume arguments: %w", err))
			return mcpResult, nil
		}
		result, err := wf.Resume(ctx, lebro.WorkflowResumeInput{
			RunID:    input.RunID,
			Input:    input.Input,
			Metadata: input.Metadata,
		})
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			mcpResult := &mcpsdk.CallToolResult{}
			mcpResult.SetError(err)
			return mcpResult, nil
		}
		return workflowResultToMCP(result), nil
	}
	s.mcpServer.AddTool(resumeTool, resumeHandler)
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
		mcpResult.StructuredContent = json.RawMessage(result.Output)
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
