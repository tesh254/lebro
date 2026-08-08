package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// AgentStep adapts an Agent-compatible Workflow to a JSON workflow StepHandler.
// It turns each workflow value into a user message and returns the nested
// agent's structured output, or its final assistant content as a JSON string.
// Thread and metadata values from WorkflowRunInput are forwarded to RunInput.
type AgentStep struct {
	agent Workflow
}

// NewAgentStep returns an adapter that invokes agent as a workflow step.
func NewAgentStep(agent Workflow) (*AgentStep, error) {
	if agent == nil || isNilInterface(agent) {
		return nil, errors.New("lebro: workflow agent is required")
	}
	return &AgentStep{agent: agent}, nil
}

// Execute maps input to one user message, runs the nested agent with the same
// context, and converts its terminal response to the next workflow value.
func (s *AgentStep) Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	if s == nil || s.agent == nil || isNilInterface(s.agent) {
		return nil, errors.New("lebro: workflow agent step is nil")
	}
	if ctx == nil {
		return nil, errors.New("lebro: workflow agent step context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	invocation := workflowInvocationFromContext(ctx)
	result, err := s.agent.Run(ctx, RunInput{
		Messages: []Message{{Role: RoleUser, Content: workflowAgentPrompt(input)}},
		ThreadID: invocation.threadID,
		Metadata: invocation.metadata,
	})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result.Status != RunStatusSucceeded {
		return nil, fmt.Errorf("lebro: workflow agent step finished with status %q", result.Status)
	}
	if output := result.StructuredOutput(); output != "" {
		return cloneRawMessage(output.Raw()), nil
	}
	for i := len(result.Messages) - 1; i >= 0; i-- {
		if result.Messages[i].Role == RoleAssistant {
			return json.Marshal(result.Messages[i].Content)
		}
	}
	return nil, errors.New("lebro: workflow agent step returned no assistant message")
}

func workflowAgentPrompt(input json.RawMessage) string {
	var text string
	if json.Unmarshal(input, &text) == nil {
		return text
	}
	return string(input)
}

// ToolStep adapts a registered, schema-checked Tool to a workflow StepHandler.
// Workflow input becomes tool arguments and successful tool output becomes the
// next workflow value. Workflow metadata is available to the tool handler.
type ToolStep struct {
	tool *RegisteredTool
}

// NewToolStep returns an adapter for a registered tool. Registering the tool
// first ensures its input and output schema boundary remains enforced.
func NewToolStep(tool *RegisteredTool) (*ToolStep, error) {
	if tool == nil {
		return nil, errors.New("lebro: workflow tool is required")
	}
	return &ToolStep{tool: tool}, nil
}

// Execute invokes the registered tool with workflow input and metadata.
func (s *ToolStep) Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	if s == nil || s.tool == nil {
		return nil, errors.New("lebro: workflow tool step is nil")
	}
	if ctx == nil {
		return nil, errors.New("lebro: workflow tool step context is nil")
	}
	result := s.tool.Execute(ctx, ToolExecutionRequest{
		Arguments: cloneRawMessage(input),
		Metadata:  workflowInvocationFromContext(ctx).metadata,
	})
	if result.State != ToolExecutionSucceeded {
		return nil, result.Err
	}
	return cloneRawMessage(result.Output), nil
}

type workflowInvocation struct {
	runID    RunID
	step     int
	stepID   StepID
	threadID ThreadID
	metadata map[string]string
}

type workflowInvocationContextKey struct{}

func withWorkflowInvocation(ctx context.Context, runID RunID, step int, stepID StepID, threadID ThreadID, metadata map[string]string) context.Context {
	return context.WithValue(ctx, workflowInvocationContextKey{}, workflowInvocation{
		runID:    runID,
		step:     step,
		stepID:   stepID,
		threadID: threadID,
		metadata: cloneMetadata(metadata),
	})
}

func workflowInvocationFromContext(ctx context.Context) workflowInvocation {
	if ctx == nil {
		return workflowInvocation{}
	}
	invocation, _ := ctx.Value(workflowInvocationContextKey{}).(workflowInvocation)
	invocation.metadata = cloneMetadata(invocation.metadata)
	return invocation
}
