package lebro

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Stable identifiers make runtime records portable across model providers,
// storage adapters, and transport layers.
type (
	AgentID    string
	RunID      string
	StepID     string
	ThreadID   string
	ToolID     string
	WorkflowID string
)

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
			return errors.New("lebro: message structured output must be valid JSON")
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
	Metadata map[string]string
}

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
	ID       RunID
	Status   RunStatus
	Messages []Message
	Metadata map[string]string
}
