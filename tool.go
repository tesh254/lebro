package lebro

import (
	"context"
	"encoding/json"
)

// ToolDefinition describes a callable capability for a model. Schemas are raw
// JSON so the core package does not depend on a particular JSON Schema library.
type ToolDefinition struct {
	ID           ToolID
	Description  string
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
}

// Tool is implemented by application capabilities that can later be invoked
// from an agent loop or a workflow step.
type Tool interface {
	Definition() ToolDefinition
	Execute(context.Context, json.RawMessage) (json.RawMessage, error)
}
