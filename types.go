package lebro

import (
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
// Tool-call fields are intentionally included in the foundation contract so
// model adapters and agent execution can evolve without changing history data.
type Message struct {
	Role       Role
	Content    string
	Name       string
	ToolCallID string
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
