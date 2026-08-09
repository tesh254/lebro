package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// WorkflowDefinition describes a named workflow before execution semantics are
// added by the workflow implementation tasks. Version is an opaque
// caller-supplied definition/version reference persisted with each run so
// readers can evolve their workflow schemas and still identify how a stored
// run was produced.
type WorkflowDefinition struct {
	ID          WorkflowID
	Name        string
	Description string
	Version     string
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
// Store optionally binds the workflow to a durable Store; when set, the
// executor persists the run record and a snapshot at each successful step
// boundary inside a single transaction. A persistence failure fails the run
// with a WorkflowErrorStepFailed wrapping the storage error, so a process
// restart never observes a partially persisted step.
type LinearWorkflowConfig struct {
	Definition     WorkflowDefinition
	Steps          []Step
	SchemaCompiler SchemaCompiler
	Listener       RunListener
	Clock          Clock
	IDSource       IDSource
	Store          Store
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
	store      Store
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
	if clock == nil || isNilInterface(clock) {
		clock = defaultClock{}
	}
	idSource := config.IDSource
	if idSource == nil || isNilInterface(idSource) {
		idSource = &sequentialIDSource{}
	}

	definition := WorkflowDefinition{
		ID:          config.Definition.ID,
		Name:        config.Definition.Name,
		Description: config.Definition.Description,
		Version:     config.Definition.Version,
	}

	return &LinearWorkflow{
		definition: definition,
		steps:      compiledSteps,
		listener:   config.Listener,
		clock:      clock,
		idSource:   idSource,
		store:      config.Store,
	}, nil
}

// Definition returns the workflow's stable definition.
func (w *LinearWorkflow) Definition() WorkflowDefinition {
	if w == nil {
		return WorkflowDefinition{}
	}
	return w.definition
}

// workflowSnapshotSchemaVersion is the envelope version the linear workflow
// executor writes on every snapshot. Readers tolerate 0 (legacy/unspecified);
// the initial release line never writes any other value.
const workflowSnapshotSchemaVersion = 1

// workflowSnapshotEnvelope is the JSON state persisted at each successful step
// boundary. It carries the current step's output and the ordered completed
// outputs so a future resumable executor can rebuild state without replaying
// the run record.
type workflowSnapshotEnvelope struct {
	Step    int               `json:"step"`
	StepID  StepID            `json:"step_id,omitempty"`
	Output  json.RawMessage   `json:"output,omitempty"`
	Outputs []json.RawMessage `json:"outputs,omitempty"`
}

// runAnchor captures the stable run fields at start time so every persistence
// point — step boundary and terminal — writes a consistent run record. The
// executor never mutates an anchor after it is captured.
type runAnchor struct {
	runID     RunID
	input     WorkflowRunInput
	metadata  map[string]string
	startedAt time.Time
}

// Run executes the configured steps in declared order. Each step receives the
// validated output of the previous step (or WorkflowRunInput.Input for the
// first step), its input and output are validated against compiled schemas
// when present, and the final step's output becomes the run output. When a
// Listener is configured, ordered run events are emitted for every lifecycle
// point; when the listener is nil, recording is disabled and workflow behavior
// is unchanged.
//
// When the workflow is bound to a Store, the run record is persisted as
// Running before the first step, and a snapshot plus an updated run record are
// written transactionally after each successful step. A persistence failure
// fails the run with a WorkflowErrorStepFailed wrapping the storage error, so
// a process restart never observes a partially persisted step. Terminal
// persistence failures on the success path are also surfaced as
// WorkflowErrorStepFailed so a caller never observes a completed run that
// durable storage still reports as Running.
func (w *LinearWorkflow) Run(ctx context.Context, input WorkflowRunInput) (WorkflowRunResult, error) {
	if w == nil {
		return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorStepFailed, Err: errors.New("lebro: workflow is nil")}
	}
	emitter := newRunEmitter(ctx, w.listener, w.clock, w.idSource)
	if err := ctx.Err(); err != nil {
		runID := w.idSource.NewRunID()
		metadata := cloneMetadata(input.Metadata)
		anchor := w.newAnchor(runID, input, metadata)
		emitter.terminal(runID, 0, "", RunEventCancelled, RunStatusCancelled, err)
		w.persistTerminalBestEffort(anchor, 0, "", nil, RunStatusCancelled, &WorkflowError{Kind: WorkflowErrorCancelled, Err: err})
		return w.cancelled(runID, metadata, 0, "", err)
	}

	runID := w.idSource.NewRunID()
	metadata := cloneMetadata(input.Metadata)
	current := cloneRawMessage(input.Input)
	completedOutputs := make([]json.RawMessage, 0, len(w.steps))
	anchor := w.newAnchor(runID, input, metadata)

	if perr := w.persistRunStart(ctx, anchor); perr != nil {
		stepErr := &WorkflowError{Kind: WorkflowErrorStepFailed, Err: fmt.Errorf("lebro: persist workflow run start: %w", perr)}
		emitter.terminal(runID, 0, "", RunEventFailed, RunStatusFailed, stepErr)
		w.persistTerminalBestEffort(anchor, 0, "", nil, RunStatusFailed, stepErr)
		return w.fail(runID, metadata, stepErr)
	}

	emitter.emit(runID, 0, "", RunEventStarted)

	for i, step := range w.steps {
		position := i + 1
		stepID := step.definition.ID

		if err := ctx.Err(); err != nil {
			emitter.terminal(runID, position, stepID, RunEventCancelled, RunStatusCancelled, err)
			w.persistTerminalBestEffort(anchor, position-1, lastStepID(w.steps, i-1), completedOutputs, RunStatusCancelled, &WorkflowError{Kind: WorkflowErrorCancelled, Step: position, StepID: stepID, Err: err})
			return w.cancelled(runID, metadata, position, stepID, err)
		}

		stepStart := emitter.emitStepStarted(runID, position, stepID)

		if err := validateStepValue(step.inputSchema, ValidationTargetStepInput, current); err != nil {
			stepErr := &WorkflowError{Kind: WorkflowErrorInvalidStepInput, Step: position, StepID: stepID, Err: err}
			emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
			emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
			w.persistTerminalBestEffort(anchor, position-1, lastStepID(w.steps, i-1), completedOutputs, RunStatusFailed, stepErr)
			return w.fail(runID, metadata, stepErr)
		}

		stepCtx := withWorkflowInvocation(ctx, runID, position, stepID, input.ThreadID, metadata)
		output, panicValue, panicked, handlerErr := invokeStepHandler(stepCtx, step.handler, cloneRawMessage(current))

		if err := ctx.Err(); err != nil {
			cause := preferContextError(err, err)
			stepErr := &WorkflowError{Kind: WorkflowErrorCancelled, Step: position, StepID: stepID, Err: cause}
			emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
			emitter.terminal(runID, position, stepID, RunEventCancelled, RunStatusCancelled, cause)
			w.persistTerminalBestEffort(anchor, position-1, lastStepID(w.steps, i-1), completedOutputs, RunStatusCancelled, stepErr)
			return w.cancelled(runID, metadata, position, stepID, cause)
		}

		if panicked {
			stepErr := &WorkflowError{Kind: WorkflowErrorStepPanicked, Step: position, StepID: stepID, Err: &StepPanicError{Value: panicValue}}
			emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
			emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
			w.persistTerminalBestEffort(anchor, position-1, lastStepID(w.steps, i-1), completedOutputs, RunStatusFailed, stepErr)
			return w.fail(runID, metadata, stepErr)
		}

		if handlerErr != nil {
			if errors.Is(handlerErr, context.Canceled) || errors.Is(handlerErr, context.DeadlineExceeded) {
				stepErr := &WorkflowError{Kind: WorkflowErrorCancelled, Step: position, StepID: stepID, Err: handlerErr}
				emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
				emitter.terminal(runID, position, stepID, RunEventCancelled, RunStatusCancelled, handlerErr)
				w.persistTerminalBestEffort(anchor, position-1, lastStepID(w.steps, i-1), completedOutputs, RunStatusCancelled, stepErr)
				return w.cancelled(runID, metadata, position, stepID, handlerErr)
			}
			stepErr := &WorkflowError{Kind: WorkflowErrorStepFailed, Step: position, StepID: stepID, Err: handlerErr}
			emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
			emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
			w.persistTerminalBestEffort(anchor, position-1, lastStepID(w.steps, i-1), completedOutputs, RunStatusFailed, stepErr)
			return w.fail(runID, metadata, stepErr)
		}

		if err := validateStepValue(step.outputSchema, ValidationTargetStepOutput, output); err != nil {
			stepErr := &WorkflowError{Kind: WorkflowErrorInvalidStepOutput, Step: position, StepID: stepID, Err: err}
			emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
			emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
			w.persistTerminalBestEffort(anchor, position-1, lastStepID(w.steps, i-1), completedOutputs, RunStatusFailed, stepErr)
			return w.fail(runID, metadata, stepErr)
		}

		if perr := w.persistStep(ctx, anchor, position, stepID, output, completedOutputs); perr != nil {
			stepErr := &WorkflowError{Kind: WorkflowErrorStepFailed, Step: position, StepID: stepID, Err: fmt.Errorf("lebro: persist workflow step %q: %w", stepID, perr)}
			emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
			emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
			w.persistTerminalBestEffort(anchor, position-1, lastStepID(w.steps, i-1), completedOutputs, RunStatusFailed, stepErr)
			return w.fail(runID, metadata, stepErr)
		}

		emitter.emitStepFinished(runID, position, stepID, stepStart, nil)
		current = cloneRawMessage(output)
		completedOutputs = append(completedOutputs, cloneRawMessage(output))
	}

	finalOutput := cloneRawMessage(current)
	if perr := w.persistTerminal(anchor, len(w.steps), lastStepID(w.steps, len(w.steps)-1), completedOutputs, RunStatusSucceeded, nil); perr != nil {
		stepErr := &WorkflowError{Kind: WorkflowErrorStepFailed, Err: fmt.Errorf("lebro: persist workflow terminal state: %w", perr)}
		emitter.terminal(runID, len(w.steps), "", RunEventFailed, RunStatusFailed, stepErr)
		return w.fail(runID, metadata, stepErr)
	}
	emitter.terminal(runID, len(w.steps), "", RunEventSucceeded, RunStatusSucceeded, nil)
	return WorkflowRunResult{
		ID:       runID,
		Status:   RunStatusSucceeded,
		Output:   finalOutput,
		Metadata: metadata,
	}, nil
}

// lastStepID returns the step ID at index i, or "" when i is out of range. It
// is used to populate WorkflowRunRecord.CurrentStepID on terminal persistence
// where the current step is the last step that completed.
func lastStepID(steps []compiledStep, i int) StepID {
	if i < 0 || i >= len(steps) {
		return ""
	}
	return steps[i].definition.ID
}

// persistTerminalBestEffort writes the terminal run record on failure and
// cancellation paths. The run already reached a terminal error in memory, so
// a terminal persistence failure is best-effort and the original error is
// preserved for the caller.
func (w *LinearWorkflow) persistTerminalBestEffort(anchor runAnchor, currentStep int, currentStepID StepID, completed []json.RawMessage, status RunStatus, failure *WorkflowError) {
	_ = w.persistTerminal(anchor, currentStep, currentStepID, completed, status, failure)
}

// newAnchor captures the stable run fields at start time. The anchor is read
// by every persistence point so a step-boundary update or terminal record
// never drops the original input, thread ID, metadata, or start time.
func (w *LinearWorkflow) newAnchor(runID RunID, input WorkflowRunInput, metadata map[string]string) runAnchor {
	startedAt := time.Time{}
	if w.store != nil && !isNilInterface(w.store) {
		startedAt = w.clock.Now()
	}
	return runAnchor{
		runID:     runID,
		input:     input,
		metadata:  metadata,
		startedAt: startedAt,
	}
}

// persistRunStart writes the initial Running run record. A failure here is
// returned to the caller so the run can fail before emitting RunEventStarted.
// When no Store is bound it is a no-op and never touches the Clock, so a
// workflow with a nil listener and nil Store never invokes the Clock.
func (w *LinearWorkflow) persistRunStart(ctx context.Context, anchor runAnchor) error {
	if w.store == nil || isNilInterface(w.store) {
		return nil
	}
	record := w.baseRunRecord(anchor)
	record.Status = RunStatusRunning
	record.Input = cloneRawMessage(anchor.input.Input)
	record.CurrentStep = 0
	record.UpdatedAt = w.clock.Now()
	return w.store.WorkflowRuns().SaveWorkflowRun(ctx, record)
}

// persistStep writes a snapshot at the completed step boundary and updates
// the run record inside a single Store.Transaction so the boundary is
// atomic. The updated run record carries the anchor's stable fields (input,
// thread ID, metadata, original start time) and the ordered completed
// outputs through the current step, so a process stop after a committed
// step leaves a resumable, inspectable Running record. When no Store is
// bound it is a no-op.
func (w *LinearWorkflow) persistStep(ctx context.Context, anchor runAnchor, position int, stepID StepID, output json.RawMessage, completed []json.RawMessage) error {
	if w.store == nil || isNilInterface(w.store) {
		return nil
	}
	stepOutputs := append(cloneRawOutputs(completed), cloneRawMessage(output))
	envelope := workflowSnapshotEnvelope{
		Step:    position,
		StepID:  stepID,
		Output:  cloneRawMessage(output),
		Outputs: cloneRawOutputs(stepOutputs),
	}
	state, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("lebro: encode workflow snapshot: %w", err)
	}
	now := w.clock.Now()
	snapshot := WorkflowSnapshotRecord{
		ID:            fmt.Sprintf("%s-snapshot-%d", anchor.runID, position),
		RunID:         anchor.runID,
		Sequence:      int64(position),
		SchemaVersion: workflowSnapshotSchemaVersion,
		State:         state,
		CreatedAt:     now,
	}
	updated := w.baseRunRecord(anchor)
	updated.Status = RunStatusRunning
	updated.CurrentStep = position
	updated.CurrentStepID = stepID
	updated.StepOutputs = stepOutputs
	updated.UpdatedAt = now
	return w.store.Transaction(ctx, func(ctx context.Context, repos Repositories) error {
		if err := repos.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, snapshot); err != nil {
			return err
		}
		return repos.WorkflowRuns().SaveWorkflowRun(ctx, updated)
	})
}

// persistTerminal writes the terminal run record. On the success path a
// persistence failure is returned so the caller never observes a completed
// run that durable storage still reports as Running; the caller surfaces it
// as a WorkflowErrorStepFailed. On failure and cancellation paths the run
// already reached a terminal error in memory, so a terminal persistence
// failure is best-effort and the original error is preserved. When no Store
// is bound it is a no-op.
func (w *LinearWorkflow) persistTerminal(anchor runAnchor, currentStep int, currentStepID StepID, completed []json.RawMessage, status RunStatus, failure *WorkflowError) error {
	if w.store == nil || isNilInterface(w.store) {
		return nil
	}
	now := w.clock.Now()
	record := w.baseRunRecord(anchor)
	record.Status = status
	record.CurrentStep = currentStep
	record.CurrentStepID = currentStepID
	record.StepOutputs = cloneRawOutputs(completed)
	record.FinishedAt = &now
	record.UpdatedAt = now
	if failure != nil {
		record.Failure = &WorkflowFailureData{
			Kind:    failure.Kind,
			Step:    failure.Step,
			StepID:  failure.StepID,
			Message: failureMessage(failure),
		}
	}
	if status == RunStatusSucceeded && len(completed) > 0 {
		record.Output = cloneRawMessage(completed[len(completed)-1])
	}
	if err := w.store.WorkflowRuns().SaveWorkflowRun(context.Background(), record); err != nil {
		if status == RunStatusSucceeded {
			return err
		}
		// Failure and cancellation paths already report the original cause to
		// the caller; a terminal persistence failure there is best-effort so
		// the caller sees the workflow's own terminal state.
		return nil
	}
	return nil
}

// baseRunRecord builds a WorkflowRunRecord with the stable fields shared by
// every persistence point. Input is only set on the initial record by the
// caller; later persistence points keep the stored input by leaving it empty
// on the upsert only when the anchor's input is empty. Step and terminal
// persistence reuse the anchor's input, thread ID, metadata, and start time
// so a step-boundary update never resets them.
func (w *LinearWorkflow) baseRunRecord(anchor runAnchor) WorkflowRunRecord {
	record := WorkflowRunRecord{
		ID:              anchor.runID,
		WorkflowID:      w.definition.ID,
		WorkflowVersion: w.definition.Version,
		StartedAt:       anchor.startedAt,
	}
	if anchor.input.ThreadID != "" {
		record.ThreadID = anchor.input.ThreadID
	}
	if len(anchor.input.Input) > 0 {
		record.Input = cloneRawMessage(anchor.input.Input)
	}
	if encoded, ok := encodeMetadata(anchor.metadata); ok {
		record.Metadata = encoded
	}
	return record
}

func failureMessage(failure *WorkflowError) string {
	if failure == nil || failure.Err == nil {
		return ""
	}
	return failure.Err.Error()
}

func cloneRawOutputs(outputs []json.RawMessage) []json.RawMessage {
	if len(outputs) == 0 {
		return nil
	}
	cloned := make([]json.RawMessage, len(outputs))
	for i, output := range outputs {
		cloned[i] = cloneRawMessage(output)
	}
	return cloned
}

func encodeMetadata(metadata map[string]string) (json.RawMessage, bool) {
	if len(metadata) == 0 {
		return nil, false
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return nil, false
	}
	return encoded, true
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
