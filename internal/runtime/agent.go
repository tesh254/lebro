package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// AgentErrorKind identifies the normalized category of an agent-loop failure.
type AgentErrorKind string

const (
	// AgentErrorUnknownTool means the model requested a tool ID that is not
	// registered with the agent's tool registry, or that the agent definition
	// references a tool ID that cannot be resolved at run start.
	AgentErrorUnknownTool AgentErrorKind = "unknown_tool"
	// AgentErrorInvalidToolArguments means a model-requested tool invocation
	// failed input schema validation before the handler ran.
	AgentErrorInvalidToolArguments AgentErrorKind = "invalid_tool_arguments"
	// AgentErrorInvalidToolOutput means a tool handler returned output that
	// failed its output schema validation.
	AgentErrorInvalidToolOutput AgentErrorKind = "invalid_tool_output"
	// AgentErrorToolFailure means a tool handler returned an ordinary error or
	// panicked during invocation.
	AgentErrorToolFailure AgentErrorKind = "tool_failure"
	// AgentErrorProviderFailure means the model adapter returned a failure. The
	// wrapped error preserves the original [*ModelError] so callers can use
	// errors.As to inspect the normalized kind.
	AgentErrorProviderFailure AgentErrorKind = "provider_failure"
	// AgentErrorStepLimitExhausted means the loop consumed MaxSteps without the
	// model producing a terminal response.
	AgentErrorStepLimitExhausted AgentErrorKind = "step_limit_exhausted"
	// AgentErrorCancelled means the run context was cancelled before a
	// terminal result was produced. The wrapped error is the context error.
	AgentErrorCancelled AgentErrorKind = "cancelled"
	// AgentErrorInvalidStructuredOutput means a terminal model response either
	// omitted structured output when one was requested or produced a value
	// that failed local JSON Schema validation.
	AgentErrorInvalidStructuredOutput AgentErrorKind = "invalid_structured_output"
)

var (
	// ErrAgentUnknownTool matches failures where a requested tool cannot be
	// resolved by stable ID.
	ErrAgentUnknownTool = errors.New("lebro: agent unknown tool")
	// ErrAgentInvalidToolArguments matches failures where tool input schema
	// validation rejected model-supplied arguments.
	ErrAgentInvalidToolArguments = errors.New("lebro: agent invalid tool arguments")
	// ErrAgentInvalidToolOutput matches failures where tool output schema
	// validation rejected a handler result.
	ErrAgentInvalidToolOutput = errors.New("lebro: agent invalid tool output")
	// ErrAgentToolFailure matches ordinary handler errors and recovered
	// panics.
	ErrAgentToolFailure = errors.New("lebro: agent tool failure")
	// ErrAgentProviderFailure matches model adapter failures.
	ErrAgentProviderFailure = errors.New("lebro: agent provider failure")
	// ErrAgentStepLimitExhausted matches runs that consumed every permitted
	// step without reaching a terminal response.
	ErrAgentStepLimitExhausted = errors.New("lebro: agent step limit exhausted")
	// ErrAgentCancelled matches runs aborted by context cancellation.
	ErrAgentCancelled = errors.New("lebro: agent cancelled")
	// ErrAgentInvalidStructuredOutput matches runs whose terminal model
	// response omitted requested structured output or failed schema validation.
	ErrAgentInvalidStructuredOutput = errors.New("lebro: agent invalid structured output")
)

// AgentError preserves the category, failing step, and cause of an agent-loop
// failure. Step is 1-indexed; a zero step means the failure happened before the
// first model call (for example, tool resolution at run start).
type AgentError struct {
	Kind AgentErrorKind
	Step int
	Err  error
}

func (e *AgentError) Error() string {
	if e == nil {
		return "lebro: agent failure"
	}
	kind := e.Kind
	if kind == "" {
		kind = AgentErrorProviderFailure
	}
	detail := ""
	if e.Err != nil {
		detail = e.Err.Error()
	}
	step := ""
	if e.Step > 0 {
		step = fmt.Sprintf(" at step %d", e.Step)
	}
	if detail == "" {
		return fmt.Sprintf("lebro: agent %s%s", kind, step)
	}
	return fmt.Sprintf("lebro: agent %s%s: %s", kind, step, detail)
}

// Unwrap exposes the original model error, tool execution error, or context
// error.
func (e *AgentError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Is supports errors.Is checks against the normalized ErrAgent sentinels while
// Unwrap continues to preserve the original cause.
func (e *AgentError) Is(target error) bool {
	if e == nil {
		return false
	}
	return target == agentErrorSentinel(e.Kind)
}

func agentErrorSentinel(kind AgentErrorKind) error {
	switch kind {
	case AgentErrorUnknownTool:
		return ErrAgentUnknownTool
	case AgentErrorInvalidToolArguments:
		return ErrAgentInvalidToolArguments
	case AgentErrorInvalidToolOutput:
		return ErrAgentInvalidToolOutput
	case AgentErrorToolFailure:
		return ErrAgentToolFailure
	case AgentErrorProviderFailure:
		return ErrAgentProviderFailure
	case AgentErrorStepLimitExhausted:
		return ErrAgentStepLimitExhausted
	case AgentErrorCancelled:
		return ErrAgentCancelled
	case AgentErrorInvalidStructuredOutput:
		return ErrAgentInvalidStructuredOutput
	default:
		return errors.New("lebro: agent failure")
	}
}

// DefaultAgentMaxSteps is used when AgentConfig.MaxSteps is zero. It keeps an
// unbounded loop from exhausting a provider budget while still allowing a
// multi-turn tool conversation to complete.
const DefaultAgentMaxSteps = 10

// AgentConfig describes a bounded tool-using agent. Model and Definition are
// required; Tools may be nil for a text-only agent. A non-nil Tools registry
// is required when Definition.Tools is non-empty.
type AgentConfig struct {
	// Definition identifies the agent and supplies instructions, model name,
	// and the tool IDs the agent is permitted to invoke.
	Definition AgentDefinition
	// Model is the provider-neutral language-model adapter. Required.
	Model Model
	// Tools resolves schema-backed tool handlers by stable ID. May be nil when
	// Definition.Tools is empty.
	Tools *ToolRegistry
	// MaxSteps bounds the number of model calls in a single run. A zero value
	// uses DefaultAgentMaxSteps.
	MaxSteps int
	// Deadline caps the total run when non-zero. It is layered on top of any
	// deadline already present on the run context.
	Deadline time.Duration
	// Listener receives ordered run lifecycle events. May be nil to disable
	// recording entirely; a nil listener does not alter agent behavior.
	Listener RunListener
	// OutputSchema requests a final JSON value that conforms to Schema from
	// every terminal model response in a run. When non-nil, SchemaCompiler is
	// required and the agent validates the final structured payload locally.
	// A RunInput.OutputSchema override takes precedence over this value.
	OutputSchema *ModelOutputSchema
	// SchemaCompiler compiles OutputSchema at construction and any
	// RunInput.OutputSchema override at run start. Required when OutputSchema
	// is set; optional otherwise. When nil, run-level overrides are rejected.
	SchemaCompiler SchemaCompiler
	// Clock supplies timestamps for run events. When nil, the system clock is
	// used. Inject a fixed clock for deterministic tests.
	Clock Clock
	// IDSource generates stable run and step identifiers. When nil, a
	// sequential source is used. Inject a fixed source for deterministic
	// tests.
	IDSource IDSource
}

// Agent repeatedly asks a model, executes requested tools, and feeds results
// back until a terminal response is produced or a configured bound is reached.
// The zero value is not usable; construct one with NewAgent.
type Agent struct {
	definition     AgentDefinition
	model          Model
	tools          *ToolRegistry
	outputSchema   *ModelOutputSchema
	compiledOutput CompiledSchema
	schemaCompiler SchemaCompiler
	maxSteps       int
	deadline       time.Duration
	allowed        map[ToolID]struct{}
	listener       RunListener
	clock          Clock
	idSource       IDSource
}

var _ Workflow = (*Agent)(nil)

// NewAgent validates the configuration and returns an agent safe for concurrent
// use. The returned agent implements Workflow so it can participate in later
// orchestration without a separate adapter.
func NewAgent(config AgentConfig) (*Agent, error) {
	if config.Definition.ID == "" {
		return nil, errors.New("lebro: agent definition ID is required")
	}
	if config.Model == nil || isNilInterface(config.Model) {
		return nil, errors.New("lebro: agent model is required")
	}
	if len(config.Definition.Tools) > 0 && (config.Tools == nil) {
		return nil, errors.New("lebro: agent tool registry is required when the definition references tools")
	}
	if config.OutputSchema != nil {
		if len(config.OutputSchema.Schema) == 0 {
			return nil, errors.New("lebro: agent output schema must not be empty")
		}
		if !json.Valid(config.OutputSchema.Schema) {
			return nil, errors.New("lebro: agent output schema must be valid JSON")
		}
		if config.SchemaCompiler == nil || isNilInterface(config.SchemaCompiler) {
			return nil, errors.New("lebro: agent schema compiler is required when output schema is set")
		}
	}
	var compiledOutput CompiledSchema
	if config.OutputSchema != nil {
		compiled, err := config.SchemaCompiler.Compile(config.OutputSchema.Schema)
		if err != nil {
			return nil, fmt.Errorf("lebro: compile agent output schema: %w", err)
		}
		if compiled == nil || isNilInterface(compiled) {
			return nil, errors.New("lebro: schema compiler returned a nil output schema")
		}
		compiledOutput = compiled
	}
	outputSchema := cloneModelOutputSchema(config.OutputSchema)
	maxSteps := config.MaxSteps
	if maxSteps <= 0 {
		maxSteps = DefaultAgentMaxSteps
	}
	definition := AgentDefinition{
		ID:           config.Definition.ID,
		Name:         config.Definition.Name,
		Instructions: config.Definition.Instructions,
		Model:        config.Definition.Model,
		Tools:        append([]ToolID(nil), config.Definition.Tools...),
	}
	allowed := make(map[ToolID]struct{}, len(definition.Tools))
	for _, id := range definition.Tools {
		allowed[id] = struct{}{}
	}
	clock := config.Clock
	if clock == nil {
		clock = defaultClock{}
	}
	idSource := config.IDSource
	if idSource == nil {
		idSource = &sequentialIDSource{}
	}
	return &Agent{
		definition:     definition,
		model:          config.Model,
		tools:          config.Tools,
		outputSchema:   outputSchema,
		compiledOutput: compiledOutput,
		schemaCompiler: config.SchemaCompiler,
		maxSteps:       maxSteps,
		deadline:       config.Deadline,
		allowed:        allowed,
		listener:       config.Listener,
		clock:          clock,
		idSource:       idSource,
	}, nil
}

// Definition returns the agent's stable definition so the agent can be treated
// as a Workflow.
func (a *Agent) Definition() WorkflowDefinition {
	if a == nil {
		return WorkflowDefinition{}
	}
	return WorkflowDefinition{
		ID:          WorkflowID(a.definition.ID),
		Name:        a.definition.Name,
		Description: a.definition.Instructions,
	}
}

// Run executes the bounded agent loop and returns a complete non-streaming
// run result. The run honors context cancellation, enforces MaxSteps and
// optional Deadline, and appends every model response and tool result to the
// transcript in canonical order. When a Listener is configured, ordered run
// events are emitted for every lifecycle point; when the listener is nil,
// recording is disabled and agent behavior is unchanged.
func (a *Agent) Run(ctx context.Context, input RunInput) (RunResult, error) {
	if a == nil {
		return RunResult{}, &AgentError{Kind: AgentErrorProviderFailure, Err: errors.New("lebro: agent is nil")}
	}
	emitter := newRunEmitter(a.listener, a.clock, a.idSource)
	if err := ctx.Err(); err != nil {
		runID := a.idSource.NewRunID()
		emitter.terminal(runID, 0, "", RunEventCancelled, RunStatusCancelled, err)
		return RunResult{ID: runID, Status: RunStatusCancelled, Messages: nil, Metadata: input.Metadata}, a.cancelledError(0, err)
	}

	runCtx, cancel := a.applyDeadline(ctx)
	defer cancel()

	runID := a.idSource.NewRunID()
	metadata := cloneMetadata(input.Metadata)

	emitter.emit(runID, 0, "", RunEventStarted)

	transcript, err := a.buildInitialTranscript(input)
	if err != nil {
		emitter.terminal(runID, 0, "", RunEventFailed, RunStatusFailed, err)
		return a.fail(runID, input, 0, err)
	}

	toolDefinitions, err := a.resolveToolDefinitions()
	if err != nil {
		emitter.terminal(runID, 0, "", RunEventFailed, RunStatusFailed, err)
		return a.fail(runID, input, 0, err)
	}

	outputSchema, compiledOutput, err := a.resolveOutputSchema(input)
	if err != nil {
		emitter.terminal(runID, 0, "", RunEventFailed, RunStatusFailed, err)
		return a.fail(runID, input, 0, err)
	}

	for step := 1; step <= a.maxSteps; step++ {
		if err := runCtx.Err(); err != nil {
			emitter.terminal(runID, step, "", RunEventCancelled, RunStatusCancelled, err)
			return a.cancelled(runID, transcript, metadata, step, err)
		}

		stepID := a.idSource.NewStepID()
		modelStart := emitter.emitModelStarted(runID, step, stepID)

		request := ModelRequest{
			Model:        a.definition.Model,
			Messages:     cloneMessages(transcript),
			Tools:        cloneToolDefinitions(toolDefinitions),
			OutputSchema: cloneModelOutputSchema(outputSchema),
		}
		response, err := a.model.Generate(runCtx, request)
		if err != nil {
			if cancelledErr := runCtx.Err(); errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || cancelledErr != nil {
				cause := preferContextError(err, cancelledErr)
				emitter.emitModelFinished(runID, step, stepID, modelStart, FinishReasonUnspecified, ModelUsage{}, cause)
				emitter.terminal(runID, step, stepID, RunEventCancelled, RunStatusCancelled, cause)
				return a.cancelled(runID, transcript, metadata, step, cause)
			}
			emitter.emitModelFinished(runID, step, stepID, modelStart, FinishReasonUnspecified, ModelUsage{}, err)
			emitter.terminal(runID, step, stepID, RunEventFailed, RunStatusFailed, err)
			return a.failWithMessages(runID, metadata, step, transcript, &AgentError{Kind: AgentErrorProviderFailure, Step: step, Err: err})
		}
		if err := response.Validate(); err != nil {
			if compiledOutput != nil && response.FinishReason != FinishReasonToolCalls &&
				response.Message.StructuredOutput != "" && !json.Valid(response.Message.StructuredOutput.Raw()) {
				structuredErr := &AgentError{Kind: AgentErrorInvalidStructuredOutput, Step: step, Err: errors.New("lebro: structured output must be valid JSON")}
				emitter.emitModelFinished(runID, step, stepID, modelStart, FinishReasonUnspecified, ModelUsage{}, structuredErr)
				emitter.terminal(runID, step, stepID, RunEventFailed, RunStatusFailed, structuredErr)
				return a.failWithMessages(runID, metadata, step, transcript, structuredErr)
			}
			failure := &ModelError{Kind: ModelErrorMalformedResponse, Provider: "agent", Message: err.Error(), Err: err}
			emitter.emitModelFinished(runID, step, stepID, modelStart, FinishReasonUnspecified, ModelUsage{}, failure)
			emitter.terminal(runID, step, stepID, RunEventFailed, RunStatusFailed, failure)
			return a.failWithMessages(runID, metadata, step, transcript, &AgentError{Kind: AgentErrorProviderFailure, Step: step, Err: failure})
		}

		emitter.emitModelFinished(runID, step, stepID, modelStart, response.FinishReason, response.Usage, nil)

		transcript = append(transcript, cloneMessage(response.Message))

		if response.FinishReason != FinishReasonToolCalls {
			if err := a.validateStructuredOutput(compiledOutput, response); err != nil {
				agentErr := &AgentError{Kind: AgentErrorInvalidStructuredOutput, Step: step, Err: err}
				emitter.terminal(runID, step, stepID, RunEventFailed, RunStatusFailed, agentErr)
				return a.failWithMessages(runID, metadata, step, transcript, agentErr)
			}
			emitter.terminal(runID, step, stepID, RunEventSucceeded, RunStatusSucceeded, nil)
			return RunResult{
				ID:       runID,
				Status:   RunStatusSucceeded,
				Messages: transcript,
				Metadata: metadata,
			}, nil
		}

		toolCalls := response.Message.ToolCalls.Values()
		for _, call := range toolCalls {
			if err := runCtx.Err(); err != nil {
				emitter.terminal(runID, step, stepID, RunEventCancelled, RunStatusCancelled, err)
				return a.cancelled(runID, transcript, metadata, step, err)
			}
			emitter.emitToolRequested(runID, step, stepID, call.ID, call.ToolID)

			toolStart := emitter.emitToolStarted(runID, step, stepID, call.ID, call.ToolID)

			result := a.executeToolCall(runCtx, runID, step, call, metadata)
			emitter.emitToolFinished(runID, step, stepID, toolStart, call.ID, call.ToolID, result.State, result.Err)

			transcript = append(transcript, toolResultMessage(call.ID, result))
			if result.State == ToolExecutionCancelled {
				emitter.terminal(runID, step, stepID, RunEventCancelled, RunStatusCancelled, result.Err)
				return a.cancelled(runID, transcript, metadata, step, result.Err)
			}
			if result.State != ToolExecutionSucceeded {
				agentErr := toolExecutionAgentError(step, result)
				emitter.terminal(runID, step, stepID, RunEventFailed, RunStatusFailed, agentErr)
				return a.failWithMessages(runID, metadata, step, transcript, agentErr)
			}
		}
	}

	exhausted := &AgentError{Kind: AgentErrorStepLimitExhausted, Step: a.maxSteps, Err: ErrAgentStepLimitExhausted}
	emitter.terminal(runID, a.maxSteps, "", RunEventFailed, RunStatusFailed, exhausted)
	return a.failWithMessages(runID, metadata, a.maxSteps, transcript, exhausted)
}

func (a *Agent) buildInitialTranscript(input RunInput) ([]Message, error) {
	messages := make([]Message, 0, len(input.Messages)+1)
	if a.definition.Instructions != "" {
		system := Message{Role: RoleSystem, Content: a.definition.Instructions}
		if err := system.Validate(); err != nil {
			return nil, &AgentError{Kind: AgentErrorProviderFailure, Step: 0, Err: err}
		}
		messages = append(messages, system)
	}
	for i, message := range input.Messages {
		if err := message.Validate(); err != nil {
			return nil, &AgentError{Kind: AgentErrorProviderFailure, Step: 0, Err: fmt.Errorf("lebro: run input message %d: %w", i, err)}
		}
		messages = append(messages, cloneMessage(message))
	}
	return messages, nil
}

func (a *Agent) resolveToolDefinitions() ([]ToolDefinition, error) {
	if len(a.definition.Tools) == 0 {
		return nil, nil
	}
	if a.tools == nil {
		return nil, &AgentError{Kind: AgentErrorUnknownTool, Step: 0, Err: ErrAgentUnknownTool}
	}
	definitions := make([]ToolDefinition, 0, len(a.definition.Tools))
	seen := make(map[ToolID]struct{}, len(a.definition.Tools))
	for _, id := range a.definition.Tools {
		if _, exists := seen[id]; exists {
			return nil, &AgentError{Kind: AgentErrorUnknownTool, Step: 0, Err: fmt.Errorf("lebro: agent definition lists duplicate tool ID %q: %w", id, ErrAgentUnknownTool)}
		}
		seen[id] = struct{}{}
		tool, ok := a.tools.Resolve(id)
		if !ok {
			return nil, &AgentError{Kind: AgentErrorUnknownTool, Step: 0, Err: fmt.Errorf("lebro: agent tool %q is not registered: %w", id, ErrAgentUnknownTool)}
		}
		definitions = append(definitions, tool.Definition())
	}
	return definitions, nil
}

func (a *Agent) resolveOutputSchema(input RunInput) (*ModelOutputSchema, CompiledSchema, error) {
	if input.OutputSchema != nil {
		if len(input.OutputSchema.Schema) == 0 {
			return nil, nil, &AgentError{Kind: AgentErrorInvalidStructuredOutput, Err: errors.New("lebro: run output schema must not be empty")}
		}
		if !json.Valid(input.OutputSchema.Schema) {
			return nil, nil, &AgentError{Kind: AgentErrorInvalidStructuredOutput, Err: errors.New("lebro: run output schema must be valid JSON")}
		}
		if a.schemaCompiler == nil || isNilInterface(a.schemaCompiler) {
			return nil, nil, &AgentError{Kind: AgentErrorInvalidStructuredOutput, Err: errors.New("lebro: agent schema compiler is required when run output schema is set")}
		}
		compiled, err := a.schemaCompiler.Compile(input.OutputSchema.Schema)
		if err != nil {
			return nil, nil, &AgentError{Kind: AgentErrorInvalidStructuredOutput, Err: fmt.Errorf("lebro: compile run output schema: %w", err)}
		}
		if compiled == nil || isNilInterface(compiled) {
			return nil, nil, &AgentError{Kind: AgentErrorInvalidStructuredOutput, Err: errors.New("lebro: schema compiler returned a nil output schema")}
		}
		return input.OutputSchema, compiled, nil
	}
	return a.outputSchema, a.compiledOutput, nil
}

func (a *Agent) validateStructuredOutput(compiled CompiledSchema, response ModelResponse) error {
	if compiled == nil {
		return nil
	}
	if response.Message.StructuredOutput == "" {
		return errors.New("lebro: structured output is missing")
	}
	validationErr := compiled.Validate(response.Message.StructuredOutput.Raw())
	if validationErr == nil {
		return nil
	}
	return &ValidationError{
		Target: ValidationTargetStructuredOutput,
		Issues: sortedValidationIssues(validationErr.Issues),
	}
}

func (a *Agent) executeToolCall(ctx context.Context, runID RunID, step int, call ModelToolCall, metadata map[string]string) ToolExecutionResult {
	if a.tools == nil {
		return failedToolExecution(call.ToolID, ToolExecutionNotFound, fmt.Errorf("lebro: tool %q is not registered: %w", call.ToolID, ErrToolNotFound))
	}
	if _, ok := a.allowed[call.ToolID]; !ok {
		return failedToolExecution(call.ToolID, ToolExecutionNotFound, fmt.Errorf("lebro: tool %q is not allowed for this agent: %w", call.ToolID, ErrToolNotFound))
	}
	toolMetadata := make(map[string]string, len(metadata)+3)
	for key, value := range metadata {
		toolMetadata[key] = value
	}
	toolMetadata["run_id"] = string(runID)
	toolMetadata["step"] = fmt.Sprintf("%d", step)
	toolMetadata["tool_call_id"] = call.ID
	return a.tools.Execute(ctx, call.ToolID, ToolExecutionRequest{
		Arguments: cloneRawMessage(call.Arguments),
		Metadata:  toolMetadata,
	})
}

func (a *Agent) applyDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if a.deadline <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, a.deadline)
}

func (a *Agent) cancelled(runID RunID, messages []Message, metadata map[string]string, step int, err error) (RunResult, error) {
	result := RunResult{
		ID:       runID,
		Status:   RunStatusCancelled,
		Messages: cloneMessages(messages),
		Metadata: metadata,
	}
	return result, a.cancelledError(step, err)
}

func (a *Agent) cancelledError(step int, err error) *AgentError {
	return &AgentError{Kind: AgentErrorCancelled, Step: step, Err: preferContextError(err, err)}
}

func (a *Agent) fail(runID RunID, input RunInput, step int, err error) (RunResult, error) {
	return RunResult{
		ID:       runID,
		Status:   RunStatusFailed,
		Messages: cloneMessages(input.Messages),
		Metadata: cloneMetadata(input.Metadata),
	}, err
}

func (a *Agent) failWithMessages(runID RunID, metadata map[string]string, step int, messages []Message, agentErr *AgentError) (RunResult, error) {
	return RunResult{
		ID:       runID,
		Status:   RunStatusFailed,
		Messages: cloneMessages(messages),
		Metadata: metadata,
	}, agentErr
}

func toolResultMessage(callID string, result ToolExecutionResult) Message {
	content := string(result.Output)
	if result.State != ToolExecutionSucceeded {
		encoded, err := json.Marshal(map[string]any{
			"error":   result.Err.Error(),
			"state":   string(result.State),
			"tool_id": string(result.ToolID),
		})
		if err != nil {
			content = fmt.Sprintf(`{"error":%q}`, result.Err.Error())
		} else {
			content = string(encoded)
		}
	}
	return Message{Role: RoleTool, ToolCallID: callID, Content: content}
}

func toolExecutionAgentError(step int, result ToolExecutionResult) *AgentError {
	switch result.State {
	case ToolExecutionNotFound:
		return &AgentError{Kind: AgentErrorUnknownTool, Step: step, Err: result.Err}
	case ToolExecutionInvalidInput:
		return &AgentError{Kind: AgentErrorInvalidToolArguments, Step: step, Err: result.Err}
	case ToolExecutionInvalidOutput:
		return &AgentError{Kind: AgentErrorInvalidToolOutput, Step: step, Err: result.Err}
	case ToolExecutionCancelled:
		return &AgentError{Kind: AgentErrorCancelled, Step: step, Err: result.Err}
	default:
		return &AgentError{Kind: AgentErrorToolFailure, Step: step, Err: result.Err}
	}
}

func preferContextError(primary, fallback error) error {
	if primary == nil {
		return fallback
	}
	if errors.Is(primary, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(primary, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(fallback, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(fallback, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return primary
}

func cloneMessages(messages []Message) []Message {
	if messages == nil {
		return nil
	}
	cloned := make([]Message, len(messages))
	for i, message := range messages {
		cloned[i] = cloneMessage(message)
	}
	return cloned
}

func cloneMessage(message Message) Message {
	return message
}

func cloneToolDefinitions(definitions []ToolDefinition) []ToolDefinition {
	if definitions == nil {
		return nil
	}
	cloned := make([]ToolDefinition, len(definitions))
	for i, definition := range definitions {
		cloned[i] = cloneToolDefinition(definition)
	}
	return cloned
}
