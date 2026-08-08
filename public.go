package lebro

import (
	"context"
	"encoding/json"

	"github.com/tesh254/lebro/internal/runtime"
)

type (
	AgentID                    = runtime.AgentID
	RunID                      = runtime.RunID
	StepID                     = runtime.StepID
	ThreadID                   = runtime.ThreadID
	ToolID                     = runtime.ToolID
	WorkflowID                 = runtime.WorkflowID
	Role                       = runtime.Role
	Message                    = runtime.Message
	AgentDefinition            = runtime.AgentDefinition
	Agent                      = runtime.Agent
	AgentConfig                = runtime.AgentConfig
	AgentError                 = runtime.AgentError
	AgentErrorKind             = runtime.AgentErrorKind
	RunInput                   = runtime.RunInput
	RunStatus                  = runtime.RunStatus
	RunResult                  = runtime.RunResult
	ToolDefinition             = runtime.ToolDefinition
	Tool                       = runtime.Tool
	ToolExecutionRequest       = runtime.ToolExecutionRequest
	ToolExecutionState         = runtime.ToolExecutionState
	ToolExecutionResult        = runtime.ToolExecutionResult
	ToolExecutionError         = runtime.ToolExecutionError
	ToolPanicError             = runtime.ToolPanicError
	RegisteredTool             = runtime.RegisteredTool
	ToolRegistry               = runtime.ToolRegistry
	SchemaCompiler             = runtime.SchemaCompiler
	CompiledSchema             = runtime.CompiledSchema
	ValidationTarget           = runtime.ValidationTarget
	ValidationIssue            = runtime.ValidationIssue
	ValidationError            = runtime.ValidationError
	SchemaError                = runtime.SchemaError
	ToolSchemaValidator        = runtime.ToolSchemaValidator
	ModelRequest               = runtime.ModelRequest
	ModelOutputSchema          = runtime.ModelOutputSchema
	ModelToolCall              = runtime.ModelToolCall
	ModelToolCalls             = runtime.ModelToolCalls
	ModelStructuredOutput      = runtime.ModelStructuredOutput
	ModelUsage                 = runtime.ModelUsage
	FinishReason               = runtime.FinishReason
	ModelResponse              = runtime.ModelResponse
	Model                      = runtime.Model
	ModelErrorKind             = runtime.ModelErrorKind
	ModelError                 = runtime.ModelError
	WorkflowDefinition         = runtime.WorkflowDefinition
	Workflow                   = runtime.Workflow
	PageRequest                = runtime.PageRequest
	Page[T any]                = runtime.Page[T]
	ThreadRecord               = runtime.ThreadRecord
	MessageRecord              = runtime.MessageRecord
	WorkflowRunRecord          = runtime.WorkflowRunRecord
	WorkflowSnapshotRecord     = runtime.WorkflowSnapshotRecord
	ThreadRepository           = runtime.ThreadRepository
	MessageRepository          = runtime.MessageRepository
	WorkflowRunRepository      = runtime.WorkflowRunRepository
	WorkflowSnapshotRepository = runtime.WorkflowSnapshotRepository
	Repositories               = runtime.Repositories
	Store                      = runtime.Store
	ThreadStore                = runtime.ThreadStore
	WorkflowRunStore           = runtime.WorkflowRunStore
	MemoryStore                = runtime.MemoryStore
)

const (
	RoleSystem    = runtime.RoleSystem
	RoleUser      = runtime.RoleUser
	RoleAssistant = runtime.RoleAssistant
	RoleTool      = runtime.RoleTool

	RunStatusPending   = runtime.RunStatusPending
	RunStatusRunning   = runtime.RunStatusRunning
	RunStatusSucceeded = runtime.RunStatusSucceeded
	RunStatusFailed    = runtime.RunStatusFailed
	RunStatusCancelled = runtime.RunStatusCancelled
	RunStatusSuspended = runtime.RunStatusSuspended

	ToolExecutionSucceeded     = runtime.ToolExecutionSucceeded
	ToolExecutionInvalidInput  = runtime.ToolExecutionInvalidInput
	ToolExecutionInvalidOutput = runtime.ToolExecutionInvalidOutput
	ToolExecutionHandlerError  = runtime.ToolExecutionHandlerError
	ToolExecutionPanicked      = runtime.ToolExecutionPanicked
	ToolExecutionCancelled     = runtime.ToolExecutionCancelled
	ToolExecutionNotFound      = runtime.ToolExecutionNotFound

	ValidationTargetToolInput  = runtime.ValidationTargetToolInput
	ValidationTargetToolOutput = runtime.ValidationTargetToolOutput
	JSONSchemaDraft202012      = runtime.JSONSchemaDraft202012

	FinishReasonStop        = runtime.FinishReasonStop
	FinishReasonLength      = runtime.FinishReasonLength
	FinishReasonToolCalls   = runtime.FinishReasonToolCalls
	FinishReasonContent     = runtime.FinishReasonContent
	FinishReasonCancelled   = runtime.FinishReasonCancelled
	FinishReasonUnspecified = runtime.FinishReasonUnspecified

	AgentErrorUnknownTool          = runtime.AgentErrorUnknownTool
	AgentErrorInvalidToolArguments = runtime.AgentErrorInvalidToolArguments
	AgentErrorInvalidToolOutput    = runtime.AgentErrorInvalidToolOutput
	AgentErrorToolFailure          = runtime.AgentErrorToolFailure
	AgentErrorProviderFailure      = runtime.AgentErrorProviderFailure
	AgentErrorStepLimitExhausted   = runtime.AgentErrorStepLimitExhausted
	AgentErrorCancelled            = runtime.AgentErrorCancelled

	DefaultAgentMaxSteps = runtime.DefaultAgentMaxSteps

	ModelErrorInvalidRequest    = runtime.ModelErrorInvalidRequest
	ModelErrorAuthentication    = runtime.ModelErrorAuthentication
	ModelErrorPermissionDenied  = runtime.ModelErrorPermissionDenied
	ModelErrorNotFound          = runtime.ModelErrorNotFound
	ModelErrorRateLimited       = runtime.ModelErrorRateLimited
	ModelErrorTimeout           = runtime.ModelErrorTimeout
	ModelErrorUnavailable       = runtime.ModelErrorUnavailable
	ModelErrorTransport         = runtime.ModelErrorTransport
	ModelErrorMalformedResponse = runtime.ModelErrorMalformedResponse
	ModelErrorUnknown           = runtime.ModelErrorUnknown
)

var (
	ErrToolNotFound = runtime.ErrToolNotFound
	ErrNotFound     = runtime.ErrNotFound
	ErrConflict     = runtime.ErrConflict
	ErrInvalidPage  = runtime.ErrInvalidPage

	ErrModelInvalidRequest    = runtime.ErrModelInvalidRequest
	ErrModelAuthentication    = runtime.ErrModelAuthentication
	ErrModelPermissionDenied  = runtime.ErrModelPermissionDenied
	ErrModelNotFound          = runtime.ErrModelNotFound
	ErrModelRateLimited       = runtime.ErrModelRateLimited
	ErrModelTimeout           = runtime.ErrModelTimeout
	ErrModelUnavailable       = runtime.ErrModelUnavailable
	ErrModelTransport         = runtime.ErrModelTransport
	ErrModelMalformedResponse = runtime.ErrModelMalformedResponse
	ErrModelUnknown           = runtime.ErrModelUnknown

	ErrAgentUnknownTool          = runtime.ErrAgentUnknownTool
	ErrAgentInvalidToolArguments = runtime.ErrAgentInvalidToolArguments
	ErrAgentInvalidToolOutput    = runtime.ErrAgentInvalidToolOutput
	ErrAgentToolFailure          = runtime.ErrAgentToolFailure
	ErrAgentProviderFailure      = runtime.ErrAgentProviderFailure
	ErrAgentStepLimitExhausted   = runtime.ErrAgentStepLimitExhausted
	ErrAgentCancelled            = runtime.ErrAgentCancelled
)

func NewToolRegistry(compiler SchemaCompiler) (*ToolRegistry, error) {
	return runtime.NewToolRegistry(compiler)
}

func NewToolSchemaValidator(compiler SchemaCompiler, definition ToolDefinition) (*ToolSchemaValidator, error) {
	return runtime.NewToolSchemaValidator(compiler, definition)
}

func ToolMetadataFromContext(ctx context.Context) map[string]string {
	return runtime.ToolMetadataFromContext(ctx)
}

func NewModelToolCalls(calls ...ModelToolCall) (ModelToolCalls, error) {
	return runtime.NewModelToolCalls(calls...)
}

func NewModelStructuredOutput(value json.RawMessage) ModelStructuredOutput {
	return runtime.NewModelStructuredOutput(value)
}

func NewMemoryStore() *MemoryStore { return runtime.NewMemoryStore() }

func NewAgent(config AgentConfig) (*Agent, error) {
	return runtime.NewAgent(config)
}
