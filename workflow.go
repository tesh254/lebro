package lebro

import "github.com/tesh254/lebro/internal/runtime"

func DefaultRetryable(err error) bool { return runtime.DefaultRetryable(err) }
func NewLinearWorkflow(config LinearWorkflowConfig) (*LinearWorkflow, error) {
	return runtime.NewLinearWorkflow(config)
}

func NewApprovalGate(requestID, guardID StepID, inner StepHandler, req ApprovalRequirement, compiler SchemaCompiler, store Store, clock Clock) (ApprovalGate, error) {
	return runtime.NewApprovalGate(requestID, guardID, inner, req, compiler, store, clock)
}
