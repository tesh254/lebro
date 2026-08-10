package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
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
	// Model is the provider-neutral language-model adapter. Required when
	// Router is nil.
	Model Model
	// Router optionally routes model calls through a provider registry with
	// routing policies and fallback chains. When set, Model is ignored.
	Router *ModelRouter
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
	// Store optionally binds agent runs to a durable thread. When non-nil and
	// RunInput.ThreadID is set, the agent loads prior messages from the thread
	// before the run and appends the new transcript on success. Failed runs
	// leave no messages, so the thread's message sequence stays valid. When
	// nil, agent behavior is unchanged.
	Store Store
}

// Agent repeatedly asks a model, executes requested tools, and feeds results
// back until a terminal response is produced or a configured bound is reached.
// The zero value is not usable; construct one with NewAgent.
type Agent struct {
	definition     AgentDefinition
	model          Model
	router         *ModelRouter
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
	store          Store
}

var _ Workflow = (*Agent)(nil)

// NewAgent validates the configuration and returns an agent safe for concurrent
// use. The returned agent implements Workflow so it can participate in later
// orchestration without a separate adapter.
func NewAgent(config AgentConfig) (*Agent, error) {
	if config.Definition.ID == "" {
		return nil, errors.New("lebro: agent definition ID is required")
	}
	if config.Router == nil && (config.Model == nil || isNilInterface(config.Model)) {
		return nil, errors.New("lebro: agent model or router is required")
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
		router:         config.Router,
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
		store:          config.Store,
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
	emitter := newRunEmitter(ctx, a.listener, a.clock, a.idSource)
	if err := ctx.Err(); err != nil {
		runID := a.idSource.NewRunID()
		emitter.terminal(runID, 0, "", RunEventCancelled, RunStatusCancelled, err)
		return RunResult{ID: runID, Status: RunStatusCancelled, Messages: nil, Metadata: input.Metadata}, a.cancelledError(0, err)
	}

	runCtx, cancel := a.applyDeadline(ctx)
	defer cancel()

	runID := a.idSource.NewRunID()
	metadata := cloneMetadata(input.Metadata)
	var allAttempts []ModelAttempt

	emitter.emit(runID, 0, "", RunEventStarted)

	loadedCount, err := a.loadPriorMessages(ctx, &input)
	if err != nil {
		emitter.terminal(runID, 0, "", RunEventFailed, RunStatusFailed, err)
		return a.fail(runID, input, 0, err)
	}

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
			return a.cancelledWithAttempts(runID, transcript, metadata, step, err, allAttempts)
		}

		stepID := a.idSource.NewStepID()
		modelStart := emitter.emitModelStarted(runID, step, stepID)

		request := ModelRequest{
			Model:        a.definition.Model,
			Messages:     cloneMessages(transcript),
			Tools:        cloneToolDefinitions(toolDefinitions),
			OutputSchema: cloneModelOutputSchema(outputSchema),
		}
		response, attempts, err := a.generateModel(runCtx, runID, step, stepID, emitter, request)
		allAttempts = append(allAttempts, attempts...)
		if err != nil {
			if cancelledErr := runCtx.Err(); errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || cancelledErr != nil {
				cause := preferContextError(err, cancelledErr)
				emitter.emitModelFinished(runID, step, stepID, modelStart, FinishReasonUnspecified, ModelUsage{}, cause)
				emitter.terminal(runID, step, stepID, RunEventCancelled, RunStatusCancelled, cause)
				return a.cancelledWithAttempts(runID, transcript, metadata, step, cause, allAttempts)
			}
			emitter.emitModelFinished(runID, step, stepID, modelStart, FinishReasonUnspecified, ModelUsage{}, err)
			emitter.terminal(runID, step, stepID, RunEventFailed, RunStatusFailed, err)
			agentErr := &AgentError{Kind: AgentErrorProviderFailure, Step: step, Err: err}
			result := a.failWithAttemptsResult(runID, metadata, step, transcript, agentErr, allAttempts)
			return result, agentErr
		}
		if err := response.Validate(); err != nil {
			if compiledOutput != nil && response.FinishReason != FinishReasonToolCalls &&
				errors.Is(err, ErrMessageStructuredOutputInvalidJSON) {
				structuredErr := &AgentError{Kind: AgentErrorInvalidStructuredOutput, Step: step, Err: errors.New("lebro: structured output must be valid JSON")}
				emitter.emitModelFinished(runID, step, stepID, modelStart, FinishReasonUnspecified, ModelUsage{}, structuredErr)
				emitter.terminal(runID, step, stepID, RunEventFailed, RunStatusFailed, structuredErr)
				result := a.failWithAttemptsResult(runID, metadata, step, transcript, structuredErr, allAttempts)
				return result, structuredErr
			}
			failure := &ModelError{Kind: ModelErrorMalformedResponse, Provider: "agent", Message: err.Error(), Err: err}
			emitter.emitModelFinished(runID, step, stepID, modelStart, FinishReasonUnspecified, ModelUsage{}, failure)
			emitter.terminal(runID, step, stepID, RunEventFailed, RunStatusFailed, failure)
			agentErr := &AgentError{Kind: AgentErrorProviderFailure, Step: step, Err: failure}
			result := a.failWithAttemptsResult(runID, metadata, step, transcript, agentErr, allAttempts)
			return result, agentErr
		}

		emitter.emitModelFinished(runID, step, stepID, modelStart, response.FinishReason, response.Usage, nil)

		transcript = append(transcript, cloneMessage(response.Message))

		if response.FinishReason != FinishReasonToolCalls {
			if err := a.validateStructuredOutput(compiledOutput, response); err != nil {
				agentErr := &AgentError{Kind: AgentErrorInvalidStructuredOutput, Step: step, Err: err}
				emitter.terminal(runID, step, stepID, RunEventFailed, RunStatusFailed, agentErr)
				result := a.failWithAttemptsResult(runID, metadata, step, transcript, agentErr, allAttempts)
				return result, agentErr
			}
			result := RunResult{
				ID:            runID,
				Status:        RunStatusSucceeded,
				Messages:      transcript,
				Metadata:      metadata,
				ModelAttempts: allAttempts,
			}
			if persistErr := a.persistNewMessages(ctx, input.ThreadID, runID, transcript, loadedCount); persistErr != nil {
				persistAgentErr := &AgentError{Kind: AgentErrorProviderFailure, Step: step, Err: persistErr}
				emitter.terminal(runID, step, stepID, RunEventFailed, RunStatusFailed, persistAgentErr)
				result := a.failWithAttemptsResult(runID, metadata, step, transcript, persistAgentErr, allAttempts)
				return result, persistAgentErr
			}
			emitter.terminal(runID, step, stepID, RunEventSucceeded, RunStatusSucceeded, nil)
			return result, nil
		}

		toolCalls := response.Message.ToolCalls.Values()
		for _, call := range toolCalls {
			if err := runCtx.Err(); err != nil {
				emitter.terminal(runID, step, stepID, RunEventCancelled, RunStatusCancelled, err)
				return a.cancelledWithAttempts(runID, transcript, metadata, step, err, allAttempts)
			}
			emitter.emitToolRequested(runID, step, stepID, call.ID, call.ToolID)

			toolStart := emitter.emitToolStarted(runID, step, stepID, call.ID, call.ToolID)

			result := a.executeToolCall(runCtx, runID, step, call, metadata)
			emitter.emitToolFinished(runID, step, stepID, toolStart, call.ID, call.ToolID, result.State, result.Err)

			transcript = append(transcript, toolResultMessage(call.ID, result))
			if result.State == ToolExecutionCancelled {
				emitter.terminal(runID, step, stepID, RunEventCancelled, RunStatusCancelled, result.Err)
				return a.cancelledWithAttempts(runID, transcript, metadata, step, result.Err, allAttempts)
			}
			if result.State != ToolExecutionSucceeded {
				agentErr := toolExecutionAgentError(step, result)
				emitter.terminal(runID, step, stepID, RunEventFailed, RunStatusFailed, agentErr)
				failResult := a.failWithAttemptsResult(runID, metadata, step, transcript, agentErr, allAttempts)
				return failResult, agentErr
			}
		}
	}

	exhausted := &AgentError{Kind: AgentErrorStepLimitExhausted, Step: a.maxSteps, Err: ErrAgentStepLimitExhausted}
	emitter.terminal(runID, a.maxSteps, "", RunEventFailed, RunStatusFailed, exhausted)
	result := a.failWithAttemptsResult(runID, metadata, a.maxSteps, transcript, exhausted, allAttempts)
	return result, exhausted
}

// StreamRun is the handle returned by Agent.RunStream. The caller drains
// Deltas until it is closed, then calls Wait to collect the terminal RunResult
// and error. Cancel unblocks any in-flight streaming work and releases
// goroutine resources even when the caller abandons the stream before
// draining; it is safe to call after Wait returns.
type StreamRun struct {
	Deltas   <-chan StreamDelta
	done     chan streamOutcome
	finished chan struct{}
	cancel   context.CancelFunc
	once     sync.Once
}

// Cancel unblocks the run goroutine and any in-flight provider stream. It is
// safe to call multiple times and must be invoked when the caller stops
// reading Deltas before the stream completes naturally.
func (s *StreamRun) Cancel() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
	})
}

// Wait blocks until the run goroutine has completed. It must be called after
// Deltas has been fully drained (for example, with a for-range loop) so the
// run goroutine is not blocked writing to the channel. The returned RunResult
// and error are the terminal values of the streaming run; on cancellation the
// result Status is RunStatusCancelled and the error wraps ErrAgentCancelled.
func (s *StreamRun) Wait() (RunResult, error) {
	if s == nil {
		return RunResult{}, errors.New("lebro: stream run is nil")
	}
	outcome, ok := <-s.done
	if !ok {
		return RunResult{}, errors.New("lebro: stream ended without outcome")
	}
	<-s.finished
	return outcome.result, outcome.err
}

// Drain is a convenience helper that drains Deltas to completion and returns
// the terminal Wait outcome. It is the canonical way to collect the final
// result when the caller does not need to inspect each delta.
func (s *StreamRun) Drain() (RunResult, error) {
	if s == nil {
		return RunResult{}, errors.New("lebro: stream run is nil")
	}
	for range s.Deltas {
	}
	return s.Wait()
}

// RunStream executes the bounded agent loop and streams ordered StreamDelta
// values to the caller before returning the final RunResult. The returned
// StreamRun owns the delta channel; the caller drains Deltas until it is
// closed, then calls Wait (or Drain) to collect the terminal RunResult and
// error. The caller must defer StreamRun.Cancel so goroutine resources are
// released even when the stream is abandoned before completion.
//
// When the configured Model implements StreamingModel, each model call
// streams text, tool-call, and structured-output deltas as they arrive;
// otherwise the run falls back to Generate and emits a single delta per step
// so streaming and non-streaming runs produce equivalent final records.
//
// Cancellation propagates through the provider stream, tool execution, and the
// loop itself: cancelling the context (or calling StreamRun.Cancel) stops
// active work, closes the delta channel, and returns a RunResult with Status
// RunStatusCancelled and an error wrapping ErrAgentCancelled.
func (a *Agent) RunStream(ctx context.Context, input RunInput) (*StreamRun, error) {
	if a == nil {
		return nil, &AgentError{Kind: AgentErrorProviderFailure, Err: errors.New("lebro: agent is nil")}
	}

	emitter := newRunEmitter(ctx, a.listener, a.clock, a.idSource)
	if err := ctx.Err(); err != nil {
		runID := a.idSource.NewRunID()
		emitter.terminal(runID, 0, "", RunEventCancelled, RunStatusCancelled, err)
		return nil, a.cancelledError(0, err)
	}

	runCtx, deadlineCancel := a.applyDeadline(ctx)
	runCtx, streamCancel := context.WithCancel(runCtx)
	cancel := func() {
		streamCancel()
		deadlineCancel()
	}

	runID := a.idSource.NewRunID()
	metadata := cloneMetadata(input.Metadata)

	loadedCount, err := a.loadPriorMessages(ctx, &input)
	if err != nil {
		cancel()
		emitter.terminal(runID, 0, "", RunEventFailed, RunStatusFailed, err)
		return nil, err
	}

	transcript, err := a.buildInitialTranscript(input)
	if err != nil {
		cancel()
		emitter.terminal(runID, 0, "", RunEventFailed, RunStatusFailed, err)
		return nil, err
	}

	toolDefinitions, err := a.resolveToolDefinitions()
	if err != nil {
		cancel()
		emitter.terminal(runID, 0, "", RunEventFailed, RunStatusFailed, err)
		return nil, err
	}

	outputSchema, compiledOutput, err := a.resolveOutputSchema(input)
	if err != nil {
		cancel()
		emitter.terminal(runID, 0, "", RunEventFailed, RunStatusFailed, err)
		return nil, err
	}

	streamingModel := a.streamingModelForRun()

	deltas := make(chan StreamDelta, 1)
	done := make(chan streamOutcome, 1)
	finished := make(chan struct{})

	emitter.emit(runID, 0, "", RunEventStarted)

	run := &StreamRun{
		Deltas:   deltas,
		done:     done,
		finished: finished,
		cancel:   cancel,
	}

	go a.runStreamLoop(streamRunParams{
		ctx:             runCtx,
		parentCtx:       ctx,
		runID:           runID,
		metadata:        metadata,
		transcript:      transcript,
		toolDefinitions: toolDefinitions,
		outputSchema:    outputSchema,
		compiledOutput:  compiledOutput,
		streamingModel:  streamingModel,
		emitter:         emitter,
		deltas:          deltas,
		done:            done,
		finished:        finished,
		threadID:        input.ThreadID,
		loadedCount:     loadedCount,
	})

	return run, nil
}

// runStreamOutcome carries the final result of a streaming run. It is sent
// once on the done channel when the run goroutine exits.
type streamOutcome struct {
	result RunResult
	err    error
}

type streamRunParams struct {
	ctx             context.Context
	parentCtx       context.Context
	runID           RunID
	metadata        map[string]string
	transcript      []Message
	toolDefinitions []ToolDefinition
	outputSchema    *ModelOutputSchema
	compiledOutput  CompiledSchema
	streamingModel  StreamingModel
	emitter         *runEmitter
	deltas          chan<- StreamDelta
	done            chan<- streamOutcome
	finished        chan<- struct{}
	threadID        ThreadID
	loadedCount     int
}

func (a *Agent) runStreamLoop(p streamRunParams) {
	defer close(p.deltas)
	defer close(p.done)
	defer close(p.finished)

	transcript := p.transcript
	var allAttempts []ModelAttempt

	for step := 1; step <= a.maxSteps; step++ {
		if err := p.ctx.Err(); err != nil {
			p.emitter.terminal(p.runID, step, "", RunEventCancelled, RunStatusCancelled, err)
			p.done <- streamOutcome{result: a.cancelledWithAttemptsResult(p.runID, transcript, p.metadata, step, err, allAttempts), err: a.cancelledError(step, err)}
			return
		}

		stepID := a.idSource.NewStepID()
		modelStart := p.emitter.emitModelStarted(p.runID, step, stepID)

		request := ModelRequest{
			Model:        a.definition.Model,
			Messages:     cloneMessages(transcript),
			Tools:        cloneToolDefinitions(p.toolDefinitions),
			OutputSchema: cloneModelOutputSchema(p.outputSchema),
		}

		response, attempts, streamErr := a.consumeStream(p.ctx, p.runID, step, stepID, modelStart, p.emitter, p.deltas, request, p.streamingModel)
		allAttempts = append(allAttempts, attempts...)
		if streamErr != nil {
			cause := streamErr
			if cancelledErr := p.ctx.Err(); errors.Is(streamErr, context.Canceled) || errors.Is(streamErr, context.DeadlineExceeded) || cancelledErr != nil {
				cause = preferContextError(streamErr, cancelledErr)
				p.emitter.emitModelFinished(p.runID, step, stepID, modelStart, FinishReasonUnspecified, ModelUsage{}, cause)
				p.emitter.terminal(p.runID, step, stepID, RunEventCancelled, RunStatusCancelled, cause)
				p.done <- streamOutcome{result: a.cancelledWithAttemptsResult(p.runID, transcript, p.metadata, step, cause, allAttempts), err: a.cancelledError(step, cause)}
				return
			}
			p.emitter.emitModelFinished(p.runID, step, stepID, modelStart, FinishReasonUnspecified, ModelUsage{}, cause)
			p.emitter.terminal(p.runID, step, stepID, RunEventFailed, RunStatusFailed, cause)
			agentErr := &AgentError{Kind: AgentErrorProviderFailure, Step: step, Err: cause}
			p.done <- streamOutcome{result: a.failWithAttemptsResult(p.runID, p.metadata, step, transcript, agentErr, allAttempts), err: agentErr}
			return
		}

		if err := response.Validate(); err != nil {
			if p.compiledOutput != nil && response.FinishReason != FinishReasonToolCalls &&
				errors.Is(err, ErrMessageStructuredOutputInvalidJSON) {
				structuredErr := &AgentError{Kind: AgentErrorInvalidStructuredOutput, Step: step, Err: errors.New("lebro: structured output must be valid JSON")}
				p.emitter.emitModelFinished(p.runID, step, stepID, modelStart, FinishReasonUnspecified, ModelUsage{}, structuredErr)
				p.emitter.terminal(p.runID, step, stepID, RunEventFailed, RunStatusFailed, structuredErr)
				p.done <- streamOutcome{result: a.failWithAttemptsResult(p.runID, p.metadata, step, transcript, structuredErr, allAttempts), err: structuredErr}
				return
			}
			failure := &ModelError{Kind: ModelErrorMalformedResponse, Provider: "agent", Message: err.Error(), Err: err}
			p.emitter.emitModelFinished(p.runID, step, stepID, modelStart, FinishReasonUnspecified, ModelUsage{}, failure)
			p.emitter.terminal(p.runID, step, stepID, RunEventFailed, RunStatusFailed, failure)
			agentErr := &AgentError{Kind: AgentErrorProviderFailure, Step: step, Err: failure}
			p.done <- streamOutcome{result: a.failWithAttemptsResult(p.runID, p.metadata, step, transcript, agentErr, allAttempts), err: agentErr}
			return
		}

		p.emitter.emitModelFinished(p.runID, step, stepID, modelStart, response.FinishReason, response.Usage, nil)
		transcript = append(transcript, cloneMessage(response.Message))

		if response.FinishReason != FinishReasonToolCalls {
			if err := a.validateStructuredOutput(p.compiledOutput, response); err != nil {
				agentErr := &AgentError{Kind: AgentErrorInvalidStructuredOutput, Step: step, Err: err}
				p.emitter.terminal(p.runID, step, stepID, RunEventFailed, RunStatusFailed, agentErr)
				p.done <- streamOutcome{result: a.failWithAttemptsResult(p.runID, p.metadata, step, transcript, agentErr, allAttempts), err: agentErr}
				return
			}
			result := RunResult{ID: p.runID, Status: RunStatusSucceeded, Messages: cloneMessages(transcript), Metadata: p.metadata, ModelAttempts: allAttempts}
			persistErr := a.persistNewMessages(p.parentCtx, p.threadID, p.runID, transcript, p.loadedCount)
			if persistErr != nil {
				persistAgentErr := &AgentError{Kind: AgentErrorProviderFailure, Step: step, Err: persistErr}
				p.emitter.terminal(p.runID, step, stepID, RunEventFailed, RunStatusFailed, persistAgentErr)
				p.done <- streamOutcome{result: a.failWithAttemptsResult(p.runID, p.metadata, step, transcript, persistAgentErr, allAttempts), err: persistAgentErr}
				return
			}
			p.emitter.terminal(p.runID, step, stepID, RunEventSucceeded, RunStatusSucceeded, nil)
			p.done <- streamOutcome{result: result, err: nil}
			return
		}

		toolCalls := response.Message.ToolCalls.Values()
		for _, call := range toolCalls {
			if err := p.ctx.Err(); err != nil {
				p.emitter.terminal(p.runID, step, stepID, RunEventCancelled, RunStatusCancelled, err)
				p.done <- streamOutcome{result: a.cancelledWithAttemptsResult(p.runID, transcript, p.metadata, step, err, allAttempts), err: a.cancelledError(step, err)}
				return
			}
			p.emitter.emitToolRequested(p.runID, step, stepID, call.ID, call.ToolID)
			toolStart := p.emitter.emitToolStarted(p.runID, step, stepID, call.ID, call.ToolID)
			result := a.executeToolCall(p.ctx, p.runID, step, call, p.metadata)
			p.emitter.emitToolFinished(p.runID, step, stepID, toolStart, call.ID, call.ToolID, result.State, result.Err)
			transcript = append(transcript, toolResultMessage(call.ID, result))
			if result.State == ToolExecutionCancelled {
				p.emitter.terminal(p.runID, step, stepID, RunEventCancelled, RunStatusCancelled, result.Err)
				p.done <- streamOutcome{result: a.cancelledWithAttemptsResult(p.runID, transcript, p.metadata, step, result.Err, allAttempts), err: a.cancelledError(step, result.Err)}
				return
			}
			if result.State != ToolExecutionSucceeded {
				agentErr := toolExecutionAgentError(step, result)
				p.emitter.terminal(p.runID, step, stepID, RunEventFailed, RunStatusFailed, agentErr)
				p.done <- streamOutcome{result: a.failWithAttemptsResult(p.runID, p.metadata, step, transcript, agentErr, allAttempts), err: agentErr}
				return
			}
		}
	}

	exhausted := &AgentError{Kind: AgentErrorStepLimitExhausted, Step: a.maxSteps, Err: ErrAgentStepLimitExhausted}
	p.emitter.terminal(p.runID, a.maxSteps, "", RunEventFailed, RunStatusFailed, exhausted)
	p.done <- streamOutcome{result: a.failWithAttemptsResult(p.runID, p.metadata, a.maxSteps, transcript, exhausted, allAttempts), err: exhausted}
}

// consumeStream drains one model call's deltas into the caller's channel and
// returns the aggregated terminal response. When the adapter does not
// implement StreamingModel, it falls back to Generate and emits a single
// delta carrying the full response so streaming callers observe equivalent
// output shape.
func (a *Agent) consumeStream(ctx context.Context, runID RunID, step int, stepID StepID, modelStart time.Time, emitter *runEmitter, deltas chan<- StreamDelta, request ModelRequest, streamingModel StreamingModel) (ModelResponse, []ModelAttempt, error) {
	if streamingModel == nil {
		var response ModelResponse
		var err error
		var attempts []ModelAttempt
		if a.router != nil {
			result, genErr := a.router.GenerateWithAttempts(ctx, request)
			response = result.Response
			err = genErr
			attempts = result.Attempts
		} else {
			response, err = a.model.Generate(ctx, request)
		}
		if err != nil {
			return ModelResponse{}, attempts, err
		}
		// Emit deltas representing the complete response so streaming
		// consumers observe every tool call and the terminal payload.
		calls := response.Message.ToolCalls.Values()
		for i := range calls {
			call := calls[i]
			delta := StreamDelta{ToolCall: &call}
			if err := delta.Validate(); err != nil {
				return ModelResponse{}, attempts, &ModelError{Kind: ModelErrorMalformedResponse, Provider: "agent", Message: err.Error(), Err: err}
			}
			emitter.emitDelta(runID, step, stepID, delta)
			if !sendDelta(ctx, deltas, delta) {
				return ModelResponse{}, attempts, context.Canceled
			}
		}
		terminal := StreamDelta{
			Text:         response.Message.Content,
			FinishReason: response.FinishReason,
			Usage:        response.Usage,
		}
		if response.Message.StructuredOutput != "" {
			terminal.StructuredOutput = response.Message.StructuredOutput
		}
		if terminal.Text == "" && terminal.ToolCall == nil && terminal.StructuredOutput == "" && terminal.FinishReason == "" {
			terminal.FinishReason = FinishReasonUnspecified
		}
		if err := terminal.Validate(); err != nil {
			return ModelResponse{}, attempts, &ModelError{Kind: ModelErrorMalformedResponse, Provider: "agent", Message: err.Error(), Err: err}
		}
		emitter.emitDelta(runID, step, stepID, terminal)
		if !sendDelta(ctx, deltas, terminal) {
			return ModelResponse{}, attempts, context.Canceled
		}
		return response, attempts, nil
	}

	var reader StreamReader
	var attempts []ModelAttempt
	var err error
	if a.router != nil {
		result, streamErr := a.router.streamWithAttempts(ctx, request, &agentModelAttemptObserver{
			emitter: emitter,
			runID:   runID,
			step:    step,
			stepID:  stepID,
			starts:  make(map[ProviderID]time.Time),
		})
		reader = result.Reader
		attempts = result.Attempts
		err = streamErr
	} else {
		reader, err = streamingModel.Stream(ctx, request)
	}
	if err != nil {
		return ModelResponse{}, attempts, err
	}
	defer func() { _ = reader.Close() }()

	var contentBuilder strings.Builder
	var toolCalls []ModelToolCall
	var structuredOutput ModelStructuredOutput
	var finishReason FinishReason
	var usage ModelUsage

	for {
		if err := ctx.Err(); err != nil {
			return ModelResponse{}, attempts, err
		}
		delta, derr := reader.Next()
		if errors.Is(derr, io.EOF) {
			if err := ctx.Err(); err != nil {
				return ModelResponse{}, attempts, err
			}
			break
		}
		if derr != nil {
			return ModelResponse{}, attempts, derr
		}
		if err := delta.Validate(); err != nil {
			return ModelResponse{}, attempts, &ModelError{Kind: ModelErrorMalformedResponse, Provider: "agent", Message: err.Error(), Err: err}
		}
		emitter.emitDelta(runID, step, stepID, delta)
		if !sendDelta(ctx, deltas, delta) {
			return ModelResponse{}, attempts, context.Canceled
		}
		if delta.Text != "" {
			contentBuilder.WriteString(delta.Text)
		}
		if delta.ToolCall != nil {
			toolCalls = append(toolCalls, cloneToolCallValue(*delta.ToolCall))
		}
		if delta.StructuredOutput != "" {
			structuredOutput = delta.StructuredOutput
		}
		if delta.FinishReason != "" {
			finishReason = delta.FinishReason
		}
		if delta.Usage != (ModelUsage{}) {
			usage = delta.Usage
		}
		if delta.Err != nil {
			return ModelResponse{}, attempts, delta.Err
		}
	}

	if finishReason == "" {
		finishReason = FinishReasonUnspecified
	}

	message := Message{
		Role:             RoleAssistant,
		Content:          contentBuilder.String(),
		StructuredOutput: structuredOutput,
	}
	if len(toolCalls) > 0 {
		encoded, err := NewModelToolCalls(toolCalls...)
		if err != nil {
			return ModelResponse{}, attempts, fmt.Errorf("lebro: aggregate stream tool calls: %w", err)
		}
		message.ToolCalls = encoded
	}

	response := ModelResponse{
		Message:      message,
		Usage:        usage,
		FinishReason: finishReason,
	}
	return response, attempts, nil
}

func (a *Agent) cancelledWithAttemptsResult(runID RunID, messages []Message, metadata map[string]string, step int, err error, attempts []ModelAttempt) RunResult {
	return RunResult{
		ID:            runID,
		Status:        RunStatusCancelled,
		Messages:      cloneMessages(messages),
		Metadata:      metadata,
		ModelAttempts: attempts,
	}
}

func cloneToolCallValue(call ModelToolCall) ModelToolCall {
	call.Arguments = cloneRawMessage(call.Arguments)
	return call
}

// sendDelta writes delta to ch unless ctx is cancelled. It returns false when
// the context was cancelled before the write completed, signalling the caller
// to abort the stream.
func sendDelta(ctx context.Context, ch chan<- StreamDelta, delta StreamDelta) bool {
	select {
	case ch <- delta:
		return true
	case <-ctx.Done():
		return false
	}
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

// generateModel calls the model either directly or through the router, emitting
// model_attempt events when routing is in use. It returns the response,
// accumulated model attempts, and any error.
func (a *Agent) generateModel(ctx context.Context, runID RunID, step int, stepID StepID, emitter *runEmitter, request ModelRequest) (ModelResponse, []ModelAttempt, error) {
	if a.router == nil {
		resp, err := a.model.Generate(ctx, request)
		return resp, nil, err
	}

	result, err := a.router.generateWithAttempts(ctx, request, &agentModelAttemptObserver{
		emitter: emitter,
		runID:   runID,
		step:    step,
		stepID:  stepID,
		starts:  make(map[ProviderID]time.Time),
	})

	return result.Response, result.Attempts, err
}

type agentModelAttemptObserver struct {
	emitter *runEmitter
	runID   RunID
	step    int
	stepID  StepID
	starts  map[ProviderID]time.Time
}

func (o *agentModelAttemptObserver) modelAttemptStarted(provider ProviderID, model string) {
	o.starts[provider] = o.emitter.emitModelAttemptStarted(o.runID, o.step, o.stepID, provider, model)
}

func (o *agentModelAttemptObserver) modelAttemptFinished(attempt ModelAttempt) {
	o.emitter.emitModelAttemptFinished(o.runID, o.step, o.stepID, attempt.Provider, attempt.Model, attempt.Status, o.starts[attempt.Provider], attempt.Error)
}

// streamingModelForRun returns the streaming model for the current run. When
// using a router, it returns the router itself (which implements StreamingModel).
// When using a direct model, it returns the streaming adapter if available.
func (a *Agent) streamingModelForRun() StreamingModel {
	if a.router != nil {
		return a.router
	}
	return AsStreamingModel(a.model)
}

func (a *Agent) cancelledWithAttempts(runID RunID, messages []Message, metadata map[string]string, step int, err error, attempts []ModelAttempt) (RunResult, error) {
	result := RunResult{
		ID:            runID,
		Status:        RunStatusCancelled,
		Messages:      cloneMessages(messages),
		Metadata:      metadata,
		ModelAttempts: attempts,
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

func (a *Agent) failWithAttemptsResult(runID RunID, metadata map[string]string, step int, messages []Message, agentErr *AgentError, attempts []ModelAttempt) RunResult {
	return RunResult{
		ID:            runID,
		Status:        RunStatusFailed,
		Messages:      cloneMessages(messages),
		Metadata:      metadata,
		ModelAttempts: attempts,
	}
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

// loadPriorMessages fetches the canonical message history for input.ThreadID
// from the store and prepends it to input.Messages. When the store is nil or
// ThreadID is empty, no persistence is configured and the input is unchanged.
// A missing thread (ErrNotFound) is treated as an empty history so the first
// run against a new thread starts cleanly.
func (a *Agent) loadPriorMessages(ctx context.Context, input *RunInput) (int, error) {
	if a.store == nil || input.ThreadID == "" {
		return 0, nil
	}
	page, err := a.store.Messages().ListMessages(ctx, input.ThreadID, PageRequest{Limit: int(^uint(0) >> 1)})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("lebro: load thread messages: %w", err)
	}
	if len(page.Records) == 0 {
		return 0, nil
	}
	prior := make([]Message, 0, len(page.Records))
	for _, record := range page.Records {
		prior = append(prior, cloneMessage(record.Message))
	}
	input.Messages = append(prior, input.Messages...)
	return len(prior), nil
}

// persistNewMessages appends the new messages produced during a successful run
// to the thread. The system message and prior loaded messages are excluded so
// only caller-supplied and loop-produced messages are stored. When the store is
// nil or threadID is empty, no persistence is configured and the method is a
// no-op. The thread is created if it does not already exist so the first run
// against a new thread ID succeeds without a separate CreateThread call. The
// transaction is retried on ErrConflict so concurrent successful runs against
// the same thread do not lose a transcript.
func (a *Agent) persistNewMessages(ctx context.Context, threadID ThreadID, runID RunID, transcript []Message, loadedCount int) error {
	if a.store == nil || threadID == "" {
		return nil
	}
	systemOffset := 0
	if a.definition.Instructions != "" {
		systemOffset = 1
	}
	start := systemOffset + loadedCount
	if start >= len(transcript) {
		return nil
	}
	now := a.clock.Now()
	records := make([]MessageRecord, 0, len(transcript)-start)
	for i, message := range transcript[start:] {
		records = append(records, MessageRecord{
			ID:        fmt.Sprintf("%s-msg-%d", runID, i+1),
			ThreadID:  threadID,
			Message:   cloneMessage(message),
			CreatedAt: now,
		})
	}
	const maxRetries = 3
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		err := a.store.Transaction(ctx, func(ctx context.Context, repos Repositories) error {
			if _, err := repos.Threads().GetThread(ctx, threadID); errors.Is(err, ErrNotFound) {
				if err := repos.Threads().CreateThread(ctx, ThreadRecord{
					ID:        threadID,
					CreatedAt: now,
					UpdatedAt: now,
				}); err != nil {
					return fmt.Errorf("lebro: create thread for persist: %w", err)
				}
			} else if err != nil {
				return fmt.Errorf("lebro: check thread for persist: %w", err)
			}
			return repos.Messages().AppendMessages(ctx, records)
		})
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrConflict) {
			return err
		}
		lastErr = err
	}
	return lastErr
}
