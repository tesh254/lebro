package httpapi

import (
	"encoding/json"

	"github.com/tesh254/lebro"
)

// The wire types below are the published HTTP contract. They are deliberately
// separate from the runtime structs they are built from: a field added to
// lebro.RunResult must not silently change what this package serves, and a
// caller decoding these types must not be exposed to transcript-only runtime
// fields.

// MessageInput is one caller-supplied conversation message. Only user text is
// accepted; the role is fixed by the server, so a client cannot inject a system
// prompt or forge an assistant turn.
type MessageInput struct {
	Content string `json:"content"`
}

// RunRequest is the body of an agent run. Messages seed the run and Metadata is
// carried through to tool execution and run events. A thread is selected with
// the thread_id query parameter rather than a body field, so the durable
// conversation a run binds to is part of the addressed resource.
type RunRequest struct {
	Messages []MessageInput    `json:"messages"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// Usage reports provider token accounting for a run. Values are zero when the
// provider does not report usage.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

// RunResponse is a completed agent run. Content is the terminal assistant
// message text and StructuredOutput is its structured payload when the agent
// declares an output schema; both may be empty, and a run that produced neither
// is still reported with its status.
//
// Token usage is deliberately absent. lebro.RunResult does not carry an
// aggregate: usage is reported per model call through the run event stream, and
// an agent's RunListener is fixed at construction rather than per call, so this
// package cannot attach one for a single request without mutating state shared
// by concurrent requests. Serving a field that was always zero would be worse
// than omitting it. Applications that need usage configure a RunListener on the
// agent, which sees every call in every run. Streamed runs do carry per-call
// usage, on the delta the provider reports it on.
type RunResponse struct {
	RunID            string          `json:"run_id"`
	Status           string          `json:"status"`
	Content          string          `json:"content"`
	StructuredOutput json.RawMessage `json:"structured_output,omitempty"`
}

// WorkflowRunRequest is the body of a workflow run. Input is the JSON value
// passed to the first step and is validated against that step's declared input
// schema before the run starts.
type WorkflowRunRequest struct {
	Input    json.RawMessage   `json:"input,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// SuspendResponse identifies a workflow run that suspended at a step boundary.
// Resume is not available over HTTP, so ResumeAvailable is always false; it is
// present so a client can branch on the field rather than on the route's
// absence.
type SuspendResponse struct {
	Step            int             `json:"step"`
	StepID          string          `json:"step_id"`
	Contract        json.RawMessage `json:"contract,omitempty"`
	ResumeAvailable bool            `json:"resume_available"`
}

// WorkflowRunResponse is a completed or suspended workflow run. Output is the
// final step's validated output and is empty when the run suspended or failed;
// Suspend is non-nil only when Status is "suspended".
type WorkflowRunResponse struct {
	RunID   string           `json:"run_id"`
	Status  string           `json:"status"`
	Output  json.RawMessage  `json:"output,omitempty"`
	Path    []string         `json:"path,omitempty"`
	Suspend *SuspendResponse `json:"suspend,omitempty"`
}

// AgentSummary describes one exposed agent.
type AgentSummary struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// AgentListResponse enumerates every exposed agent.
type AgentListResponse struct {
	Agents []AgentSummary `json:"agents"`
}

// WorkflowSummary describes one exposed workflow. InputSchema is the first
// step's declared input schema and is absent when the workflow accepts any
// input.
type WorkflowSummary struct {
	ID          string          `json:"id"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Version     string          `json:"version,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// WorkflowListResponse enumerates every exposed workflow.
type WorkflowListResponse struct {
	Workflows []WorkflowSummary `json:"workflows"`
}

// ThreadResponse is a durable conversation's metadata. Messages are served
// separately so a long-lived thread is paged rather than returned whole.
type ThreadResponse struct {
	ID        string          `json:"id"`
	Namespace string          `json:"namespace,omitempty"`
	OwnerID   string          `json:"owner_id,omitempty"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
}

// MessageResponse is one durable message in a thread.
type MessageResponse struct {
	ID        string `json:"id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

// MessageListResponse is one page of a thread's messages. NextCursor is empty
// when the page is the last one.
type MessageListResponse struct {
	Messages   []MessageResponse `json:"messages"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

// HealthResponse reports server readiness and what it currently exposes.
type HealthResponse struct {
	Status    string `json:"status"`
	Agents    int    `json:"agents"`
	Workflows int    `json:"workflows"`
}

// StreamEvent is one Server-Sent Event payload on the stream route. Type
// mirrors the event name so a client that buffers data lines without reading
// event names still sees the type. Text, ToolCall, and StructuredOutput carry a
// model delta; RunID and Status are populated on the terminal event; Error
// carries the public error code for a failed or cancelled run.
//
// Usage appears on a delta when the provider reports per-call figures, and on
// the terminal event as the run total across every model call. A client that
// treats the terminal event as the single end-of-run marker therefore finds the
// full accounting there without summing deltas itself.
//
// There is no field for a delta-level error: a provider stream that aborts is
// reported once, through the terminal event's Error, rather than as an
// otherwise-empty delta followed by the real classification.
type StreamEvent struct {
	Type             string          `json:"type"`
	RunID            string          `json:"run_id,omitempty"`
	Text             string          `json:"text,omitempty"`
	ToolCall         *ToolCallEvent  `json:"tool_call,omitempty"`
	StructuredOutput json.RawMessage `json:"structured_output,omitempty"`
	FinishReason     string          `json:"finish_reason,omitempty"`
	Status           string          `json:"status,omitempty"`
	Usage            *Usage          `json:"usage,omitempty"`
	Error            *ErrorBody      `json:"error,omitempty"`
}

// ToolCallEvent is a model-requested tool invocation on the stream. Arguments
// is present only when the configured Redactor passes it through;
// DefaultRedactor removes it.
type ToolCallEvent struct {
	ID        string          `json:"id"`
	ToolID    string          `json:"tool_id"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ErrorBody is the public description of a failure. Code is a stable
// machine-readable value; Message is a fixed public string for that code and
// never carries internal error text.
type ErrorBody struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

// ErrorResponse is the body of every non-2xx response.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// usageFromModel converts runtime token accounting to the wire shape.
func usageFromModel(usage lebro.ModelUsage) Usage {
	return Usage{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		TotalTokens:  usage.TotalTokens,
	}
}

// runResponseFromResult projects a completed run onto the wire contract. It
// reports the terminal assistant message, which is the last assistant turn in
// the transcript; a run whose transcript has no assistant message yields empty
// content rather than leaking a tool or system turn.
func runResponseFromResult(result lebro.RunResult) RunResponse {
	response := RunResponse{
		RunID:  string(result.ID),
		Status: string(result.Status),
	}
	for i := len(result.Messages) - 1; i >= 0; i-- {
		message := result.Messages[i]
		if message.Role != lebro.RoleAssistant {
			continue
		}
		response.Content = message.Content
		if message.StructuredOutput != "" {
			response.StructuredOutput = message.StructuredOutput.Raw()
		}
		break
	}
	return response
}

// workflowRunResponseFromResult projects a workflow run onto the wire contract.
// A suspended run reports its resume contract but never claims resume is
// available, because this package does not expose a resume route.
func workflowRunResponseFromResult(result lebro.WorkflowRunResult) WorkflowRunResponse {
	response := WorkflowRunResponse{
		RunID:  string(result.ID),
		Status: string(result.Status),
		Output: result.Output,
	}
	for _, stepID := range result.Path {
		response.Path = append(response.Path, string(stepID))
	}
	if result.Status == lebro.RunStatusSuspended && result.Suspend != nil {
		response.Suspend = &SuspendResponse{
			Step:            result.Suspend.Step,
			StepID:          string(result.Suspend.StepID),
			Contract:        result.Suspend.Contract,
			ResumeAvailable: false,
		}
	}
	return response
}
