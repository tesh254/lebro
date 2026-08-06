package runtime

import "context"

// WorkflowDefinition describes a named workflow before execution semantics are
// added by the workflow implementation tasks.
type WorkflowDefinition struct {
	ID          WorkflowID
	Name        string
	Description string
}

// Workflow is the common contract implemented by executable workflows.
type Workflow interface {
	Definition() WorkflowDefinition
	Run(context.Context, RunInput) (RunResult, error)
}
