package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Stable identifiers make runtime records portable across model providers,
// storage adapters, and transport layers.
type (
	AgentID             string
	RunID               string
	StepID              string
	ThreadID            string
	ToolID              string
	WorkflowID          string
	ScheduleID          string
	ScheduleExecutionID string
)

// ErrMessageStructuredOutputInvalidJSON is returned by Message.Validate when
// structured output is present but not valid JSON. It lets callers distinguish
// a structured-output JSON defect from other message validation failures.
var ErrMessageStructuredOutputInvalidJSON = errors.New("lebro: message structured output must be valid JSON")

// Role identifies the author of a message in an agent conversation.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is the provider-neutral representation of a conversation message.
// Assistant tool requests use ToolCalls; tool results use ToolCallID. Name is
// the optional author name and is not a tool-call representation.
type Message struct {
	Role             Role                  `json:"role"`
	Content          string                `json:"content"`
	Name             string                `json:"name,omitempty"`
	ToolCallID       string                `json:"tool_call_id,omitempty"`
	ToolCalls        ModelToolCalls        `json:"tool_calls,omitempty,omitzero"`
	StructuredOutput ModelStructuredOutput `json:"structured_output,omitempty"`
	Reasoning        ModelReasoning        `json:"reasoning,omitempty,omitzero"`
}

// Validate checks invariants that every provider adapter must preserve.
func (m Message) Validate() error {
	switch m.Role {
	case RoleSystem, RoleUser, RoleAssistant, RoleTool:
	default:
		return fmt.Errorf("lebro: invalid message role %q", m.Role)
	}

	if m.Role == RoleTool && m.ToolCallID == "" {
		return errors.New("lebro: tool messages require a tool call ID")
	}
	if m.Role != RoleTool && m.ToolCallID != "" {
		return errors.New("lebro: only tool messages can contain a tool call ID")
	}
	toolCalls := m.ToolCalls.Values()
	if len(toolCalls) > 0 && m.Role != RoleAssistant {
		return errors.New("lebro: only assistant messages can contain tool calls")
	}
	if m.StructuredOutput != "" {
		if m.Role != RoleAssistant {
			return errors.New("lebro: only assistant messages can contain structured output")
		}
		if !json.Valid(m.StructuredOutput.Raw()) {
			return ErrMessageStructuredOutputInvalidJSON
		}
	}
	if !m.Reasoning.IsZero() {
		if m.Role != RoleAssistant {
			return errors.New("lebro: only assistant messages can contain reasoning")
		}
		if err := m.Reasoning.Validate(); err != nil {
			return err
		}
	}

	return nil
}

// AgentDefinition describes an agent independently of the model adapter that
// eventually executes it.
type AgentDefinition struct {
	ID           AgentID
	Name         string
	Instructions string
	Model        string
	Tools        []ToolID
}

// RunInput is the common input shape for future agent and workflow runs.
type RunInput struct {
	Messages []Message
	ThreadID ThreadID
	// RunID optionally supplies the durable identity for this run. It is
	// validated before any model call; an empty value uses the IDSource.
	RunID        RunID
	Metadata     map[string]string
	OutputSchema *ModelOutputSchema
	// Annotations attaches validated, namespaced application metadata to the
	// durable records created for this run: transcript messages, model
	// attempts, tool executions, and run events. It is validated before the
	// run starts; nil leaves every record untouched.
	Annotations Metadata
	// ObservabilityScope isolates durable diagnostics that may exist before a
	// thread is created (for example, a failed authorization or cancellation).
	// When a thread already exists, its namespace and owner take precedence.
	ObservabilityScope ObservabilityScope
	// Reasoning configures reasoning for every model call in this run. Provider
	// adapters reject settings their selected model cannot represent.
	Reasoning ReasoningConfig
	// Memory overrides the agent's Memory configuration for this run. A nil
	// value retains the agent default; an explicit configuration is scoped to
	// this run only.
	Memory         *MemoryProcessorConfig
	memoryRecalled bool
}

// ObservabilityScope is retained for source compatibility. It is the same
// reusable RuntimeScope used by workflow and scheduling persistence.
type ObservabilityScope = RuntimeScope

// RunStatus identifies the terminal or in-progress state of a run.
type RunStatus string

const (
	RunStatusPending   RunStatus = "pending"
	RunStatusRunning   RunStatus = "running"
	RunStatusSucceeded RunStatus = "succeeded"
	RunStatusFailed    RunStatus = "failed"
	RunStatusCancelled RunStatus = "cancelled"
	RunStatusSuspended RunStatus = "suspended"
)

// RunResult is the provider-neutral result shape returned by executable
// primitives. The executor is intentionally introduced in a later task.
type RunResult struct {
	ID            RunID
	Status        RunStatus
	Messages      []Message
	Metadata      map[string]string
	ModelAttempts []ModelAttempt
}

// ModelAttempt records one provider invocation during a run. When routing and
// fallback are in use, multiple attempts may be recorded per run.
type ModelAttempt struct {
	Provider ProviderID
	Model    string
	Status   ModelAttemptStatus
	Error    *ModelError
}

// ModelAttemptStatus describes the outcome of a single provider attempt.
type ModelAttemptStatus string

const (
	ModelAttemptSuccess   ModelAttemptStatus = "success"
	ModelAttemptFallback  ModelAttemptStatus = "fallback"
	ModelAttemptFailed    ModelAttemptStatus = "failed"
	ModelAttemptCancelled ModelAttemptStatus = "cancelled"
)

// StructuredOutput returns the structured JSON payload of the final assistant
// message in the transcript. The value is empty when the final assistant
// message produced no structured output. When the run was driven by an agent
// with an output schema, the returned value has already passed local schema
// validation.
func (r RunResult) StructuredOutput() ModelStructuredOutput {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role == RoleAssistant {
			return r.Messages[i].StructuredOutput
		}
	}
	return ""
}

// DecodeStructuredOutput unmarshals the final assistant structured payload into
// the caller-provided value. It returns an error when the run produced no
// structured output or the payload cannot be decoded into v.
func (r RunResult) DecodeStructuredOutput(v any) error {
	output := r.StructuredOutput()
	if output == "" {
		return errors.New("lebro: run result has no structured output")
	}
	if err := json.Unmarshal(output.Raw(), v); err != nil {
		return fmt.Errorf("lebro: decode structured output: %w", err)
	}
	return nil
}
