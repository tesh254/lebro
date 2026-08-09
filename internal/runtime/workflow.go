package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
// output. SuspendSchema is an optional JSON Schema that the executor compiles
// once and validates a SuspendSignal.Contract against when a handler signals
// suspend; a step that may suspend without a SuspendSchema is rejected as an
// invalid step output so a process restart never observes an unvalidated
// resume contract. Retry optionally configures retry behavior for transient
// handler failures; a nil Retry means the step runs exactly once with no retry.
type StepDefinition struct {
	ID            StepID
	Name          string
	Description   string
	InputSchema   json.RawMessage
	OutputSchema  json.RawMessage
	SuspendSchema json.RawMessage
	Retry         *RetryPolicy
}

// Step pairs a declared step with its handler.
type Step struct {
	Definition StepDefinition
	Handler    StepHandler
}

// RetryablePredicate decides whether a step handler error is eligible for
// retry. It receives the raw handler error; validation and context errors are
// handled separately by the executor and never reach the predicate. A nil
// predicate is equivalent to DefaultRetryable.
type RetryablePredicate func(error) bool

// DefaultRetryable returns true for handler errors that are not context
// cancellation or deadline errors. Context errors are surfaced as cancellation
// by the executor regardless of the predicate, but DefaultRetryable rejects
// them defensively so a custom predicate that delegates to it keeps that
// behavior.
func DefaultRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

// RetryPolicy configures how a step retries transient handler failures. The
// zero value is not usable; construct one with Attempts >= 1.
//
// Attempts is the maximum total attempts for a step. 1 means no retry (the
// step runs once and any handler error fails the run). Values greater than 1
// allow retries: a step that returns a retryable error is invoked again until
// it succeeds, the attempt limit is reached, or the run context is cancelled.
//
// Delay is the fixed wait applied before each retry attempt. A zero delay
// retries immediately. Validation errors (invalid step input/output) and
// panics are never retried regardless of policy.
//
// Retryable selects which handler errors are retried. When nil,
// DefaultRetryable is used and retries every handler error that is not a
// context cancellation or deadline error.
type RetryPolicy struct {
	Attempts  int
	Delay     time.Duration
	Retryable RetryablePredicate
}

// IsRetryable reports whether err should be retried under p. A nil predicate
// delegates to DefaultRetryable.
func (p RetryPolicy) IsRetryable(err error) bool {
	if p.Retryable != nil {
		return p.Retryable(err)
	}
	return DefaultRetryable(err)
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
//
// RetryOverrides optionally overrides per-step retry policy at run time. The
// map is keyed by StepID and wins over the StepDefinition.Retry configured at
// workflow construction. A present entry with Attempts == 1 disables retry for
// that step on this run; a present entry with Attempts > 1 enables or changes
// retry. Steps absent from the map keep their configured policy (which may be
// no retry when the definition omits Retry).
type WorkflowRunInput struct {
	Input          json.RawMessage
	ThreadID       ThreadID
	Metadata       map[string]string
	RetryOverrides map[StepID]RetryPolicy
}

// WorkflowRunResult is the JSON-centric result of a linear workflow run. Output
// is the validated output of the final step. Suspend is non-nil iff the run
// suspended at a step boundary; in that case Output is empty and Status is
// RunStatusSuspended.
type WorkflowRunResult struct {
	ID       RunID
	Status   RunStatus
	Output   json.RawMessage
	Metadata map[string]string
	Suspend  *SuspendResult
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

// SuspendSignal is returned by a step handler to suspend the run with a typed
// payload so it can later resume from its durable snapshot. A handler returns
// it as the error result wrapped by ErrWorkflowSuspend (see SuspendError).
//
// Contract is an optional JSON Schema-validated payload persisted with the
// snapshot; Resume validates WorkflowResumeInput.Input against it before any
// step runs. Payload is an opaque caller-defined context that is also
// persisted so the resuming process can rebuild any non-step state.
//
// A step whose handler signals suspend must declare SuspendSchema on its
// StepDefinition; otherwise the executor rejects the suspend as an invalid
// step output so a process restart never observes an unvalidated resume
// contract.
type SuspendSignal struct {
	StepID   StepID
	Contract json.RawMessage
	Payload  json.RawMessage
}

// SuspendError carries a SuspendSignal through the error channel so the
// StepHandler signature stays (json.RawMessage, error). It wraps the
// ErrWorkflowSuspend sentinel for errors.Is/As detection.
type SuspendError struct {
	Signal SuspendSignal
}

// Error implements the error interface.
func (e *SuspendError) Error() string {
	if e == nil {
		return "lebro: workflow suspend"
	}
	return "lebro: workflow suspend at step " + string(e.Signal.StepID)
}

// Is supports errors.Is against ErrWorkflowSuspend.
func (e *SuspendError) Is(target error) bool {
	return target == ErrWorkflowSuspend
}

// Unwrap is intentionally absent: ErrWorkflowSuspend is matched via Is so a
// single sentinel can coexist with per-signal data.

// ErrWorkflowSuspend is the sentinel matched by errors.Is to detect a handler
// that returned a SuspendError. Handlers should wrap SuspendSignal with
// SuspendError (or any error whose Is matches ErrWorkflowSuspend) rather than
// returning ErrWorkflowSuspend directly.
var ErrWorkflowSuspend = errors.New("lebro: workflow suspend")

// suspendSignalFromError extracts the SuspendSignal from a SuspendError. It
// returns ok=false when err is not a *SuspendError so the executor can reject
// handlers that match ErrWorkflowSuspend via a custom Is without carrying a
// signal.
func suspendSignalFromError(err error) (SuspendSignal, bool) {
	var suspendErr *SuspendError
	if errors.As(err, &suspendErr) {
		return suspendErr.Signal, true
	}
	return SuspendSignal{}, false
}

// SuspendResult is the non-output portion of a suspended WorkflowRunResult.
// Step is the 1-indexed position of the suspend boundary; StepID is the
// declared step identifier. Contract is the validated resume contract that
// Resume will check WorkflowResumeInput.Input against. Payload is the opaque
// caller-defined context persisted alongside the snapshot.
type SuspendResult struct {
	Step     int
	StepID   StepID
	Contract json.RawMessage
	Payload  json.RawMessage
}

// WorkflowResumeInput is the JSON-centric input for resuming a suspended
// workflow run. RunID identifies the suspended run to resume; Input is
// validated against the SuspendSignal.Contract persisted at the suspend
// boundary before any step runs; Metadata is merged into the run metadata for
// the resumed run.
type WorkflowResumeInput struct {
	RunID    RunID
	Input    json.RawMessage
	Metadata map[string]string
}

// ErrNotSuspended is returned by Resume when the referenced run is not in the
// suspended status (e.g. Running, Succeeded, Failed, Cancelled, or unknown).
var ErrNotSuspended = errors.New("lebro: workflow run is not suspended")

// ErrInvalidResumeInput is returned by Resume when WorkflowResumeInput.Input
// fails validation against the persisted SuspendSignal.Contract. The snapshot
// and run record are not modified on this path so a caller can retry with a
// corrected input.
var ErrInvalidResumeInput = errors.New("lebro: workflow resume input invalid")

// ErrWorkflowResumeRequiresStore is returned by Resume when the workflow has
// no bound Store. Resume is a durable operation: it loads the suspended
// snapshot from storage, so a workflow configured without LinearWorkflowConfig
// .Store cannot resume.
var ErrWorkflowResumeRequiresStore = errors.New("lebro: workflow resume requires a bound store")

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
		if len(step.Definition.InputSchema) > 0 || len(step.Definition.OutputSchema) > 0 || len(step.Definition.SuspendSchema) > 0 {
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
// executor writes on every snapshot. Readers tolerate 0 (legacy/unspecified)
// and 1 (pre-suspend); the suspend release writes 2 and adds the optional
// Suspend field.
const workflowSnapshotSchemaVersion = 2

// workflowSnapshotEnvelope is the JSON state persisted at each successful step
// boundary. It carries the current step's output and the ordered completed
// outputs so a future resumable executor can rebuild state without replaying
// the run record. Suspend is populated only on a suspend boundary so Resume
// can validate the resume input against the persisted contract.
type workflowSnapshotEnvelope struct {
	Step    int               `json:"step"`
	StepID  StepID            `json:"step_id,omitempty"`
	Output  json.RawMessage   `json:"output,omitempty"`
	Outputs []json.RawMessage `json:"outputs,omitempty"`
	Suspend *suspendEnvelope  `json:"suspend,omitempty"`
}

// suspendEnvelope carries the validated resume contract and opaque caller
// payload persisted at a suspend boundary. Resume decodes it to validate
// WorkflowResumeInput.Input before any step runs.
type suspendEnvelope struct {
	Contract json.RawMessage `json:"contract,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
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

	if perr := validateRetryOverrides(input.RetryOverrides); perr != nil {
		overrideErr := perr.(*invalidRetryOverrideError)
		position := w.stepPosition(overrideErr.stepID)
		stepErr := &WorkflowError{Kind: WorkflowErrorStepFailed, Step: position, StepID: overrideErr.stepID, Err: perr}
		emitter.terminal(runID, position, overrideErr.stepID, RunEventFailed, RunStatusFailed, stepErr)
		w.persistTerminalBestEffort(anchor, 0, "", nil, RunStatusFailed, stepErr)
		return w.fail(runID, metadata, stepErr)
	}

	if perr := w.persistRunStart(ctx, anchor); perr != nil {
		stepErr := &WorkflowError{Kind: WorkflowErrorStepFailed, Err: fmt.Errorf("lebro: persist workflow run start: %w", perr)}
		emitter.terminal(runID, 0, "", RunEventFailed, RunStatusFailed, stepErr)
		w.persistTerminalBestEffort(anchor, 0, "", nil, RunStatusFailed, stepErr)
		return w.fail(runID, metadata, stepErr)
	}

	emitter.emit(runID, 0, "", RunEventStarted)

	return w.executeSteps(ctx, anchor, emitter, runID, metadata, current, completedOutputs, 0, input.RetryOverrides, input.ThreadID)
}

// executeSteps runs the workflow steps from startIndex through the last step,
// validating each handoff, emitting lifecycle events, and persisting step
// boundaries. It is shared by Run (startIndex == 0) and Resume (startIndex ==
// number of previously completed steps). current is the input to the first
// step to execute (the resume input for Resume, or WorkflowRunInput.Input for
// Run); completedOutputs is the ordered list of outputs already produced
// before startIndex (empty for Run, preloaded from the snapshot for Resume).
// retryOverrides and threadID are the run-level inputs forwarded to each step.
//
// On suspend the returned WorkflowRunResult has Status RunStatusSuspended and
// Suspend populated. On terminal success the run is persisted as Succeeded.
func (w *LinearWorkflow) executeSteps(ctx context.Context, anchor runAnchor, emitter *runEmitter, runID RunID, metadata map[string]string, current json.RawMessage, completedOutputs []json.RawMessage, startIndex int, retryOverrides map[StepID]RetryPolicy, threadID ThreadID) (WorkflowRunResult, error) {
	for i := startIndex; i < len(w.steps); i++ {
		step := w.steps[i]
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

		stepCtx := withWorkflowInvocation(ctx, runID, position, stepID, threadID, metadata)
		policy := resolveRetryPolicy(step.retry, retryOverrides, stepID)

		output, runErr := w.runStepWithRetry(stepCtx, step, position, stepID, stepStart, current, policy, emitter, runID)
		if runErr != nil {
			if runErr.kind == retryCancelled {
				cause := runErr.cause
				stepErr := &WorkflowError{Kind: WorkflowErrorCancelled, Step: position, StepID: stepID, Err: cause}
				emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
				emitter.terminal(runID, position, stepID, RunEventCancelled, RunStatusCancelled, cause)
				w.persistTerminalBestEffort(anchor, position-1, lastStepID(w.steps, i-1), completedOutputs, RunStatusCancelled, stepErr)
				return w.cancelled(runID, metadata, position, stepID, cause)
			}
			if runErr.kind == retryPanicked {
				stepErr := &WorkflowError{Kind: WorkflowErrorStepPanicked, Step: position, StepID: stepID, Err: runErr.cause}
				emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
				emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
				w.persistTerminalBestEffort(anchor, position-1, lastStepID(w.steps, i-1), completedOutputs, RunStatusFailed, stepErr)
				return w.fail(runID, metadata, stepErr)
			}
			if runErr.kind == retryInvalidOutput {
				stepErr := &WorkflowError{Kind: WorkflowErrorInvalidStepOutput, Step: position, StepID: stepID, Err: runErr.cause}
				emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
				emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
				w.persistTerminalBestEffort(anchor, position-1, lastStepID(w.steps, i-1), completedOutputs, RunStatusFailed, stepErr)
				return w.fail(runID, metadata, stepErr)
			}
			if runErr.kind == retrySuspended {
				suspendErr := runErr.cause.(*SuspendError)
				signal := suspendErr.Signal
				if perr := w.persistSuspend(ctx, anchor, position, stepID, completedOutputs, signal); perr != nil {
					stepErr := &WorkflowError{Kind: WorkflowErrorStepFailed, Step: position, StepID: stepID, Err: fmt.Errorf("lebro: persist workflow suspend at step %q: %w", stepID, perr)}
					emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
					emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
					w.persistTerminalBestEffort(anchor, position-1, lastStepID(w.steps, i-1), completedOutputs, RunStatusFailed, stepErr)
					return w.fail(runID, metadata, stepErr)
				}
				emitter.emitStepFinished(runID, position, stepID, stepStart, nil)
				emitter.emitSuspended(runID, position, stepID)
				return WorkflowRunResult{
					ID:       runID,
					Status:   RunStatusSuspended,
					Metadata: metadata,
					Suspend: &SuspendResult{
						Step:     position,
						StepID:   signal.StepID,
						Contract: cloneRawMessage(signal.Contract),
						Payload:  cloneRawMessage(signal.Payload),
					},
				}, nil
			}
			stepErr := &WorkflowError{Kind: WorkflowErrorStepFailed, Step: position, StepID: stepID, Err: runErr.cause}
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

// Resume continues a previously suspended workflow run from its durable
// snapshot. It loads the run record and latest snapshot from the bound Store,
// validates WorkflowResumeInput.Input against the SuspendSignal.Contract
// persisted at the suspend boundary, and runs the remaining steps without
// re-executing completed ones.
//
// Resume requires a bound Store (LinearWorkflowConfig.Store). A workflow
// configured without a Store cannot resume because the suspended state is not
// durable. The referenced run must be in RunStatusSuspended; resuming a run in
// any other status returns ErrNotSuspended.
//
// Invalid resume input is rejected with ErrInvalidResumeInput before any step
// runs or persistence occurs, so the suspended snapshot and run record are not
// corrupted. Resume metadata is merged into the run's existing metadata for
// the resumed run.
func (w *LinearWorkflow) Resume(ctx context.Context, input WorkflowResumeInput) (WorkflowRunResult, error) {
	if w == nil {
		return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorStepFailed, Err: errors.New("lebro: workflow is nil")}
	}
	if w.store == nil || isNilInterface(w.store) {
		return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorStepFailed, Err: ErrWorkflowResumeRequiresStore}
	}
	if err := ctx.Err(); err != nil {
		return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorCancelled, Err: err}
	}

	run, err := w.store.WorkflowRuns().GetWorkflowRun(ctx, input.RunID)
	if err != nil {
		return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorStepFailed, Step: 0, Err: fmt.Errorf("lebro: load suspended run %q: %w", input.RunID, err)}
	}
	if run.Status != RunStatusSuspended {
		return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorStepFailed, Err: fmt.Errorf("lebro: run %q %w: %s", input.RunID, ErrNotSuspended, run.Status)}
	}

	snapshots, err := w.store.WorkflowSnapshots().ListWorkflowSnapshots(ctx, input.RunID, PageRequest{Limit: 1000})
	if err != nil {
		return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorStepFailed, Err: fmt.Errorf("lebro: list snapshots for run %q: %w", input.RunID, err)}
	}
	if len(snapshots.Records) == 0 {
		return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorStepFailed, Err: fmt.Errorf("lebro: no snapshot for suspended run %q", input.RunID)}
	}
	// Select the suspend snapshot with the highest sequence. Only suspend
	// boundaries populate the envelope's Suspend field, so we decode each
	// candidate and pick the matching one with the largest Sequence.
	var (
		snapshot     WorkflowSnapshotRecord
		envelope     workflowSnapshotEnvelope
		foundSuspend bool
	)
	for _, cand := range snapshots.Records {
		var env workflowSnapshotEnvelope
		if err := json.Unmarshal(cand.State, &env); err != nil {
			continue
		}
		if env.Suspend == nil {
			continue
		}
		if !foundSuspend || cand.Sequence > snapshot.Sequence {
			snapshot = cand
			envelope = env
			foundSuspend = true
		}
	}
	if !foundSuspend {
		return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorStepFailed, Err: fmt.Errorf("lebro: no suspend snapshot for run %q", input.RunID)}
	}

	resumeInput := cloneRawMessage(input.Input)
	if len(envelope.Suspend.Contract) > 0 {
		compiled := w.suspendSchemaForStep(envelope.StepID)
		if compiled == nil {
			return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorStepFailed, Err: fmt.Errorf("lebro: cannot resolve SuspendSchema for step %q to validate resume input", envelope.StepID)}
		}
		if verr := validateStepValue(compiled, ValidationTargetResumeInput, resumeInput); verr != nil {
			return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorStepFailed, Err: fmt.Errorf("lebro: %w: %s", ErrInvalidResumeInput, verr)}
		}
	}

	mergedMetadata := decodeMetadata(run.Metadata)
	for k, v := range input.Metadata {
		if mergedMetadata == nil {
			mergedMetadata = map[string]string{}
		}
		mergedMetadata[k] = v
	}

	anchor := runAnchor{
		runID:     run.ID,
		input:     WorkflowRunInput{Input: cloneRawMessage(run.Input), ThreadID: run.ThreadID, Metadata: mergedMetadata},
		metadata:  mergedMetadata,
		startedAt: run.StartedAt,
	}

	if perr := w.persistResumeStart(ctx, anchor); perr != nil {
		return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorStepFailed, Err: fmt.Errorf("lebro: persist workflow resume start: %w", perr)}
	}

	emitter := newRunEmitter(ctx, w.listener, w.clock, w.idSource)
	resumePosition := envelope.Step + 1
	var resumeStepID StepID
	if resumePosition <= len(w.steps) {
		resumeStepID = w.steps[resumePosition-1].definition.ID
	}
	emitter.emitResumed(run.ID, resumePosition, resumeStepID)

	completedOutputs := cloneRawOutputs(envelope.Outputs)
	return w.executeSteps(ctx, anchor, emitter, run.ID, mergedMetadata, resumeInput, completedOutputs, envelope.Step, nil, run.ThreadID)
}

// suspendSchemaForStep returns the compiled SuspendSchema for the declared
// step, or nil when the step is not declared or has no SuspendSchema. Resume
// uses it to validate WorkflowResumeInput.Input against the same schema that
// validated the persisted SuspendSignal.Contract at the suspend boundary.
func (w *LinearWorkflow) suspendSchemaForStep(stepID StepID) CompiledSchema {
	for _, step := range w.steps {
		if step.definition.ID == stepID {
			return step.suspendSchema
		}
	}
	return nil
}

// persistResumeStart flips the run record back to Running so a process stop
// during resume leaves an inspectable in-progress record. The original
// StartedAt and Input are preserved from the stored run record via the anchor.
func (w *LinearWorkflow) persistResumeStart(ctx context.Context, anchor runAnchor) error {
	if w.store == nil || isNilInterface(w.store) {
		return nil
	}
	now := w.clock.Now()
	record := w.baseRunRecord(anchor)
	record.Status = RunStatusRunning
	record.CurrentStep = 0
	record.CurrentStepID = ""
	record.UpdatedAt = now
	return w.store.WorkflowRuns().SaveWorkflowRun(ctx, record)
}

// decodeMetadata unmarshals a stored run's metadata JSON into a string map.
// A nil or empty payload returns nil.
func decodeMetadata(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
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

// persistSuspend writes a suspend snapshot and updates the run record to
// Suspended inside a single Store.Transaction so the suspend boundary is
// atomic. The snapshot carries the validated resume contract and opaque
// payload so Resume can validate WorkflowResumeInput.Input without re-running
// the suspending step. The run record's CurrentStep/CurrentStepID identify
// the suspending step; FinishedAt stays nil because the run is resumable.
// When no Store is bound the suspend is in-memory only and the caller still
// receives a suspended WorkflowRunResult.
func (w *LinearWorkflow) persistSuspend(ctx context.Context, anchor runAnchor, position int, stepID StepID, completed []json.RawMessage, signal SuspendSignal) error {
	if w.store == nil || isNilInterface(w.store) {
		return nil
	}
	envelope := workflowSnapshotEnvelope{
		Step:    position,
		StepID:  stepID,
		Output:  nil,
		Outputs: cloneRawOutputs(completed),
		Suspend: &suspendEnvelope{
			Contract: cloneRawMessage(signal.Contract),
			Payload:  cloneRawMessage(signal.Payload),
		},
	}
	state, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("lebro: encode workflow suspend snapshot: %w", err)
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
	updated.Status = RunStatusSuspended
	updated.CurrentStep = position
	updated.CurrentStepID = stepID
	updated.StepOutputs = cloneRawOutputs(completed)
	updated.UpdatedAt = now
	return w.store.Transaction(ctx, func(ctx context.Context, repos Repositories) error {
		if err := repos.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, snapshot); err != nil {
			return err
		}
		return repos.WorkflowRuns().SaveWorkflowRun(ctx, updated)
	})
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
	definition    StepDefinition
	handler       StepHandler
	inputSchema   CompiledSchema
	outputSchema  CompiledSchema
	suspendSchema CompiledSchema
	retry         *RetryPolicy
}

func newCompiledStep(compiler SchemaCompiler, step Step) (compiledStep, error) {
	cs := compiledStep{
		definition: cloneStepDefinition(step.Definition),
		handler:    step.Handler,
		retry:      cloneRetryPolicy(step.Definition.Retry),
	}
	if err := validateRetryPolicy(cs.retry); err != nil {
		return cs, err
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
	if len(step.Definition.SuspendSchema) > 0 {
		if !json.Valid(step.Definition.SuspendSchema) {
			return cs, errors.New("suspend schema must be valid JSON")
		}
		compiled, err := compiler.Compile(step.Definition.SuspendSchema)
		if err != nil {
			return cs, fmt.Errorf("compile suspend schema: %w", err)
		}
		if compiled == nil || isNilInterface(compiled) {
			return cs, errors.New("schema compiler returned a nil suspend schema")
		}
		cs.suspendSchema = compiled
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
	def.SuspendSchema = cloneRawMessage(def.SuspendSchema)
	def.Retry = cloneRetryPolicy(def.Retry)
	return def
}

// cloneRetryPolicy returns a deep copy of p. When p is nil the result is nil
// so a missing policy stays missing after a clone.
func cloneRetryPolicy(p *RetryPolicy) *RetryPolicy {
	if p == nil {
		return nil
	}
	copy := *p
	return &copy
}

// validateRetryPolicy returns an error when p is configured with an invalid
// attempt count. A nil policy means "no retry" and is always valid.
func validateRetryPolicy(p *RetryPolicy) error {
	if p == nil {
		return nil
	}
	if p.Attempts < 1 {
		return errors.New("retry policy attempts must be >= 1")
	}
	if p.Delay < 0 {
		return errors.New("retry policy delay must be >= 0")
	}
	return nil
}

// validateRetryOverrides checks every per-run retry override up front so an
// invalid run configuration fails before any step runs or persistence occurs.
// Unknown step IDs are tolerated: an override for a step that is not in the
// workflow is simply never selected by resolveRetryPolicy, so it cannot
// affect behavior and is ignored.
//
// When more than one override is invalid, the error reports the
// lexicographically smallest StepID so the failure is deterministic across
// runs and debuggable. The returned error is an *invalidRetryOverrideError so
// the caller can recover the StepID and resolve its 1-indexed position in the
// workflow.
func validateRetryOverrides(overrides map[StepID]RetryPolicy) error {
	if len(overrides) == 0 {
		return nil
	}
	keys := make([]StepID, 0, len(overrides))
	for stepID := range overrides {
		keys = append(keys, stepID)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, stepID := range keys {
		policy := overrides[stepID]
		if err := validateRetryPolicy(&policy); err != nil {
			return &invalidRetryOverrideError{stepID: stepID, cause: err}
		}
	}
	return nil
}

// invalidRetryOverrideError carries the StepID of the first invalid override
// (in sorted order) so the workflow executor can preserve step identity on
// the resulting WorkflowError and persisted WorkflowFailureData.
type invalidRetryOverrideError struct {
	stepID StepID
	cause  error
}

func (e *invalidRetryOverrideError) Error() string {
	if e == nil {
		return "lebro: invalid retry override"
	}
	return fmt.Sprintf("lebro: invalid retry override for step %q: %s", e.stepID, e.cause)
}

func (e *invalidRetryOverrideError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// stepPosition returns the 1-indexed position of stepID in the workflow, or
// 0 when the step is not declared. Used to attach step identity to failures
// raised before the step loop runs (e.g. an invalid retry override for a
// declared step).
func (w *LinearWorkflow) stepPosition(stepID StepID) int {
	for i, step := range w.steps {
		if step.definition.ID == stepID {
			return i + 1
		}
	}
	return 0
}

// retryOutcomeKind classifies the terminal outcome of runStepWithRetry so the
// caller can map it back to the appropriate WorkflowErrorKind without re-
// inspecting the wrapped error.
type retryOutcomeKind int

const (
	retryFailed retryOutcomeKind = iota
	retryCancelled
	retryPanicked
	retryInvalidOutput
	retrySuspended
)

// retryOutcome bundles the terminal result of runStepWithRetry. On success
// output holds the validated handler output and cause is nil. On failure cause
// holds the relevant error (handler error, context error, *StepPanicError, or
// *ValidationError) and kind classifies it.
type retryOutcome struct {
	kind  retryOutcomeKind
	cause error
}

// resolveRetryPolicy returns the effective retry policy for stepID, with the
// run-time override winning over the compiled step policy. A present override
// entry replaces the step policy entirely; an override with Attempts == 1
// disables retry for the run.
func resolveRetryPolicy(compiled *RetryPolicy, overrides map[StepID]RetryPolicy, stepID StepID) *RetryPolicy {
	if overrides == nil {
		return compiled
	}
	if override, ok := overrides[stepID]; ok {
		return &override
	}
	return compiled
}

// runStepWithRetry invokes the step handler with the configured retry policy.
// The first attempt uses the step's outer step_started event; retry attempts
// (attempt >= 2) emit step_attempt_started/finished events. Validation of the
// handler output happens inside this function so a validation failure on any
// attempt is treated as non-retryable: validation errors are deterministic and
// retrying would not change the result.
//
// The function never returns a *WorkflowError; the caller maps retryOutcome
// to the appropriate workflow error kind. This keeps the retry loop's error
// surface narrow and testable.
func (w *LinearWorkflow) runStepWithRetry(ctx context.Context, step compiledStep, position int, stepID StepID, stepStart time.Time, input json.RawMessage, policy *RetryPolicy, emitter *runEmitter, runID RunID) (json.RawMessage, *retryOutcome) {
	maxAttempts := 1
	if policy != nil && policy.Attempts > maxAttempts {
		maxAttempts = policy.Attempts
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, &retryOutcome{kind: retryCancelled, cause: err}
		}

		var attemptStart time.Time
		var delay time.Duration
		if attempt == 1 {
			attemptStart = stepStart
		} else {
			if policy != nil {
				delay = policy.Delay
			}
			if delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return nil, &retryOutcome{kind: retryCancelled, cause: ctx.Err()}
				}
			}
			attemptStart = emitter.emitStepAttemptStarted(runID, position, stepID, attempt, delay)
		}

		output, panicValue, panicked, handlerErr := invokeStepHandler(ctx, step.handler, cloneRawMessage(input))

		if err := ctx.Err(); err != nil {
			cause := preferContextError(err, err)
			if attempt != 1 {
				emitter.emitStepAttemptFinished(runID, position, stepID, attempt, delay, attemptStart, cause)
			}
			return nil, &retryOutcome{kind: retryCancelled, cause: cause}
		}

		if panicked {
			panicErr := &StepPanicError{Value: panicValue}
			if attempt != 1 {
				emitter.emitStepAttemptFinished(runID, position, stepID, attempt, delay, attemptStart, panicErr)
			}
			return nil, &retryOutcome{kind: retryPanicked, cause: panicErr}
		}

		if handlerErr != nil {
			// Suspend is terminal and non-retryable. The handler signalled
			// suspend via a SuspendError (or any error matching
			// ErrWorkflowSuspend). Validate the resume contract against the
			// step's SuspendSchema and surface a retrySuspended outcome so the
			// caller persists the suspend boundary and stops the run.
			if errors.Is(handlerErr, ErrWorkflowSuspend) {
				signal, ok := suspendSignalFromError(handlerErr)
				if !ok {
					return nil, &retryOutcome{kind: retryFailed, cause: fmt.Errorf("lebro: handler returned an error matching ErrWorkflowSuspend but it carried no SuspendSignal: %w", handlerErr)}
				}
				if step.suspendSchema == nil {
					return nil, &retryOutcome{kind: retryInvalidOutput, cause: fmt.Errorf("lebro: step %q signalled suspend but declares no SuspendSchema", stepID)}
				}
				if err := validateStepValue(step.suspendSchema, ValidationTargetSuspendContract, signal.Contract); err != nil {
					if attempt != 1 {
						emitter.emitStepAttemptFinished(runID, position, stepID, attempt, delay, attemptStart, err)
					}
					return nil, &retryOutcome{kind: retryInvalidOutput, cause: err}
				}
				if signal.StepID == "" {
					signal.StepID = stepID
				}
				if attempt != 1 {
					emitter.emitStepAttemptFinished(runID, position, stepID, attempt, delay, attemptStart, nil)
				}
				return nil, &retryOutcome{kind: retrySuspended, cause: &SuspendError{Signal: signal}}
			}
			if errors.Is(handlerErr, context.Canceled) || errors.Is(handlerErr, context.DeadlineExceeded) {
				if attempt != 1 {
					emitter.emitStepAttemptFinished(runID, position, stepID, attempt, delay, attemptStart, handlerErr)
				}
				return nil, &retryOutcome{kind: retryCancelled, cause: handlerErr}
			}
			isLast := attempt == maxAttempts
			retryable := policy != nil && policy.IsRetryable(handlerErr)
			if isLast || !retryable {
				if attempt != 1 {
					emitter.emitStepAttemptFinished(runID, position, stepID, attempt, delay, attemptStart, handlerErr)
				}
				return nil, &retryOutcome{kind: retryFailed, cause: handlerErr}
			}
			if attempt != 1 {
				emitter.emitStepAttemptFinished(runID, position, stepID, attempt, delay, attemptStart, handlerErr)
			}
			continue
		}

		if err := validateStepValue(step.outputSchema, ValidationTargetStepOutput, output); err != nil {
			if attempt != 1 {
				emitter.emitStepAttemptFinished(runID, position, stepID, attempt, delay, attemptStart, err)
			}
			return nil, &retryOutcome{kind: retryInvalidOutput, cause: err}
		}

		if attempt != 1 {
			emitter.emitStepAttemptFinished(runID, position, stepID, attempt, delay, attemptStart, nil)
		}
		return output, nil
	}

	return nil, &retryOutcome{kind: retryFailed, cause: errors.New("lebro: step retry loop exited without result")}
}
