package lebro

import "github.com/tesh254/lebro/internal/runtime"

func NewAgent(config AgentConfig) (*Agent, error) { return runtime.NewAgent(config) }

func NewAgentStep(agent Workflow) (*AgentStep, error) { return runtime.NewAgentStep(agent) }

// NewSubagent exposes a Workflow as a schema-backed delegation capability.
func NewSubagent(config SubagentConfig) (*Subagent, error) { return runtime.NewSubagent(config) }
