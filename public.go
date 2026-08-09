package lebro

import (
	"context"
	"encoding/json"
	"time"

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
	StreamingModel             = runtime.StreamingModel
	StreamDelta                = runtime.StreamDelta
	StreamReader               = runtime.StreamReader
	StreamReaderFunc           = runtime.StreamReaderFunc
	StreamRun                  = runtime.StreamRun
	ModelErrorKind             = runtime.ModelErrorKind
	ModelError                 = runtime.ModelError
	WorkflowDefinition         = runtime.WorkflowDefinition
	Workflow                   = runtime.Workflow
	StepDefinition             = runtime.StepDefinition
	Step                       = runtime.Step
	StepHandler                = runtime.StepHandler
	StepHandlerFunc            = runtime.StepHandlerFunc
	AgentStep                  = runtime.AgentStep
	ToolStep                   = runtime.ToolStep
	LinearWorkflowConfig       = runtime.LinearWorkflowConfig
	LinearWorkflow             = runtime.LinearWorkflow
	WorkflowRunInput           = runtime.WorkflowRunInput
	WorkflowRunResult          = runtime.WorkflowRunResult
	WorkflowError              = runtime.WorkflowError
	WorkflowErrorKind          = runtime.WorkflowErrorKind
	StepPanicError             = runtime.StepPanicError
	RetryPolicy                = runtime.RetryPolicy
	RetryablePredicate         = runtime.RetryablePredicate
	PageRequest                = runtime.PageRequest
	Page[T any]                = runtime.Page[T]
	ThreadRecord               = runtime.ThreadRecord
	MessageRecord              = runtime.MessageRecord
	WorkflowRunRecord          = runtime.WorkflowRunRecord
	WorkflowSnapshotRecord     = runtime.WorkflowSnapshotRecord
	WorkflowFailureData        = runtime.WorkflowFailureData
	WorkflowRunFilter          = runtime.WorkflowRunFilter
	ThreadRepository           = runtime.ThreadRepository
	MessageRepository          = runtime.MessageRepository
	WorkflowRunRepository      = runtime.WorkflowRunRepository
	WorkflowSnapshotRepository = runtime.WorkflowSnapshotRepository
	Repositories               = runtime.Repositories
	Store                      = runtime.Store
	ThreadStore                = runtime.ThreadStore
	WorkflowRunStore           = runtime.WorkflowRunStore
	MemoryStore                = runtime.MemoryStore
	SQLiteStore                = runtime.SQLiteStore
	PostgresStore              = runtime.PostgresStore
	PostgresStoreOptions       = runtime.PostgresStoreOptions
	RunEvent                   = runtime.RunEvent
	RunEventType               = runtime.RunEventType
	RunListener                = runtime.RunListener
	RunRecorder                = runtime.RunRecorder
	Clock                      = runtime.Clock
	IDSource                   = runtime.IDSource
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

	ValidationTargetToolInput        = runtime.ValidationTargetToolInput
	ValidationTargetToolOutput       = runtime.ValidationTargetToolOutput
	ValidationTargetStructuredOutput = runtime.ValidationTargetStructuredOutput
	ValidationTargetStepInput        = runtime.ValidationTargetStepInput
	ValidationTargetStepOutput       = runtime.ValidationTargetStepOutput
	JSONSchemaDraft202012            = runtime.JSONSchemaDraft202012

	FinishReasonStop        = runtime.FinishReasonStop
	FinishReasonLength      = runtime.FinishReasonLength
	FinishReasonToolCalls   = runtime.FinishReasonToolCalls
	FinishReasonContent     = runtime.FinishReasonContent
	FinishReasonCancelled   = runtime.FinishReasonCancelled
	FinishReasonUnspecified = runtime.FinishReasonUnspecified

	AgentErrorUnknownTool             = runtime.AgentErrorUnknownTool
	AgentErrorInvalidToolArguments    = runtime.AgentErrorInvalidToolArguments
	AgentErrorInvalidToolOutput       = runtime.AgentErrorInvalidToolOutput
	AgentErrorToolFailure             = runtime.AgentErrorToolFailure
	AgentErrorProviderFailure         = runtime.AgentErrorProviderFailure
	AgentErrorStepLimitExhausted      = runtime.AgentErrorStepLimitExhausted
	AgentErrorCancelled               = runtime.AgentErrorCancelled
	AgentErrorInvalidStructuredOutput = runtime.AgentErrorInvalidStructuredOutput

	WorkflowErrorInvalidStepInput  = runtime.WorkflowErrorInvalidStepInput
	WorkflowErrorInvalidStepOutput = runtime.WorkflowErrorInvalidStepOutput
	WorkflowErrorStepFailed        = runtime.WorkflowErrorStepFailed
	WorkflowErrorStepPanicked      = runtime.WorkflowErrorStepPanicked
	WorkflowErrorCancelled         = runtime.WorkflowErrorCancelled

	RunEventStarted             = runtime.RunEventStarted
	RunEventModelStarted        = runtime.RunEventModelStarted
	RunEventModelFinished       = runtime.RunEventModelFinished
	RunEventToolRequested       = runtime.RunEventToolRequested
	RunEventToolStarted         = runtime.RunEventToolStarted
	RunEventToolFinished        = runtime.RunEventToolFinished
	RunEventDelta               = runtime.RunEventDelta
	RunEventStepStarted         = runtime.RunEventStepStarted
	RunEventStepFinished        = runtime.RunEventStepFinished
	RunEventStepAttemptStarted  = runtime.RunEventStepAttemptStarted
	RunEventStepAttemptFinished = runtime.RunEventStepAttemptFinished
	RunEventSucceeded           = runtime.RunEventSucceeded
	RunEventFailed              = runtime.RunEventFailed
	RunEventCancelled           = runtime.RunEventCancelled

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

	ErrMessageStructuredOutputInvalidJSON = runtime.ErrMessageStructuredOutputInvalidJSON

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

	ErrAgentUnknownTool             = runtime.ErrAgentUnknownTool
	ErrAgentInvalidToolArguments    = runtime.ErrAgentInvalidToolArguments
	ErrAgentInvalidToolOutput       = runtime.ErrAgentInvalidToolOutput
	ErrAgentToolFailure             = runtime.ErrAgentToolFailure
	ErrAgentProviderFailure         = runtime.ErrAgentProviderFailure
	ErrAgentStepLimitExhausted      = runtime.ErrAgentStepLimitExhausted
	ErrAgentCancelled               = runtime.ErrAgentCancelled
	ErrAgentInvalidStructuredOutput = runtime.ErrAgentInvalidStructuredOutput

	ErrWorkflowInvalidStepInput  = runtime.ErrWorkflowInvalidStepInput
	ErrWorkflowInvalidStepOutput = runtime.ErrWorkflowInvalidStepOutput
	ErrWorkflowStepFailure       = runtime.ErrWorkflowStepFailure
	ErrWorkflowStepPanicked      = runtime.ErrWorkflowStepPanicked
	ErrWorkflowCancelled         = runtime.ErrWorkflowCancelled
)

// DefaultRetryable reports whether a handler error is eligible for retry
// under the default policy. It rejects context cancellation and deadline
// errors and accepts all other handler errors. Use it as a building block for
// custom RetryablePredicate implementations. Because it is a function, it
// cannot be reassigned to change runtime behavior; pass it (or a function
// that delegates to it) to RetryPolicy.Retryable instead.
func DefaultRetryable(err error) bool { return runtime.DefaultRetryable(err) }

func NewToolRegistry(compiler SchemaCompiler) (*ToolRegistry, error) {
	return runtime.NewToolRegistry(compiler)
}

func NewToolSchemaValidator(compiler SchemaCompiler, definition ToolDefinition) (*ToolSchemaValidator, error) {
	return runtime.NewToolSchemaValidator(compiler, definition)
}

func NewAgentStep(agent Workflow) (*AgentStep, error) { return runtime.NewAgentStep(agent) }

func NewToolStep(tool *RegisteredTool) (*ToolStep, error) { return runtime.NewToolStep(tool) }

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

// NewSQLiteStore opens (or creates) a file-backed SQLite storage adapter at
// the given DSN and returns the store. Call Migrate once to install the
// schema.
func NewSQLiteStore(dsn string) (*SQLiteStore, error) { return runtime.NewSQLiteStore(dsn) }

// NewPostgresStore opens a PostgreSQL connection pool at the given DSN and
// returns the store. Call Migrate once to install the schema.
func NewPostgresStore(dsn string, opts PostgresStoreOptions) (*PostgresStore, error) {
	return runtime.NewPostgresStore(dsn, opts)
}

func NewAgent(config AgentConfig) (*Agent, error) {
	return runtime.NewAgent(config)
}

func NewLinearWorkflow(config LinearWorkflowConfig) (*LinearWorkflow, error) {
	return runtime.NewLinearWorkflow(config)
}

func NewRunRecorder() *RunRecorder { return runtime.NewRunRecorder() }

func NewFixedClock(t time.Time) Clock { return runtime.NewFixedClock(t) }

func NewFixedIDSource(runIDs []RunID, stepIDs []StepID) IDSource {
	return runtime.NewFixedIDSource(runIDs, stepIDs)
}

// AsStreamingModel returns model as a StreamingModel when the concrete value
// implements Stream. It returns nil when the adapter only supports Generate,
// letting callers fall back to a non-streaming run without type assertions.
func AsStreamingModel(model Model) StreamingModel {
	return runtime.AsStreamingModel(model)
}
