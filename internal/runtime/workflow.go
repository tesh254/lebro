package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// WorkflowDefinition describes a named workflow before execution semantics are
// added by the workflow implementation tasks.
type WorkflowDefinition struct {
	ID          WorkflowID
	Name        string
	Description string
}

// Workflow is the common contract implemented by message-centric executable
// workflows. The Agent type implements it so an agent can participate in
// orchestration without a separate adapter. JSON-step workflows such as
// LinearWorkflow use their own run API because their input and output are raw
// JSON values rather than conversation messages.
type Workflow interface {
	Definition() WorkflowDefinition
	Run(context.Context, RunInput) (RunResult, error)
}

// StepHandler is implemented by workflow step handlers. A handler receives the
// validated output of the previous step (or the run input for the first step)
// as raw JSON and returns raw JSON that is validated against the next step's
// input schema before being passed on.
type StepHandler interface {
	Execute(context.Context, json.RawMessage) (json.RawMessage, error)
}

// StepHandlerFunc lets an ordinary Go function satisfy StepHandler.
type StepHandlerFunc func(context.Context, json.RawMessage) (json.RawMessage, error)

// Execute calls f, satisfying StepHandler.
func (f StepHandlerFunc) Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	return f(ctx, input)
}

// StepDefinition describes one named, ordered step in a linear workflow.
// InputSchema and OutputSchema are optional JSON Schemas; when present, the
// executor compiles them once and validates each handoff. A step with no
// InputSchema accepts any value; a step with no OutputSchema returns unchecked
// output.
type StepDefinition struct {
	ID           StepID
	Name         string
	Description  string
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
}

// Step pairs a declared step with its handler.
type Step struct {
	Definition StepDefinition
	Handler    StepHandler
}

// WorkflowErrorKind identifies the normalized category of a workflow failure.
type WorkflowErrorKind string

const (
	// WorkflowErrorInvalidStepInput means a step's input (the previous step's
	// output or the run input for the first step) failed its input schema
	// validation before the handler ran.
	WorkflowErrorInvalidStepInput WorkflowErrorKind = "invalid_step_input"
	// WorkflowErrorInvalidStepOutput means a step handler returned output that
	// failed its output schema validation.
	WorkflowErrorInvalidStepOutput WorkflowErrorKind = "invalid_step_output"
	// WorkflowErrorStepFailed means a step handler returned an ordinary error.
	WorkflowErrorStepFailed WorkflowErrorKind = "step_failed"
	// WorkflowErrorStepPanicked means a step handler panicked during
	// invocation; the panic value is captured as a StepPanicError.
	WorkflowErrorStepPanicked WorkflowErrorKind = "step_panicked"
	// WorkflowErrorCancelled means the run context was cancelled before a
	// terminal result was produced. The wrapped error is the context error.
	WorkflowErrorCancelled WorkflowErrorKind = "cancelled"
)

var (
	// ErrWorkflowInvalidStepInput matches failures where a step input schema
	// rejected the handoff from the previous step.
	ErrWorkflowInvalidStepInput = errors.New("lebro: workflow invalid step input")
	// ErrWorkflowInvalidStepOutput matches failures where a step output schema
	// rejected a handler result.
	ErrWorkflowInvalidStepOutput = errors.New("lebro: workflow invalid step output")
	// ErrWorkflowStepFailure matches ordinary step handler errors.
	ErrWorkflowStepFailure = errors.New("lebro: workflow step failure")
	// ErrWorkflowStepPanicked matches recovered step handler panics.
	ErrWorkflowStepPanicked = errors.New("lebro: workflow step panicked")
	// ErrWorkflowCancelled matches runs aborted by context cancellation.
	ErrWorkflowCancelled = errors.New("lebro: workflow cancelled")
)

// WorkflowError preserves the category, failing step, and cause of a workflow
// failure. Step is 1-indexed; a zero step means the failure happened before the
// first step. StepID is the declared step identifier.
type WorkflowError struct {
	Kind   WorkflowErrorKind
	Step   int
	StepID StepID
	Err    error
}

func (e *WorkflowError) Error() string {
	if e == nil {
		return "lebro: workflow failure"
	}
	kind := e.Kind
	if kind == "" {
		kind = WorkflowErrorStepFailed
	}
	detail := ""
	if e.Err != nil {
		detail = e.Err.Error()
	}
	step := ""
	if e.StepID != "" {
		step = fmt.Sprintf(" at step %q", e.StepID)
	} else if e.Step > 0 {
		step = fmt.Sprintf(" at step %d", e.Step)
	}
	if detail == "" {
		return fmt.Sprintf("lebro: workflow %s%s", kind, step)
	}
	return fmt.Sprintf("lebro: workflow %s%s: %s", kind, step, detail)
}

// Unwrap exposes the original handler error, validation error, or context
// error.
func (e *WorkflowError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Is supports errors.Is checks against the normalized ErrWorkflow sentinels
// while Unwrap continues to preserve the original cause.
func (e *WorkflowError) Is(target error) bool {
	if e == nil {
		return false
	}
	return target == workflowErrorSentinel(e.Kind)
}

func workflowErrorSentinel(kind WorkflowErrorKind) error {
	switch kind {
	case WorkflowErrorInvalidStepInput:
		return ErrWorkflowInvalidStepInput
	case WorkflowErrorInvalidStepOutput:
		return ErrWorkflowInvalidStepOutput
	case WorkflowErrorStepFailed:
		return ErrWorkflowStepFailure
	case WorkflowErrorStepPanicked:
		return ErrWorkflowStepPanicked
	case WorkflowErrorCancelled:
		return ErrWorkflowCancelled
	default:
		return errors.New("lebro: workflow failure")
	}
}

// StepPanicError records a recovered step handler panic without allowing the
// panic value's formatting methods to trigger another panic.
type StepPanicError struct {
	Value any
}

func (e *StepPanicError) Error() string {
	if e == nil {
		return ""
	}
	if message, ok := e.Value.(string); ok {
		return "lebro: step handler panicked: " + message
	}
	return fmt.Sprintf("lebro: step handler panicked with %T", e.Value)
}

// WorkflowRunInput is the JSON-centric input for a linear workflow run.
type WorkflowRunInput struct {
	Input    json.RawMessage
	ThreadID ThreadID
	Metadata map[string]string
}

// WorkflowRunResult is the JSON-centric result of a linear workflow run. Output
// is the validated output of the final step.
type WorkflowRunResult struct {
	ID       RunID
	Status   RunStatus
	Output   json.RawMessage
	Metadata map[string]string
}

// DecodeOutput unmarshals the final step output into the caller-provided value.
// It returns an error when the run produced no output or the payload cannot be
// decoded into v.
func (r WorkflowRunResult) DecodeOutput(v any) error {
	if len(r.Output) == 0 {
		return errors.New("lebro: workflow run result has no output")
	}
	if err := json.Unmarshal(r.Output, v); err != nil {
		return fmt.Errorf("lebro: decode workflow output: %w", err)
	}
	return nil
}

// LinearWorkflowConfig describes a linear workflow composed of ordered, typed
// steps. Definition is required; Steps must contain at least one entry.
// SchemaCompiler is required when any step declares an input or output schema.
type LinearWorkflowConfig struct {
	Definition     WorkflowDefinition
	Steps          []Step
	SchemaCompiler SchemaCompiler
	Listener       RunListener
	Clock          Clock
	IDSource       IDSource
}

// LinearWorkflow executes typed steps in declared order, validating each
// handoff against compiled JSON Schemas, and emits workflow and step lifecycle
// records through the existing run-event model. The zero value is not usable;
// construct one with NewLinearWorkflow.
type LinearWorkflow struct {
	definition WorkflowDefinition
	steps      []compiledStep
	listener   RunListener
	clock      Clock
	idSource   IDSource
}

// NewLinearWorkflow validates the configuration, compiles step schemas once,
// and returns a workflow safe for concurrent use.
func NewLinearWorkflow(config LinearWorkflowConfig) (*LinearWorkflow, error) {
	if config.Definition.ID == "" {
		return nil, errors.New("lebro: workflow definition ID is required")
	}
	if len(config.Steps) == 0 {
		return nil, errors.New("lebro: workflow must have at least one step")
	}

	hasSchema := false
	seen := make(map[StepID]struct{}, len(config.Steps))
	for _, step := range config.Steps {
		if step.Definition.ID == "" {
			return nil, errors.New("lebro: workflow step ID is required")
		}
		if _, exists := seen[step.Definition.ID]; exists {
			return nil, fmt.Errorf("lebro: workflow step ID %q is already registered", step.Definition.ID)
		}
		seen[step.Definition.ID] = struct{}{}
		if step.Handler == nil || isNilInterface(step.Handler) {
			return nil, fmt.Errorf("lebro: workflow step %q handler is required", step.Definition.ID)
		}
		if len(step.Definition.InputSchema) > 0 || len(step.Definition.OutputSchema) > 0 {
			hasSchema = true
		}
	}
	if hasSchema && (config.SchemaCompiler == nil || isNilInterface(config.SchemaCompiler)) {
		return nil, errors.New("lebro: workflow schema compiler is required when steps declare schemas")
	}

	compiledSteps := make([]compiledStep, 0, len(config.Steps))
	for _, step := range config.Steps {
		compiled, err := newCompiledStep(config.SchemaCompiler, step)
		if err != nil {
			return nil, fmt.Errorf("lebro: compile workflow step %q: %w", step.Definition.ID, err)
		}
		compiledSteps = append(compiledSteps, compiled)
	}

	clock := config.Clock
	if clock == nil {
		clock = defaultClock{}
	}
	idSource := config.IDSource
	if idSource == nil {
		idSource = &sequentialIDSource{}
	}

	definition := WorkflowDefinition{
		ID:          config.Definition.ID,
		Name:        config.Definition.Name,
		Description: config.Definition.Description,
	}

	return &LinearWorkflow{
		definition: definition,
		steps:      compiledSteps,
		listener:   config.Listener,
		clock:      clock,
		idSource:   idSource,
	}, nil
}

// Definition returns the workflow's stable definition.
func (w *LinearWorkflow) Definition() WorkflowDefinition {
	if w == nil {
		return WorkflowDefinition{}
	}
	return w.definition
}

// Run executes the configured steps in declared order. Each step receives the
// validated output of the previous step (or WorkflowRunInput.Input for the
// first step), its input and output are validated against compiled schemas
// when present, and the final step's output becomes the run output. When a
// Listener is configured, ordered run events are emitted for every lifecycle
// point; when the listener is nil, recording is disabled and workflow behavior
// is unchanged.
func (w *LinearWorkflow) Run(ctx context.Context, input WorkflowRunInput) (WorkflowRunResult, error) {
	if w == nil {
		return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorStepFailed, Err: errors.New("lebro: workflow is nil")}
	}
	emitter := newRunEmitter(w.listener, w.clock, w.idSource)
	if err := ctx.Err(); err != nil {
		runID := w.idSource.NewRunID()
		metadata := cloneMetadata(input.Metadata)
		emitter.terminal(runID, 0, "", RunEventCancelled, RunStatusCancelled, err)
		return w.cancelled(runID, metadata, 0, "", err)
	}

	runID := w.idSource.NewRunID()
	metadata := cloneMetadata(input.Metadata)
	current := cloneRawMessage(input.Input)

	emitter.emit(runID, 0, "", RunEventStarted)

	for i, step := range w.steps {
		position := i + 1
		stepID := step.definition.ID

		if err := ctx.Err(); err != nil {
			emitter.terminal(runID, position, stepID, RunEventCancelled, RunStatusCancelled, err)
			return w.cancelled(runID, metadata, position, stepID, err)
		}

		stepStart := emitter.emitStepStarted(runID, position, stepID)

		if err := validateStepValue(step.inputSchema, ValidationTargetStepInput, current); err != nil {
			stepErr := &WorkflowError{Kind: WorkflowErrorInvalidStepInput, Step: position, StepID: stepID, Err: err}
			emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
			emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
			return w.fail(runID, metadata, stepErr)
		}

		output, panicValue, panicked, handlerErr := invokeStepHandler(ctx, step.handler, cloneRawMessage(current))

		if err := ctx.Err(); err != nil {
			cause := preferContextError(err, err)
			stepErr := &WorkflowError{Kind: WorkflowErrorCancelled, Step: position, StepID: stepID, Err: cause}
			emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
			emitter.terminal(runID, position, stepID, RunEventCancelled, RunStatusCancelled, cause)
			return w.cancelled(runID, metadata, position, stepID, cause)
		}

		if panicked {
			stepErr := &WorkflowError{Kind: WorkflowErrorStepPanicked, Step: position, StepID: stepID, Err: &StepPanicError{Value: panicValue}}
			emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
			emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
			return w.fail(runID, metadata, stepErr)
		}

		if handlerErr != nil {
			if errors.Is(handlerErr, context.Canceled) || errors.Is(handlerErr, context.DeadlineExceeded) {
				stepErr := &WorkflowError{Kind: WorkflowErrorCancelled, Step: position, StepID: stepID, Err: handlerErr}
				emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
				emitter.terminal(runID, position, stepID, RunEventCancelled, RunStatusCancelled, handlerErr)
				return w.cancelled(runID, metadata, position, stepID, handlerErr)
			}
			stepErr := &WorkflowError{Kind: WorkflowErrorStepFailed, Step: position, StepID: stepID, Err: handlerErr}
			emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
			emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
			return w.fail(runID, metadata, stepErr)
		}

		if err := validateStepValue(step.outputSchema, ValidationTargetStepOutput, output); err != nil {
			stepErr := &WorkflowError{Kind: WorkflowErrorInvalidStepOutput, Step: position, StepID: stepID, Err: err}
			emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
			emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
			return w.fail(runID, metadata, stepErr)
		}

		emitter.emitStepFinished(runID, position, stepID, stepStart, nil)
		current = cloneRawMessage(output)
	}

	emitter.terminal(runID, len(w.steps), "", RunEventSucceeded, RunStatusSucceeded, nil)
	return WorkflowRunResult{
		ID:       runID,
		Status:   RunStatusSucceeded,
		Output:   cloneRawMessage(current),
		Metadata: metadata,
	}, nil
}

func (w *LinearWorkflow) fail(runID RunID, metadata map[string]string, stepErr *WorkflowError) (WorkflowRunResult, error) {
	return WorkflowRunResult{
		ID:       runID,
		Status:   RunStatusFailed,
		Metadata: metadata,
	}, stepErr
}

func (w *LinearWorkflow) cancelled(runID RunID, metadata map[string]string, step int, stepID StepID, err error) (WorkflowRunResult, error) {
	return WorkflowRunResult{
		ID:       runID,
		Status:   RunStatusCancelled,
		Metadata: metadata,
	}, &WorkflowError{Kind: WorkflowErrorCancelled, Step: step, StepID: stepID, Err: preferContextError(err, err)}
}

type compiledStep struct {
	definition   StepDefinition
	handler      StepHandler
	inputSchema  CompiledSchema
	outputSchema CompiledSchema
}

func newCompiledStep(compiler SchemaCompiler, step Step) (compiledStep, error) {
	cs := compiledStep{
		definition: cloneStepDefinition(step.Definition),
		handler:    step.Handler,
	}
	if len(step.Definition.InputSchema) > 0 {
		if !json.Valid(step.Definition.InputSchema) {
			return cs, errors.New("input schema must be valid JSON")
		}
		compiled, err := compiler.Compile(step.Definition.InputSchema)
		if err != nil {
			return cs, fmt.Errorf("compile input schema: %w", err)
		}
		if compiled == nil || isNilInterface(compiled) {
			return cs, errors.New("schema compiler returned a nil input schema")
		}
		cs.inputSchema = compiled
	}
	if len(step.Definition.OutputSchema) > 0 {
		if !json.Valid(step.Definition.OutputSchema) {
			return cs, errors.New("output schema must be valid JSON")
		}
		compiled, err := compiler.Compile(step.Definition.OutputSchema)
		if err != nil {
			return cs, fmt.Errorf("compile output schema: %w", err)
		}
		if compiled == nil || isNilInterface(compiled) {
			return cs, errors.New("schema compiler returned a nil output schema")
		}
		cs.outputSchema = compiled
	}
	return cs, nil
}

func validateStepValue(compiled CompiledSchema, target ValidationTarget, value json.RawMessage) error {
	if compiled == nil {
		return nil
	}
	if validationErr := compiled.Validate(value); validationErr != nil {
		return &ValidationError{
			Target: target,
			Issues: sortedValidationIssues(validationErr.Issues),
		}
	}
	return nil
}

func invokeStepHandler(ctx context.Context, handler StepHandler, input json.RawMessage) (output json.RawMessage, panicValue any, panicked bool, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			output = nil
			err = nil
			panicValue = recovered
			panicked = true
		}
	}()
	output, err = handler.Execute(ctx, input)
	return output, nil, false, err
}

func cloneStepDefinition(def StepDefinition) StepDefinition {
	def.InputSchema = cloneRawMessage(def.InputSchema)
	def.OutputSchema = cloneRawMessage(def.OutputSchema)
	return def
}
