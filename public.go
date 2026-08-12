package lebro

import (
	"context"
	"encoding/json"
	"time"

	"github.com/tesh254/lebro/internal/runtime"
)

type (
	AgentID                     = runtime.AgentID
	RunID                       = runtime.RunID
	StepID                      = runtime.StepID
	ThreadID                    = runtime.ThreadID
	ToolID                      = runtime.ToolID
	WorkflowID                  = runtime.WorkflowID
	ScheduleID                  = runtime.ScheduleID
	ScheduleExecutionID         = runtime.ScheduleExecutionID
	Role                        = runtime.Role
	Message                     = runtime.Message
	AgentDefinition             = runtime.AgentDefinition
	Agent                       = runtime.Agent
	AgentConfig                 = runtime.AgentConfig
	AgentError                  = runtime.AgentError
	AgentErrorKind              = runtime.AgentErrorKind
	RunInput                    = runtime.RunInput
	RunStatus                   = runtime.RunStatus
	RunResult                   = runtime.RunResult
	ModelAttempt                = runtime.ModelAttempt
	ModelAttemptStatus          = runtime.ModelAttemptStatus
	ProviderID                  = runtime.ProviderID
	ProviderCapabilities        = runtime.ProviderCapabilities
	ProviderEntry               = runtime.ProviderEntry
	ProviderRegistry            = runtime.ProviderRegistry
	RoutingPolicy               = runtime.RoutingPolicy
	ModelRouter                 = runtime.ModelRouter
	ModelRouterConfig           = runtime.ModelRouterConfig
	FallbackPolicy              = runtime.FallbackPolicy
	ToolDefinition              = runtime.ToolDefinition
	Tool                        = runtime.Tool
	ToolExecutionRequest        = runtime.ToolExecutionRequest
	ToolExecutionState          = runtime.ToolExecutionState
	ToolExecutionResult         = runtime.ToolExecutionResult
	ToolExecutionError          = runtime.ToolExecutionError
	ToolPanicError              = runtime.ToolPanicError
	RegisteredTool              = runtime.RegisteredTool
	ToolRegistry                = runtime.ToolRegistry
	Subagent                    = runtime.Subagent
	SubagentConfig              = runtime.SubagentConfig
	SubagentError               = runtime.SubagentError
	SubagentErrorKind           = runtime.SubagentErrorKind
	SchemaCompiler              = runtime.SchemaCompiler
	CompiledSchema              = runtime.CompiledSchema
	ValidationTarget            = runtime.ValidationTarget
	ValidationIssue             = runtime.ValidationIssue
	ValidationError             = runtime.ValidationError
	SchemaError                 = runtime.SchemaError
	ToolSchemaValidator         = runtime.ToolSchemaValidator
	ModelRequest                = runtime.ModelRequest
	ModelOutputSchema           = runtime.ModelOutputSchema
	ModelToolCall               = runtime.ModelToolCall
	ModelToolCalls              = runtime.ModelToolCalls
	ModelStructuredOutput       = runtime.ModelStructuredOutput
	ModelUsage                  = runtime.ModelUsage
	FinishReason                = runtime.FinishReason
	ModelResponse               = runtime.ModelResponse
	Model                       = runtime.Model
	StreamingModel              = runtime.StreamingModel
	StreamDelta                 = runtime.StreamDelta
	StreamReader                = runtime.StreamReader
	StreamReaderFunc            = runtime.StreamReaderFunc
	StreamRun                   = runtime.StreamRun
	ModelErrorKind              = runtime.ModelErrorKind
	ModelError                  = runtime.ModelError
	WorkflowDefinition          = runtime.WorkflowDefinition
	Workflow                    = runtime.Workflow
	StepDefinition              = runtime.StepDefinition
	Step                        = runtime.Step
	StepHandler                 = runtime.StepHandler
	StepHandlerFunc             = runtime.StepHandlerFunc
	Branch                      = runtime.Branch
	BranchPredicate             = runtime.BranchPredicate
	FanOut                      = runtime.FanOut
	FanOutBranch                = runtime.FanOutBranch
	FanOutFailurePolicy         = runtime.FanOutFailurePolicy
	FanOutInputMapper           = runtime.FanOutInputMapper
	FanOutBranchResult          = runtime.FanOutBranchResult
	FanOutJoinResult            = runtime.FanOutJoinResult
	AgentStep                   = runtime.AgentStep
	ToolStep                    = runtime.ToolStep
	LinearWorkflowConfig        = runtime.LinearWorkflowConfig
	LinearWorkflow              = runtime.LinearWorkflow
	WorkflowRunInput            = runtime.WorkflowRunInput
	WorkflowRunResult           = runtime.WorkflowRunResult
	WorkflowError               = runtime.WorkflowError
	WorkflowErrorKind           = runtime.WorkflowErrorKind
	StepPanicError              = runtime.StepPanicError
	SuspendSignal               = runtime.SuspendSignal
	SuspendError                = runtime.SuspendError
	SuspendResult               = runtime.SuspendResult
	WorkflowResumeInput         = runtime.WorkflowResumeInput
	RetryPolicy                 = runtime.RetryPolicy
	RetryablePredicate          = runtime.RetryablePredicate
	PageRequest                 = runtime.PageRequest
	Page[T any]                 = runtime.Page[T]
	ThreadRecord                = runtime.ThreadRecord
	MessageRecord               = runtime.MessageRecord
	WorkflowRunRecord           = runtime.WorkflowRunRecord
	WorkflowSnapshotRecord      = runtime.WorkflowSnapshotRecord
	WorkflowFailureData         = runtime.WorkflowFailureData
	WorkflowRunFilter           = runtime.WorkflowRunFilter
	ScheduleRecord              = runtime.ScheduleRecord
	ScheduleExecutionRecord     = runtime.ScheduleExecutionRecord
	ScheduleExecStatus          = runtime.ScheduleExecStatus
	ScheduleFilter              = runtime.ScheduleFilter
	ConcurrencyPolicy           = runtime.ConcurrencyPolicy
	CronSchedule                = runtime.CronSchedule
	Scheduler                   = runtime.Scheduler
	SchedulerConfig             = runtime.SchedulerConfig
	TickResult                  = runtime.TickResult
	WorkflowResolver            = runtime.WorkflowResolver
	WorkflowMap                 = runtime.WorkflowMap
	ThreadRepository            = runtime.ThreadRepository
	MessageRepository           = runtime.MessageRepository
	WorkflowRunRepository       = runtime.WorkflowRunRepository
	WorkflowSnapshotRepository  = runtime.WorkflowSnapshotRepository
	ScheduleRepository          = runtime.ScheduleRepository
	ScheduleExecutionRepository = runtime.ScheduleExecutionRepository
	Repositories                = runtime.Repositories
	Store                       = runtime.Store
	ThreadStore                 = runtime.ThreadStore
	WorkflowRunStore            = runtime.WorkflowRunStore
	MemoryStore                 = runtime.MemoryStore
	SQLiteStore                 = runtime.SQLiteStore
	PostgresStore               = runtime.PostgresStore
	PostgresStoreOptions        = runtime.PostgresStoreOptions
	MemoryVectorStore           = runtime.MemoryVectorStore
	SQLiteVectorStore           = runtime.SQLiteVectorStore
	PostgresVectorStore         = runtime.PostgresVectorStore
	PostgresVectorStoreOptions  = runtime.PostgresVectorStoreOptions
	VectorStore                 = runtime.VectorStore
	EmbeddingRecord             = runtime.EmbeddingRecord
	VectorMetadataFilter        = runtime.VectorMetadataFilter
	SimilarityQuery             = runtime.SimilarityQuery
	SimilarityResult            = runtime.SimilarityResult
	Document                    = runtime.Document
	Chunk                       = runtime.Chunk
	Chunker                     = runtime.Chunker
	CharacterChunker            = runtime.CharacterChunker
	CharacterChunkerConfig      = runtime.CharacterChunkerConfig
	EmbeddingModel              = runtime.EmbeddingModel
	Indexer                     = runtime.Indexer
	IndexerConfig               = runtime.IndexerConfig
	IndexResult                 = runtime.IndexResult
	Retriever                   = runtime.Retriever
	RetrievalQuery              = runtime.RetrievalQuery
	RetrievedChunk              = runtime.RetrievedChunk
	VectorRetriever             = runtime.VectorRetriever
	VectorRetrieverConfig       = runtime.VectorRetrieverConfig
	RetrievalTool               = runtime.RetrievalTool
	RetrievalToolConfig         = runtime.RetrievalToolConfig
	RAGError                    = runtime.RAGError
	RAGErrorKind                = runtime.RAGErrorKind
	RunEvent                    = runtime.RunEvent
	RunEventType                = runtime.RunEventType
	RunListener                 = runtime.RunListener
	RunRecorder                 = runtime.RunRecorder
	Clock                       = runtime.Clock
	IDSource                    = runtime.IDSource
	Identity                    = runtime.Identity
	Capability                  = runtime.Capability
	Action                      = runtime.Action
	ResourceKind                = runtime.ResourceKind
	Resource                    = runtime.Resource
	Decision                    = runtime.Decision
	Policy                      = runtime.Policy
	AllowAllPolicy              = runtime.AllowAllPolicy
	PolicyDenial                = runtime.PolicyDenial
	PolicyStore                 = runtime.PolicyStore
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

	ConcurrencyAllow = runtime.ConcurrencyAllow
	ConcurrencySkip  = runtime.ConcurrencySkip

	ScheduleExecSucceeded = runtime.ScheduleExecSucceeded
	ScheduleExecFailed    = runtime.ScheduleExecFailed
	ScheduleExecSkipped   = runtime.ScheduleExecSkipped
	ScheduleExecMissed    = runtime.ScheduleExecMissed

	ToolExecutionSucceeded     = runtime.ToolExecutionSucceeded
	ToolExecutionInvalidInput  = runtime.ToolExecutionInvalidInput
	ToolExecutionInvalidOutput = runtime.ToolExecutionInvalidOutput
	ToolExecutionHandlerError  = runtime.ToolExecutionHandlerError
	ToolExecutionPanicked      = runtime.ToolExecutionPanicked
	ToolExecutionCancelled     = runtime.ToolExecutionCancelled
	ToolExecutionNotFound      = runtime.ToolExecutionNotFound
	ToolExecutionUnauthorized  = runtime.ToolExecutionUnauthorized

	ValidationTargetToolInput        = runtime.ValidationTargetToolInput
	ValidationTargetToolOutput       = runtime.ValidationTargetToolOutput
	ValidationTargetStructuredOutput = runtime.ValidationTargetStructuredOutput
	ValidationTargetStepInput        = runtime.ValidationTargetStepInput
	ValidationTargetStepOutput       = runtime.ValidationTargetStepOutput
	ValidationTargetSuspendContract  = runtime.ValidationTargetSuspendContract
	ValidationTargetResumeInput      = runtime.ValidationTargetResumeInput
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
	AgentErrorUnauthorized            = runtime.AgentErrorUnauthorized

	ActionAgentRun     = runtime.ActionAgentRun
	ActionToolCall     = runtime.ActionToolCall
	ActionWorkflowRun  = runtime.ActionWorkflowRun
	ActionStorageRead  = runtime.ActionStorageRead
	ActionStorageWrite = runtime.ActionStorageWrite

	ResourceKindAgent            = runtime.ResourceKindAgent
	ResourceKindTool             = runtime.ResourceKindTool
	ResourceKindWorkflow         = runtime.ResourceKindWorkflow
	ResourceKindThread           = runtime.ResourceKindThread
	ResourceKindMessage          = runtime.ResourceKindMessage
	ResourceKindWorkflowRun      = runtime.ResourceKindWorkflowRun
	ResourceKindWorkflowSnapshot = runtime.ResourceKindWorkflowSnapshot
	ResourceKindSchedule         = runtime.ResourceKindSchedule

	SubagentErrorInvalidInput = runtime.SubagentErrorInvalidInput
	SubagentErrorRunFailed    = runtime.SubagentErrorRunFailed
	SubagentErrorCancelled    = runtime.SubagentErrorCancelled

	WorkflowErrorInvalidStepInput        = runtime.WorkflowErrorInvalidStepInput
	WorkflowErrorInvalidStepOutput       = runtime.WorkflowErrorInvalidStepOutput
	WorkflowErrorStepFailed              = runtime.WorkflowErrorStepFailed
	WorkflowErrorStepPanicked            = runtime.WorkflowErrorStepPanicked
	WorkflowErrorCancelled               = runtime.WorkflowErrorCancelled
	WorkflowErrorNoBranchMatched         = runtime.WorkflowErrorNoBranchMatched
	WorkflowErrorBranchConditionFailed   = runtime.WorkflowErrorBranchConditionFailed
	WorkflowErrorInvalidBranchInput      = runtime.WorkflowErrorInvalidBranchInput
	WorkflowErrorFanOutBranchFailed      = runtime.WorkflowErrorFanOutBranchFailed
	WorkflowErrorFanOutInputMapperFailed = runtime.WorkflowErrorFanOutInputMapperFailed
	WorkflowErrorInvalidFanOutInput      = runtime.WorkflowErrorInvalidFanOutInput
	WorkflowErrorUnauthorized            = runtime.WorkflowErrorUnauthorized

	RunEventStarted              = runtime.RunEventStarted
	RunEventModelStarted         = runtime.RunEventModelStarted
	RunEventModelFinished        = runtime.RunEventModelFinished
	RunEventToolRequested        = runtime.RunEventToolRequested
	RunEventToolStarted          = runtime.RunEventToolStarted
	RunEventToolFinished         = runtime.RunEventToolFinished
	RunEventDelta                = runtime.RunEventDelta
	RunEventStepStarted          = runtime.RunEventStepStarted
	RunEventStepFinished         = runtime.RunEventStepFinished
	RunEventStepAttemptStarted   = runtime.RunEventStepAttemptStarted
	RunEventStepAttemptFinished  = runtime.RunEventStepAttemptFinished
	RunEventSucceeded            = runtime.RunEventSucceeded
	RunEventFailed               = runtime.RunEventFailed
	RunEventCancelled            = runtime.RunEventCancelled
	RunEventSuspended            = runtime.RunEventSuspended
	RunEventResumed              = runtime.RunEventResumed
	RunEventBranchSelected       = runtime.RunEventBranchSelected
	RunEventModelAttemptStarted  = runtime.RunEventModelAttemptStarted
	RunEventModelAttemptFinished = runtime.RunEventModelAttemptFinished

	FanOutFailFast   = runtime.FanOutFailFast
	FanOutCollectAll = runtime.FanOutCollectAll

	ModelAttemptSuccess  = runtime.ModelAttemptSuccess
	ModelAttemptFallback = runtime.ModelAttemptFallback
	ModelAttemptFailed   = runtime.ModelAttemptFailed

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

	RAGErrorInvalidDocument = runtime.RAGErrorInvalidDocument
	RAGErrorChunking        = runtime.RAGErrorChunking
	RAGErrorEmbedding       = runtime.RAGErrorEmbedding
	RAGErrorIndexing        = runtime.RAGErrorIndexing
	RAGErrorRetrieval       = runtime.RAGErrorRetrieval

	ChunkMetadataDocumentID = runtime.ChunkMetadataDocumentID
	ChunkMetadataSource     = runtime.ChunkMetadataSource
	ChunkMetadataChunkIndex = runtime.ChunkMetadataChunkIndex

	DefaultChunkSize          = runtime.DefaultChunkSize
	DefaultEmbeddingBatchSize = runtime.DefaultEmbeddingBatchSize
	DefaultRetrievalTopK      = runtime.DefaultRetrievalTopK
)

var (
	ErrToolNotFound          = runtime.ErrToolNotFound
	ErrNotFound              = runtime.ErrNotFound
	ErrConflict              = runtime.ErrConflict
	ErrInvalidPage           = runtime.ErrInvalidPage
	ErrSchedulerRunning      = runtime.ErrSchedulerRunning
	ErrProviderNotFound      = runtime.ErrProviderNotFound
	ErrProviderAlreadyExists = runtime.ErrProviderAlreadyExists

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
	ErrAgentUnauthorized            = runtime.ErrAgentUnauthorized

	ErrPolicyDenied = runtime.ErrPolicyDenied

	ErrSubagentInvalidInput = runtime.ErrSubagentInvalidInput
	ErrSubagentRunFailed    = runtime.ErrSubagentRunFailed
	ErrSubagentCancelled    = runtime.ErrSubagentCancelled

	ErrWorkflowInvalidStepInput  = runtime.ErrWorkflowInvalidStepInput
	ErrWorkflowInvalidStepOutput = runtime.ErrWorkflowInvalidStepOutput
	ErrWorkflowStepFailure       = runtime.ErrWorkflowStepFailure
	ErrWorkflowStepPanicked      = runtime.ErrWorkflowStepPanicked
	ErrWorkflowCancelled         = runtime.ErrWorkflowCancelled

	ErrWorkflowSuspend             = runtime.ErrWorkflowSuspend
	ErrNotSuspended                = runtime.ErrNotSuspended
	ErrInvalidResumeInput          = runtime.ErrInvalidResumeInput
	ErrWorkflowResumeRequiresStore = runtime.ErrWorkflowResumeRequiresStore

	ErrWorkflowNoBranchMatched         = runtime.ErrWorkflowNoBranchMatched
	ErrWorkflowBranchConditionFailed   = runtime.ErrWorkflowBranchConditionFailed
	ErrWorkflowInvalidBranchInput      = runtime.ErrWorkflowInvalidBranchInput
	ErrWorkflowFanOutBranchFailed      = runtime.ErrWorkflowFanOutBranchFailed
	ErrWorkflowFanOutInputMapperFailed = runtime.ErrWorkflowFanOutInputMapperFailed
	ErrWorkflowInvalidFanOutInput      = runtime.ErrWorkflowInvalidFanOutInput
	ErrWorkflowUnauthorized            = runtime.ErrWorkflowUnauthorized

	ErrVectorNotFound         = runtime.ErrVectorNotFound
	ErrVectorAlreadyExists    = runtime.ErrVectorAlreadyExists
	ErrVectorInvalidDimension = runtime.ErrVectorInvalidDimension
	ErrVectorInvalidInput     = runtime.ErrVectorInvalidInput

	ErrRAGInvalidDocument = runtime.ErrRAGInvalidDocument
	ErrRAGChunking        = runtime.ErrRAGChunking
	ErrRAGEmbedding       = runtime.ErrRAGEmbedding
	ErrRAGIndexing        = runtime.ErrRAGIndexing
	ErrRAGRetrieval       = runtime.ErrRAGRetrieval
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

// NewSubagent exposes a Workflow as a schema-backed delegation capability that
// a supervising agent can select and invoke by stable tool ID. Register the
// result in a ToolRegistry and list its ID in the supervisor's definition.
// Delegated runs are bounded independently of the parent and are correlated to
// it through the run event stream; thread context is shared only when the
// configuration opts in.
func NewSubagent(config SubagentConfig) (*Subagent, error) {
	return runtime.NewSubagent(config)
}

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

// WithIdentity returns a context carrying identity so downstream agent, tool,
// workflow, and storage operations authorize against the same caller. Nested
// runs (subagents, workflow steps) inherit it automatically because they share
// the run context.
func WithIdentity(ctx context.Context, identity Identity) context.Context {
	return runtime.WithIdentity(ctx, identity)
}

// IdentityFromContext returns a caller-owned copy of the identity previously
// stored with WithIdentity and whether one was present.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	return runtime.IdentityFromContext(ctx)
}

// Allow returns an allowing policy Decision.
func Allow() Decision { return runtime.Allow() }

// Deny returns a denying policy Decision carrying an optional reason.
func Deny(reason string) Decision { return runtime.Deny(reason) }

// NewPolicyStore returns a Store that authorizes every repository operation
// against policy before delegating to store. A nil policy leaves the store's
// behavior unchanged.
func NewPolicyStore(store Store, policy Policy) (*PolicyStore, error) {
	return runtime.NewPolicyStore(store, policy)
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

// NewMemoryVectorStore creates an empty in-memory vector store for tests and
// local development.
func NewMemoryVectorStore() *MemoryVectorStore { return runtime.NewMemoryVectorStore() }

// NewSQLiteVectorStore opens (or creates) a file-backed SQLite vector store at
// the given DSN. Call Migrate once to install the vector schema.
func NewSQLiteVectorStore(dsn string) (*SQLiteVectorStore, error) {
	return runtime.NewSQLiteVectorStore(dsn)
}

// NewPostgresVectorStore opens a PostgreSQL connection pool at the given DSN
// and returns a vector store. Call Migrate once to install the vector schema.
// Requires the pgvector extension on the target database.
func NewPostgresVectorStore(dsn string, opts PostgresVectorStoreOptions) (*PostgresVectorStore, error) {
	return runtime.NewPostgresVectorStore(dsn, opts)
}

// NewCharacterChunker returns a fixed-width, optionally overlapping rune-window
// chunker. Size and Overlap are measured in runes, so a multi-byte character is
// never split across chunks; Overlap must be less than Size.
func NewCharacterChunker(config CharacterChunkerConfig) (*CharacterChunker, error) {
	return runtime.NewCharacterChunker(config)
}

// NewIndexer builds the ingestion pipeline that chunks a document, embeds its
// chunks, and upserts them into a vector index. Call EnsureIndex once before
// the first Ingest. Ingestion is idempotent for an unchanged document because
// chunk IDs derive from the document ID and ordinal position.
func NewIndexer(config IndexerConfig) (*Indexer, error) {
	return runtime.NewIndexer(config)
}

// NewVectorRetriever returns a Retriever that embeds a natural-language query
// and searches a vector index. Its configured metadata filter is merged into
// every query and takes precedence on key collisions, so a configured scope
// cannot be widened by a caller.
func NewVectorRetriever(config VectorRetrieverConfig) (*VectorRetriever, error) {
	return runtime.NewVectorRetriever(config)
}

// NewRetrievalTool exposes a Retriever as an ordinary schema-backed Tool that a
// model can select inside the existing bounded tool loop. Register the result in
// a ToolRegistry and list its ID in the agent's definition.
//
// It adds no implicit agent behavior: retrieval happens only when the model
// calls the tool. The metadata filter and result-count cap are fixed at
// construction and are not model-settable, so a model chooses what to search
// for but not what it may read.
func NewRetrievalTool(config RetrievalToolConfig) (*RetrievalTool, error) {
	return runtime.NewRetrievalTool(config)
}

// ChunkID renders the stable identifier for a chunk at the given ordinal
// position within a document, matching what Chunker implementations assign.
func ChunkID(documentID string, index int) string { return runtime.ChunkID(documentID, index) }

func NewAgent(config AgentConfig) (*Agent, error) {
	return runtime.NewAgent(config)
}

func NewProviderRegistry() *ProviderRegistry {
	return runtime.NewProviderRegistry()
}

func NewModelRouter(config ModelRouterConfig) (*ModelRouter, error) {
	return runtime.NewModelRouter(config)
}

func DefaultModelRetryable(err *ModelError) bool {
	return runtime.DefaultModelRetryable(err)
}

func NewLinearWorkflow(config LinearWorkflowConfig) (*LinearWorkflow, error) {
	return runtime.NewLinearWorkflow(config)
}

// NewScheduler returns a Scheduler that fires durable schedules from the
// configured store, reusing LinearWorkflow.Run for each execution. It requires
// a non-nil Store and Resolver. Schedules persisted before a restart resume
// automatically because due work is reloaded from the store on every tick.
func NewScheduler(config SchedulerConfig) (*Scheduler, error) {
	return runtime.NewScheduler(config)
}

// ParseCronSpec compiles a five-field cron expression or an "@every <duration>"
// interval into a CronSchedule whose Next reports the following fire time.
func ParseCronSpec(spec string) (CronSchedule, error) {
	return runtime.ParseCronSpec(spec)
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
