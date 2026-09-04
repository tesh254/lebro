package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
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
//
// Branches makes a step a "branching step": when reached, the executor
// evaluates each branch's Condition in declared order, selects the first
// branch whose predicate returns true, and runs that branch's Steps in order.
// A branching step MUST NOT carry a Handler, OutputSchema, SuspendSchema, or
// Retry; those fields are rejected at build time because a branching step
// produces no output of its own. InputSchema on a branching step validates the
// value passed to every branch predicate. Default names the branch selected
// when no Condition returns true; an empty Default with no match fails the run
// with WorkflowErrorNoBranchMatched.
type StepDefinition struct {
	ID            StepID
	Name          string
	Description   string
	InputSchema   json.RawMessage
	OutputSchema  json.RawMessage
	SuspendSchema json.RawMessage
	Retry         *RetryPolicy
	Branches      []Branch
	Default       string
	FanOut        *FanOut
	Map           *Map
	ForEach       *ForEach
	DoWhile       *DoWhile
	DoUntil       *DoUntil
	Sleep         *Sleep
	SleepUntil    *SleepUntil
}

// Sleep pauses a durable workflow for Duration. The input value is passed to
// the following step unchanged once the scheduler wakes the run.
type Sleep struct {
	Duration time.Duration
}

// SleepUntil pauses a durable workflow until At. At is normalized to UTC when
// the wait is persisted; a time in the past resumes on the scheduler's next
// tick.
type SleepUntil struct {
	At time.Time
}

// Map is a declarative transform step. Fields maps output property names to
// dotted paths in the current JSON value; "." selects the whole value. Map
// never invokes user code, making its decisions safe to replay from a saved
// workflow definition.
type Map struct {
	// Path selects one value from the current input. It is mutually exclusive
	// with Fields and is useful when the next step accepts an array or scalar.
	Path   string
	Fields map[string]string
}

// ForEach runs Steps for every element of the current JSON array. Results are
// returned as an array in input order, even when MaxParallel is greater than
// one. A value of zero or one runs sequentially.
type ForEach struct {
	Steps       []Step
	MaxParallel int
}

// LoopPredicate evaluates the output from a completed loop iteration. It must
// be pure and deterministic: persisted checkpoints retain the output and
// iteration number, but Go functions themselves cannot be persisted.
type LoopPredicate func(ctx context.Context, output json.RawMessage, iteration int) (bool, error)

// DoWhile executes Steps at least once, then continues while Condition returns
// true. MaxIterations is required and bounds total body executions.
type DoWhile struct {
	Steps         []Step
	MaxIterations int
	Condition     LoopPredicate
}

// DoUntil executes Steps at least once, then continues until Condition returns
// true. MaxIterations is required and bounds total body executions.
type DoUntil struct {
	Steps         []Step
	MaxIterations int
	Condition     LoopPredicate
}

// loopCheckpoint records a persisted loop iteration. It remains internal until
// loop recovery is part of the public run-result contract.
type loopCheckpoint struct {
	StepID     StepID          `json:"step_id"`
	Iterations int             `json:"iterations"`
	Output     json.RawMessage `json:"output,omitempty"`
}

// BranchPredicate evaluates whether a named branch should be selected at a
// branching step. It receives the validated input to the branching step (the
// previous step's output or the run input for the first step) as raw JSON and
// returns true when the branch should be taken. A returned error fails the run
// with WorkflowErrorBranchConditionFailed. Predicates must be pure and
// deterministic; they must not perform I/O or mutate external state so the
// selected path depends only on the step input.
type BranchPredicate func(ctx context.Context, input json.RawMessage) (bool, error)

// Branch describes one named path at a branching step. Name identifies the
// branch for Default selection and event reporting; it must be non-empty and
// unique among sibling branches. Condition is evaluated in declared order; the
// first branch whose Condition returns true is selected. Steps is the ordered
// list of steps executed when this branch is selected; each step follows the
// same rules as a top-level step (unique ID across the whole workflow, non-nil
// handler, valid schemas). A branch must contain at least one step.
type Branch struct {
	Name      string
	Condition BranchPredicate
	Steps     []Step
}

// FanOutFailurePolicy controls how a fan-out step reacts when a child branch
// fails. FailFast cancels siblings after the first failure; CollectAll lets
// every branch finish before returning the lowest declared-index failure.
type FanOutFailurePolicy string

const (
	// FanOutFailFast cancels remaining and in-flight sibling branches after
	// the first child failure and waits for them to drain before returning.
	FanOutFailFast FanOutFailurePolicy = "fail_fast"
	// FanOutCollectAll lets every branch run to completion regardless of
	// sibling failures, then returns the lowest declared-index failure.
	FanOutCollectAll FanOutFailurePolicy = "collect_all"
)

// FanOutInputMapper derives a branch input from the fan-out step input. A nil
// mapper passes a private copy of the fan-out input to the branch unchanged.
// The mapper must be pure and deterministic; it must not perform I/O.
type FanOutInputMapper func(ctx context.Context, input json.RawMessage) (json.RawMessage, error)

// FanOutBranch declares one independently executed path in a fan-out step.
// Name must be non-empty and unique among sibling fan-out branches. Steps run
// in declared order after InputMapper produces the branch input; each step
// follows the same rules as a top-level step. A nil InputMapper passes a
// clone of the fan-out input unchanged.
type FanOutBranch struct {
	Name        string
	InputMapper FanOutInputMapper
	Steps       []Step
}

// FanOut configures bounded concurrent execution and deterministic joining.
// Branches lists the independent paths; MaxParallel bounds active branches
// (a value of 0 or 1 means sequential). FailurePolicy controls sibling
// cancellation on failure; a zero value means FailFast.
type FanOut struct {
	Branches      []FanOutBranch
	MaxParallel   int
	FailurePolicy FanOutFailurePolicy
}

// FanOutBranchResult records one child branch's terminal state. Output is set
// only for successful branches; Failure mirrors a WorkflowError in portable
// form so the result can be persisted with a WorkflowRunRecord.
type FanOutBranchResult struct {
	Name    string               `json:"name"`
	Status  RunStatus            `json:"status"`
	Output  json.RawMessage      `json:"output,omitempty"`
	Failure *WorkflowFailureData `json:"failure,omitempty"`
}

// FanOutJoinResult records a fan-out barrier and every child branch result in
// declaration order. Status is Succeeded when every branch succeeded, Failed
// when one or more branches failed, and Cancelled when the run context
// cancelled execution. It is carried on WorkflowRunResult.FanOut and the
// persisted snapshot so the join outcome is inspectable and durable.
type FanOutJoinResult struct {
	StepID   StepID               `json:"step_id"`
	Status   RunStatus            `json:"status"`
	Branches []FanOutBranchResult `json:"branches"`
}

// ForEachItemResult records durable completion state for one foreach item.
// Index is the original input position, so result ordering never depends on
// worker scheduling.
type ForEachItemResult struct {
	Index   int                  `json:"index"`
	Status  RunStatus            `json:"status"`
	Output  json.RawMessage      `json:"output,omitempty"`
	Failure *WorkflowFailureData `json:"failure,omitempty"`
}

// ForEachResult records a completed foreach barrier and its item outcomes in
// input order. It is persisted with workflow snapshots.
type ForEachResult struct {
	StepID StepID              `json:"step_id"`
	Status RunStatus           `json:"status"`
	Items  []ForEachItemResult `json:"items"`
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
	// WorkflowErrorNoBranchMatched means a branching step evaluated every
	// branch predicate and none returned true, and no Default branch was
	// configured. The run stops at the branching step.
	WorkflowErrorNoBranchMatched WorkflowErrorKind = "no_branch_matched"
	// WorkflowErrorBranchConditionFailed means a branch predicate returned
	// an error while being evaluated. The wrapped error is the predicate
	// error; the run stops at the branching step.
	WorkflowErrorBranchConditionFailed WorkflowErrorKind = "branch_condition_failed"
	// WorkflowErrorInvalidBranchInput means the input to a branching step
	// failed its InputSchema validation before any predicate ran.
	WorkflowErrorInvalidBranchInput WorkflowErrorKind = "invalid_branch_input"
	// WorkflowErrorFanOutBranchFailed means a child branch in a fan-out step
	// returned a terminal failure. The wrapped error is the branch's
	// WorkflowError; StepID identifies the failing child step.
	WorkflowErrorFanOutBranchFailed WorkflowErrorKind = "fanout_branch_failed"
	// WorkflowErrorFanOutInputMapperFailed means a fan-out branch's
	// InputMapper returned an error. The wrapped error is the mapper error.
	WorkflowErrorFanOutInputMapperFailed WorkflowErrorKind = "fanout_input_mapper_failed"
	// WorkflowErrorInvalidFanOutInput means the input to a fan-out step
	// failed its InputSchema validation before any branch ran.
	WorkflowErrorInvalidFanOutInput WorkflowErrorKind = "invalid_fanout_input"
	// WorkflowErrorUnauthorized means a configured Policy denied the run at
	// start. The wrapped error is a *PolicyDenial.
	WorkflowErrorUnauthorized WorkflowErrorKind = "unauthorized"
	// WorkflowErrorMaxIterations means a guarded loop reached its declared
	// bound while its continuation predicate still requested another iteration.
	WorkflowErrorMaxIterations WorkflowErrorKind = "max_iterations"
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
	// ErrWorkflowNoBranchMatched matches branching steps where no predicate
	// returned true and no Default branch was configured.
	ErrWorkflowNoBranchMatched = errors.New("lebro: workflow no branch matched")
	// ErrWorkflowBranchConditionFailed matches branching steps where a
	// predicate returned an error during evaluation.
	ErrWorkflowBranchConditionFailed = errors.New("lebro: workflow branch condition failed")
	// ErrWorkflowInvalidBranchInput matches branching steps whose input
	// failed the InputSchema before any predicate ran.
	ErrWorkflowInvalidBranchInput = errors.New("lebro: workflow invalid branch input")
	// ErrWorkflowFanOutBranchFailed matches fan-out steps where a child
	// branch returned a terminal failure.
	ErrWorkflowFanOutBranchFailed = errors.New("lebro: workflow fan-out branch failed")
	// ErrWorkflowFanOutInputMapperFailed matches fan-out steps where a
	// branch InputMapper returned an error.
	ErrWorkflowFanOutInputMapperFailed = errors.New("lebro: workflow fan-out input mapper failed")
	// ErrWorkflowInvalidFanOutInput matches fan-out steps whose input
	// failed the InputSchema before any branch ran.
	ErrWorkflowInvalidFanOutInput = errors.New("lebro: workflow invalid fan-out input")
	// ErrWorkflowUnauthorized matches runs denied by a configured Policy at
	// start. The wrapped *PolicyDenial also matches ErrPolicyDenied.
	ErrWorkflowUnauthorized = errors.New("lebro: workflow unauthorized")
	// ErrWorkflowMaxIterations matches guarded loops that reached their
	// required maximum before their predicate allowed completion.
	ErrWorkflowMaxIterations = errors.New("lebro: workflow max iterations reached")
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
	case WorkflowErrorNoBranchMatched:
		return ErrWorkflowNoBranchMatched
	case WorkflowErrorBranchConditionFailed:
		return ErrWorkflowBranchConditionFailed
	case WorkflowErrorInvalidBranchInput:
		return ErrWorkflowInvalidBranchInput
	case WorkflowErrorFanOutBranchFailed:
		return ErrWorkflowFanOutBranchFailed
	case WorkflowErrorFanOutInputMapperFailed:
		return ErrWorkflowFanOutInputMapperFailed
	case WorkflowErrorInvalidFanOutInput:
		return ErrWorkflowInvalidFanOutInput
	case WorkflowErrorUnauthorized:
		return ErrWorkflowUnauthorized
	case WorkflowErrorMaxIterations:
		return ErrWorkflowMaxIterations
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
	Input    json.RawMessage
	ThreadID ThreadID
	// RunID optionally supplies a durable run identity created by the caller.
	// It must be a valid opaque runtime ID and must not already name a stored
	// workflow run.
	RunID          RunID
	Scope          RuntimeScope
	Metadata       map[string]string
	RetryOverrides map[StepID]RetryPolicy
}

// WorkflowRunResult is the JSON-centric result of a linear workflow run. Output
// is the validated output of the final step. Suspend is non-nil iff the run
// suspended at a step boundary; in that case Output is empty and Status is
// RunStatusSuspended. Path is the ordered list of StepIDs of the first steps
// of the branches selected at each branching step; it is empty when the
// workflow has no branching steps or none were reached.
type WorkflowRunResult struct {
	ID       RunID
	Status   RunStatus
	Output   json.RawMessage
	Metadata map[string]string
	Suspend  *SuspendResult
	Path     []StepID
	FanOut   []FanOutJoinResult
	ForEach  []ForEachResult
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
	if errors.As(err, &suspendErr) && suspendErr != nil {
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

// ErrWorkflowSleepRequiresScheduler is returned by Resume for a durable sleep
// boundary. Sleep steps resume only through a Scheduler so their wake token
// remains fenced and retries stay idempotent.
var ErrWorkflowSleepRequiresScheduler = errors.New("lebro: workflow sleep resumes via scheduler")

// errWorkflowWakeupStale is internal: a wakeup from an earlier sleep found a
// terminal run or a newer suspend boundary. Scheduler records it as a harmless
// completed one-shot execution.
var errWorkflowWakeupStale = errors.New("lebro: workflow wakeup is stale")

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
	// RuntimeStore optionally binds durable workflow state to an
	// application-owned capability-based adapter. Exactly one of Store and
	// RuntimeStore may be set. Durable workflows accept sequential fallback
	// when Transactions is absent; a failed coupled write can therefore leave
	// earlier writes in place (see TransactionalStore).
	RuntimeStore RuntimeStore
	// Policy optionally authorizes the workflow run at start against the caller
	// Identity carried on the run context (see WithIdentity). A denied run fails
	// with a WorkflowErrorUnauthorized wrapping a *PolicyDenial, recorded in the
	// run record and events. Agent and tool steps additionally enforce their own
	// policy because the same context identity flows into every nested run. When
	// nil, no authorization is applied and workflow behavior is unchanged.
	Policy Policy
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
	storeCaps  StoreCapabilities
	policy     Policy
	claimsMu   sync.Mutex
	activeIDs  map[RunID]struct{}
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
	if err := validateStepTree(config.Steps, seen, &hasSchema); err != nil {
		return nil, err
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

	store := config.Store
	var storeCaps StoreCapabilities
	if config.RuntimeStore != nil && !isNilInterface(config.RuntimeStore) {
		if store != nil && !isNilInterface(store) {
			return nil, errors.New("lebro: workflow store and runtime store are mutually exclusive")
		}
		bridged, err := bridgeRuntimeStore(config.RuntimeStore)
		if err != nil {
			return nil, err
		}
		store = bridged
		storeCaps = config.RuntimeStore.Capabilities()
		if err := requireCapability(storeCaps, StoreCapabilityWorkflowState, "durable workflow persistence"); err != nil {
			return nil, err
		}
	} else {
		var err error
		storeCaps, err = storeCapabilitiesOf(store)
		if err != nil {
			return nil, err
		}
	}

	clock := config.Clock
	if clock == nil || isNilInterface(clock) {
		clock = defaultClock{}
	}
	idSource := config.IDSource
	if idSource == nil || isNilInterface(idSource) {
		idSource = NewUUIDIDSource()
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
		store:      store,
		storeCaps:  storeCaps,
		policy:     config.Policy,
		activeIDs:  make(map[RunID]struct{}),
	}, nil
}

// Definition returns the workflow's stable definition.
func (w *LinearWorkflow) Definition() WorkflowDefinition {
	if w == nil {
		return WorkflowDefinition{}
	}
	return w.definition
}

// InputSchema returns a copy of the first step's input schema. It defines the
// JSON value accepted by Run. A nil result means the workflow accepts any input.
func (w *LinearWorkflow) InputSchema() json.RawMessage {
	if w == nil || len(w.steps) == 0 {
		return nil
	}
	return cloneRawMessage(w.steps[0].definition.InputSchema)
}

// ValidateInput validates a value against the first step's input schema without
// creating a run or emitting workflow events.
func (w *LinearWorkflow) ValidateInput(input json.RawMessage) error {
	if w == nil || len(w.steps) == 0 {
		return errors.New("lebro: workflow is nil")
	}
	return validateStepValue(w.steps[0].inputSchema, ValidationTargetStepInput, input)
}

// workflowSnapshotSchemaVersion is the envelope version the linear workflow
// executor writes on every snapshot. Readers tolerate 0 (legacy/unspecified),
// 1 (pre-suspend), 2 (pre-branch), 3 (pre-fan-out), and 4 (pre-foreach).
// Version 5 adds durable per-item foreach state. Version 6 adds loop
// checkpoints. Version 7 adds durable sleep wakeup state.
const workflowSnapshotSchemaVersion = 7

const maxLoopIterations = int(^uint32(0) - 1)

func workflowSnapshotSequence(position, iteration int) int64 {
	return int64(position)<<32 | int64(uint32(iteration))
}

func workflowTerminalSnapshotSequence(position int) int64 {
	return int64(position)<<32 | int64(^uint32(0))
}

// workflowSnapshotEnvelope is the JSON state persisted at each successful step
// boundary. It carries the current step's output and the ordered completed
// outputs so a future resumable executor can rebuild state without replaying
// the run record. Suspend is populated only on a suspend boundary so Resume
// can validate the resume input against the persisted contract. Path is the
// cumulative list of StepIDs of the first steps of branches selected so far;
// it is empty on pre-branch snapshots and grows as branching steps resolve.
type workflowSnapshotEnvelope struct {
	Step    int                `json:"step"`
	StepID  StepID             `json:"step_id,omitempty"`
	Output  json.RawMessage    `json:"output,omitempty"`
	Outputs []json.RawMessage  `json:"outputs,omitempty"`
	Suspend *suspendEnvelope   `json:"suspend,omitempty"`
	Path    []StepID           `json:"path,omitempty"`
	FanOut  []FanOutJoinResult `json:"fan_out,omitempty"`
	ForEach []ForEachResult    `json:"for_each,omitempty"`
	Loops   []loopCheckpoint   `json:"loops,omitempty"`
}

// suspendEnvelope carries the validated resume contract and opaque caller
// payload persisted at a suspend boundary. Resume decodes it to validate
// WorkflowResumeInput.Input before any step runs.
type suspendEnvelope struct {
	Contract json.RawMessage `json:"contract,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	Wake     *wakeEnvelope   `json:"wake,omitempty"`
}

// wakeEnvelope fences a scheduled resume to one exact suspend snapshot.
// Retrying an old wakeup after a later suspend therefore cannot advance a run.
type wakeEnvelope struct {
	At    time.Time `json:"at"`
	Token string    `json:"token"`
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
// Branching steps evaluate their branch predicates in declared order, select
// the first matching branch, emit a branch_selected event, and execute that
// branch's steps. The selected branch's entry StepID is appended to the run
// Path so the route through branching steps is inspectable and resumable.
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
	if err := validateSuppliedRunID(input.RunID); err != nil {
		return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorStepFailed, Err: err}
	}
	runID := w.runID(input.RunID)
	if input.RunID != "" {
		if !w.claimRunID(runID) {
			return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorStepFailed, Err: ErrRunIDAlreadyExists}
		}
		defer w.releaseRunID(runID)
	}
	if input.RunID != "" && w.store != nil && !isNilInterface(w.store) {
		if err := ctx.Err(); err != nil {
			return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorCancelled, Err: err}
		}
		if _, err := w.store.WorkflowRuns().GetWorkflowRun(ctx, runID); err == nil {
			return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorStepFailed, Err: ErrRunIDAlreadyExists}
		} else if !errors.Is(err, ErrNotFound) {
			return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorStepFailed, Err: fmt.Errorf("lebro: check supplied workflow run ID: %w", err)}
		}
	}
	emitter := newRunEmitter(ctx, w.listener, w.clock, w.idSource)
	if err := ctx.Err(); err != nil {
		metadata := cloneMetadata(input.Metadata)
		anchor := w.newAnchor(runID, input, metadata)
		emitter.terminal(runID, 0, "", RunEventCancelled, RunStatusCancelled, err)
		w.persistTerminalBestEffort(anchor, nil, 0, "", nil, nil, RunStatusCancelled, &WorkflowError{Kind: WorkflowErrorCancelled, Err: err})
		return w.cancelled(runID, metadata, nil, nil, 0, "", err)
	}

	metadata := cloneMetadata(input.Metadata)
	current := cloneRawMessage(input.Input)
	completedOutputs := make([]json.RawMessage, 0, len(w.steps))
	anchor := w.newAnchor(runID, input, metadata)
	if derr := authorize(ctx, w.policy, ActionWorkflowRun, Resource{Kind: ResourceKindWorkflow, ID: string(w.definition.ID)}); derr != nil {
		authErr := &WorkflowError{Kind: WorkflowErrorUnauthorized, Err: derr}
		emitter.terminal(runID, 0, "", RunEventFailed, RunStatusFailed, authErr)
		w.persistTerminalBestEffort(anchor, nil, 0, "", nil, nil, RunStatusFailed, authErr)
		return w.fail(runID, metadata, nil, nil, authErr)
	}

	if perr := validateRetryOverrides(input.RetryOverrides); perr != nil {
		overrideErr := perr.(*invalidRetryOverrideError)
		position := w.stepPosition(overrideErr.stepID)
		stepErr := &WorkflowError{Kind: WorkflowErrorStepFailed, Step: position, StepID: overrideErr.stepID, Err: perr}
		emitter.terminal(runID, position, overrideErr.stepID, RunEventFailed, RunStatusFailed, stepErr)
		w.persistTerminalBestEffort(anchor, nil, 0, "", nil, nil, RunStatusFailed, stepErr)
		return w.fail(runID, metadata, nil, nil, stepErr)
	}

	if perr := w.persistRunStart(ctx, anchor); perr != nil {
		stepErr := &WorkflowError{Kind: WorkflowErrorStepFailed, Err: fmt.Errorf("lebro: persist workflow run start: %w", perr)}
		emitter.terminal(runID, 0, "", RunEventFailed, RunStatusFailed, stepErr)
		w.persistTerminalBestEffort(anchor, nil, 0, "", nil, nil, RunStatusFailed, stepErr)
		return w.fail(runID, metadata, nil, nil, stepErr)
	}

	emitter.emit(runID, 0, "", RunEventStarted)

	frames := []stepFrame{{steps: w.steps, index: 0}}
	return w.executeFrames(ctx, anchor, emitter, runID, metadata, current, completedOutputs, frames, nil, nil, nil, 0, input.RetryOverrides, input.ThreadID)
}

func (w *LinearWorkflow) runID(supplied RunID) RunID {
	if supplied != "" {
		return supplied
	}
	return w.idSource.NewRunID()
}

// claimRunID prevents two concurrent calls on this workflow instance from
// executing the same caller-supplied identity. Durable adapters remain
// responsible for cross-process uniqueness when a control plane permits
// multiple workers to start the same externally supplied run.
func (w *LinearWorkflow) claimRunID(id RunID) bool {
	w.claimsMu.Lock()
	defer w.claimsMu.Unlock()
	if _, exists := w.activeIDs[id]; exists {
		return false
	}
	w.activeIDs[id] = struct{}{}
	return true
}

func (w *LinearWorkflow) releaseRunID(id RunID) {
	w.claimsMu.Lock()
	delete(w.activeIDs, id)
	w.claimsMu.Unlock()
}

// stepFrame is one level of the execution stack. The top frame holds the
// step list being walked; when a branching step selects a branch, the branch's
// steps are pushed as a new frame. When a frame is exhausted, it is popped and
// the parent frame advances past the branching step.
type stepFrame struct {
	steps []compiledStep
	index int
}

// executeFrames walks the frame stack, executing each step in order. It is
// shared by Run (frames hold the top-level steps, position starts at 0) and
// Resume (frames are reconstructed from the persisted path, position starts at
// the suspend step's position). current is the input to the next step;
// completedOutputs is the ordered list of outputs already produced; path is the
// cumulative list of selected branch entry StepIDs; position is a global
// 1-indexed counter incremented before each step (branching or regular).
//
// Branching steps evaluate predicates, emit branch_selected, append the
// selected branch's entry StepID to path, and push the branch's steps as a
// new frame. Regular steps validate input, run the handler with retry, validate
// output, persist a snapshot, and advance the frame index. When all frames are
// exhausted the run succeeds.
//
// On suspend the returned WorkflowRunResult has Status RunStatusSuspended and
// Suspend populated. On terminal success the run is persisted as Succeeded.
func (w *LinearWorkflow) executeFrames(ctx context.Context, anchor runAnchor, emitter *runEmitter, runID RunID, metadata map[string]string, current json.RawMessage, completedOutputs []json.RawMessage, frames []stepFrame, path []StepID, fanOutResults []FanOutJoinResult, forEachResults []ForEachResult, position int, retryOverrides map[StepID]RetryPolicy, threadID ThreadID) (WorkflowRunResult, error) {
	lastStepID := StepID("")
	lastPosition := 0

	for len(frames) > 0 {
		top := len(frames) - 1
		if frames[top].index >= len(frames[top].steps) {
			frames = frames[:top]
			if len(frames) > 0 {
				frames[len(frames)-1].index++
			}
			continue
		}

		step := frames[top].steps[frames[top].index]
		position++
		stepID := step.definition.ID

		if err := ctx.Err(); err != nil {
			emitter.terminal(runID, position, stepID, RunEventCancelled, RunStatusCancelled, err)
			w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusCancelled, &WorkflowError{Kind: WorkflowErrorCancelled, Step: position, StepID: stepID, Err: err})
			return w.cancelled(runID, metadata, path, fanOutResults, position, stepID, err)
		}

		if len(step.branches) > 0 {
			stepStart := emitter.emitStepStarted(runID, position, stepID)

			if err := validateStepValue(step.inputSchema, ValidationTargetStepInput, current); err != nil {
				stepErr := &WorkflowError{Kind: WorkflowErrorInvalidBranchInput, Step: position, StepID: stepID, Err: err}
				emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
				emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
				w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusFailed, stepErr)
				return w.fail(runID, metadata, path, fanOutResults, stepErr)
			}

			selected, matchErr := w.selectBranch(ctx, step, current)
			if matchErr != nil {
				kind := WorkflowErrorBranchConditionFailed
				if errors.Is(matchErr, ErrWorkflowNoBranchMatched) {
					kind = WorkflowErrorNoBranchMatched
				}
				stepErr := &WorkflowError{Kind: kind, Step: position, StepID: stepID, Err: matchErr}
				emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
				emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
				w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusFailed, stepErr)
				return w.fail(runID, metadata, path, fanOutResults, stepErr)
			}

			emitter.emitBranchSelected(runID, position, stepID, selected.name)
			path = append(path, selected.entryID)
			emitter.emitStepFinished(runID, position, stepID, stepStart, nil)

			frames = append(frames, stepFrame{steps: selected.steps, index: 0})
			continue
		}

		if step.mapPath != "" || len(step.mapFields) > 0 {
			stepStart := emitter.emitStepStarted(runID, position, stepID)
			if err := validateStepValue(step.inputSchema, ValidationTargetStepInput, current); err != nil {
				stepErr := &WorkflowError{Kind: WorkflowErrorInvalidStepInput, Step: position, StepID: stepID, Err: err}
				emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
				emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
				w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusFailed, stepErr)
				return w.fail(runID, metadata, path, fanOutResults, stepErr)
			}
			output, err := executeMap(step.mapPath, step.mapFields, current)
			if err != nil {
				stepErr := &WorkflowError{Kind: WorkflowErrorStepFailed, Step: position, StepID: stepID, Err: err}
				emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
				emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
				w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusFailed, stepErr)
				return w.fail(runID, metadata, path, fanOutResults, stepErr)
			}
			if err := validateStepValue(step.outputSchema, ValidationTargetStepOutput, output); err != nil {
				stepErr := &WorkflowError{Kind: WorkflowErrorInvalidStepOutput, Step: position, StepID: stepID, Err: err}
				emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
				emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
				w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusFailed, stepErr)
				return w.fail(runID, metadata, path, fanOutResults, stepErr)
			}
			if err := w.persistStep(ctx, anchor, path, position, stepID, output, completedOutputs, forEachResults); err != nil {
				stepErr := &WorkflowError{Kind: WorkflowErrorStepFailed, Step: position, StepID: stepID, Err: fmt.Errorf("lebro: persist workflow map step %q: %w", stepID, err)}
				emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
				emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
				w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusFailed, stepErr)
				return w.fail(runID, metadata, path, fanOutResults, stepErr)
			}
			emitter.emitStepFinished(runID, position, stepID, stepStart, nil)
			current, completedOutputs, lastStepID, lastPosition = cloneRawMessage(output), append(completedOutputs, cloneRawMessage(output)), stepID, position
			frames[top].index++
			continue
		}

		if step.forEach != nil {
			stepStart := emitter.emitStepStarted(runID, position, stepID)
			if err := validateStepValue(step.inputSchema, ValidationTargetStepInput, current); err != nil {
				stepErr := &WorkflowError{Kind: WorkflowErrorInvalidStepInput, Step: position, StepID: stepID, Err: err}
				emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
				emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
				w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusFailed, stepErr)
				return w.fail(runID, metadata, path, fanOutResults, stepErr)
			}
			output, foreachResult, foreachErr := w.executeForEach(ctx, emitter, runID, metadata, current, position, stepID, step.forEach, retryOverrides, threadID)
			forEachResults = append(forEachResults, foreachResult)
			if foreachErr != nil {
				emitter.emitStepFinished(runID, position, stepID, stepStart, foreachErr)
				if foreachErr.Kind == WorkflowErrorCancelled {
					emitter.terminal(runID, position, stepID, RunEventCancelled, RunStatusCancelled, foreachErr)
					w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusCancelled, foreachErr)
					result, err := w.cancelled(runID, metadata, path, fanOutResults, position, stepID, foreachErr.Err)
					result.ForEach = cloneForEachResults(forEachResults)
					return result, err
				}
				emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, foreachErr)
				w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusFailed, foreachErr)
				result, err := w.fail(runID, metadata, path, fanOutResults, foreachErr)
				result.ForEach = cloneForEachResults(forEachResults)
				return result, err
			}
			if err := w.persistForEachStep(ctx, anchor, path, position, stepID, output, completedOutputs, forEachResults); err != nil {
				stepErr := &WorkflowError{Kind: WorkflowErrorStepFailed, Step: position, StepID: stepID, Err: fmt.Errorf("lebro: persist workflow foreach step %q: %w", stepID, err)}
				emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
				emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
				return w.fail(runID, metadata, path, fanOutResults, stepErr)
			}
			emitter.emitStepFinished(runID, position, stepID, stepStart, nil)
			current, completedOutputs, lastStepID, lastPosition = cloneRawMessage(output), append(completedOutputs, cloneRawMessage(output)), stepID, position
			frames[top].index++
			continue
		}

		if step.loop != nil {
			stepStart := emitter.emitStepStarted(runID, position, stepID)
			if err := validateStepValue(step.inputSchema, ValidationTargetStepInput, current); err != nil {
				stepErr := &WorkflowError{Kind: WorkflowErrorInvalidStepInput, Step: position, StepID: stepID, Err: err}
				emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
				emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
				w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusFailed, stepErr)
				return w.fail(runID, metadata, path, fanOutResults, stepErr)
			}
			output, loopErr := w.executeLoop(ctx, anchor, emitter, runID, metadata, current, position, stepID, step.loop, completedOutputs, retryOverrides, threadID)
			if loopErr != nil {
				emitter.emitStepFinished(runID, position, stepID, stepStart, loopErr)
				if loopErr.Kind == WorkflowErrorCancelled {
					emitter.terminal(runID, position, stepID, RunEventCancelled, RunStatusCancelled, loopErr)
					w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusCancelled, loopErr)
					return w.cancelled(runID, metadata, path, fanOutResults, position, stepID, loopErr.Err)
				}
				emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, loopErr)
				w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusFailed, loopErr)
				return w.fail(runID, metadata, path, fanOutResults, loopErr)
			}
			if err := w.persistStep(ctx, anchor, path, position, stepID, output, completedOutputs, forEachResults); err != nil {
				stepErr := &WorkflowError{Kind: WorkflowErrorStepFailed, Step: position, StepID: stepID, Err: fmt.Errorf("lebro: persist workflow loop step %q: %w", stepID, err)}
				emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
				emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
				return w.fail(runID, metadata, path, fanOutResults, stepErr)
			}
			emitter.emitStepFinished(runID, position, stepID, stepStart, nil)
			current, completedOutputs, lastStepID, lastPosition = cloneRawMessage(output), append(completedOutputs, cloneRawMessage(output)), stepID, position
			frames[top].index++
			continue
		}

		if step.fanOut != nil {
			stepStart := emitter.emitStepStarted(runID, position, stepID)

			if err := validateStepValue(step.inputSchema, ValidationTargetStepInput, current); err != nil {
				stepErr := &WorkflowError{Kind: WorkflowErrorInvalidFanOutInput, Step: position, StepID: stepID, Err: err}
				emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
				emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
				w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusFailed, stepErr)
				return w.fail(runID, metadata, path, fanOutResults, stepErr)
			}

			joinedOutput, joinResult, fanErr := w.executeFanOut(ctx, anchor, emitter, runID, metadata, current, position, stepID, step, retryOverrides, threadID)
			if fanErr != nil {
				fanOutResults = append(fanOutResults, joinResult)
				emitter.emitStepFinished(runID, position, stepID, stepStart, fanErr)
				if fanErr.Kind == WorkflowErrorCancelled {
					emitter.terminal(runID, position, stepID, RunEventCancelled, RunStatusCancelled, fanErr)
					w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusCancelled, fanErr)
					return w.cancelled(runID, metadata, path, fanOutResults, position, stepID, fanErr.Err)
				}
				emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, fanErr)
				w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusFailed, fanErr)
				return w.fail(runID, metadata, path, fanOutResults, fanErr)
			}

			joinForSnapshot := []FanOutJoinResult{joinResult}
			if perr := w.persistFanOutStep(ctx, anchor, path, position, stepID, joinedOutput, completedOutputs, joinForSnapshot); perr != nil {
				stepErr := &WorkflowError{Kind: WorkflowErrorStepFailed, Step: position, StepID: stepID, Err: fmt.Errorf("lebro: persist workflow fan-out step %q: %w", stepID, perr)}
				emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
				emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
				w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusFailed, stepErr)
				return w.fail(runID, metadata, path, fanOutResults, stepErr)
			}

			emitter.emitStepFinished(runID, position, stepID, stepStart, nil)
			fanOutResults = append(fanOutResults, joinResult)
			current = cloneRawMessage(joinedOutput)
			completedOutputs = append(completedOutputs, cloneRawMessage(joinedOutput))
			lastStepID = stepID
			lastPosition = position
			frames[top].index++
			continue
		}

		if step.definition.Sleep != nil || step.definition.SleepUntil != nil {
			stepStart := emitter.emitStepStarted(runID, position, stepID)
			// Sleep is pass-through, so both schemas intentionally gate current.
			if err := validateStepValue(step.inputSchema, ValidationTargetStepInput, current); err != nil {
				stepErr := &WorkflowError{Kind: WorkflowErrorInvalidStepInput, Step: position, StepID: stepID, Err: err}
				emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
				emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
				w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusFailed, stepErr)
				return w.fail(runID, metadata, path, fanOutResults, stepErr)
			}
			if err := validateStepValue(step.outputSchema, ValidationTargetStepOutput, current); err != nil {
				stepErr := &WorkflowError{Kind: WorkflowErrorInvalidStepOutput, Step: position, StepID: stepID, Err: err}
				emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
				emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
				w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusFailed, stepErr)
				return w.fail(runID, metadata, path, fanOutResults, stepErr)
			}
			at := w.clock.Now().UTC()
			if step.definition.Sleep != nil {
				at = at.Add(step.definition.Sleep.Duration)
			} else {
				at = step.definition.SleepUntil.At.UTC()
			}
			token := fmt.Sprintf("%s-wake-%d", runID, position)
			if err := w.persistSleep(ctx, anchor, path, fanOutResults, forEachResults, position, stepID, completedOutputs, current, at, token); err != nil {
				stepErr := &WorkflowError{Kind: WorkflowErrorStepFailed, Step: position, StepID: stepID, Err: fmt.Errorf("lebro: persist workflow sleep at step %q: %w", stepID, err)}
				emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
				emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
				w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusFailed, stepErr)
				return w.fail(runID, metadata, path, fanOutResults, stepErr)
			}
			emitter.emitStepFinished(runID, position, stepID, stepStart, nil)
			emitter.emitSuspended(runID, position, stepID)
			return WorkflowRunResult{ID: runID, Status: RunStatusSuspended, Metadata: metadata, Suspend: &SuspendResult{Step: position, StepID: stepID, Payload: cloneRawMessage(current)}, Path: cloneStepIDs(path), FanOut: cloneFanOutJoinResults(fanOutResults), ForEach: cloneForEachResults(forEachResults)}, nil
		}

		stepStart := emitter.emitStepStarted(runID, position, stepID)

		if err := validateStepValue(step.inputSchema, ValidationTargetStepInput, current); err != nil {
			stepErr := &WorkflowError{Kind: WorkflowErrorInvalidStepInput, Step: position, StepID: stepID, Err: err}
			emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
			emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
			w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusFailed, stepErr)
			return w.fail(runID, metadata, path, fanOutResults, stepErr)
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
				w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusCancelled, stepErr)
				return w.cancelled(runID, metadata, path, fanOutResults, position, stepID, cause)
			}
			if runErr.kind == retryPanicked {
				stepErr := &WorkflowError{Kind: WorkflowErrorStepPanicked, Step: position, StepID: stepID, Err: runErr.cause}
				emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
				emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
				w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusFailed, stepErr)
				return w.fail(runID, metadata, path, fanOutResults, stepErr)
			}
			if runErr.kind == retryInvalidOutput {
				stepErr := &WorkflowError{Kind: WorkflowErrorInvalidStepOutput, Step: position, StepID: stepID, Err: runErr.cause}
				emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
				emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
				w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusFailed, stepErr)
				return w.fail(runID, metadata, path, fanOutResults, stepErr)
			}
			if runErr.kind == retrySuspended {
				suspendErr := runErr.cause.(*SuspendError)
				signal := suspendErr.Signal
				if perr := w.persistSuspend(ctx, anchor, path, fanOutResults, forEachResults, position, stepID, completedOutputs, signal); perr != nil {
					stepErr := &WorkflowError{Kind: WorkflowErrorStepFailed, Step: position, StepID: stepID, Err: fmt.Errorf("lebro: persist workflow suspend at step %q: %w", stepID, perr)}
					emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
					emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
					w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusFailed, stepErr)
					return w.fail(runID, metadata, path, fanOutResults, stepErr)
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
					Path:   cloneStepIDs(path),
					FanOut: cloneFanOutJoinResults(fanOutResults),
				}, nil
			}
			stepErr := &WorkflowError{Kind: WorkflowErrorStepFailed, Step: position, StepID: stepID, Err: runErr.cause}
			emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
			emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
			w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusFailed, stepErr)
			return w.fail(runID, metadata, path, fanOutResults, stepErr)
		}

		if perr := w.persistStep(ctx, anchor, path, position, stepID, output, completedOutputs, forEachResults); perr != nil {
			stepErr := &WorkflowError{Kind: WorkflowErrorStepFailed, Step: position, StepID: stepID, Err: fmt.Errorf("lebro: persist workflow step %q: %w", stepID, perr)}
			emitter.emitStepFinished(runID, position, stepID, stepStart, stepErr)
			emitter.terminal(runID, position, stepID, RunEventFailed, RunStatusFailed, stepErr)
			w.persistTerminalBestEffort(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusFailed, stepErr)
			return w.fail(runID, metadata, path, fanOutResults, stepErr)
		}

		emitter.emitStepFinished(runID, position, stepID, stepStart, nil)
		current = cloneRawMessage(output)
		completedOutputs = append(completedOutputs, cloneRawMessage(output))
		lastStepID = stepID
		lastPosition = position
		frames[top].index++
	}

	finalOutput := cloneRawMessage(current)
	if perr := w.persistTerminal(anchor, path, lastPosition, lastStepID, fanOutResults, completedOutputs, RunStatusSucceeded, nil); perr != nil {
		stepErr := &WorkflowError{Kind: WorkflowErrorStepFailed, Err: fmt.Errorf("lebro: persist workflow terminal state: %w", perr)}
		emitter.terminal(runID, lastPosition, lastStepID, RunEventFailed, RunStatusFailed, stepErr)
		return w.fail(runID, metadata, path, fanOutResults, stepErr)
	}
	emitter.terminal(runID, lastPosition, lastStepID, RunEventSucceeded, RunStatusSucceeded, nil)
	return WorkflowRunResult{
		ID:       runID,
		Status:   RunStatusSucceeded,
		Output:   finalOutput,
		Metadata: metadata,
		Path:     cloneStepIDs(path),
		FanOut:   cloneFanOutJoinResults(fanOutResults),
		ForEach:  cloneForEachResults(forEachResults),
	}, nil
}

// selectBranch evaluates the branch predicates in declared order and returns
// the first matching compiled branch. When no predicate matches, the default
// branch is returned if configured. When no default is configured,
// ErrWorkflowNoBranchMatched is returned. A predicate error is wrapped with
// the branch name and returned; the executor maps it to
// WorkflowErrorBranchConditionFailed.
func (w *LinearWorkflow) selectBranch(ctx context.Context, step compiledStep, input json.RawMessage) (compiledBranch, error) {
	for i := range step.branches {
		br := &step.branches[i]
		matched, err := br.condition(ctx, input)
		if err != nil {
			return compiledBranch{}, fmt.Errorf("lebro: branch %q condition failed: %w", br.name, err)
		}
		if matched {
			return *br, nil
		}
	}
	if step.defaultBranch != "" {
		for i := range step.branches {
			if step.branches[i].name == step.defaultBranch {
				return step.branches[i], nil
			}
		}
	}
	return compiledBranch{}, ErrWorkflowNoBranchMatched
}

// resumeFrames reconstructs the frame stack from the persisted path and the
// suspending step's StepID. It walks the step tree following the path (the
// entry StepIDs of selected branches) until it finds the suspend step, then
// returns the frame stack with the top frame's index pointing past it so
// executeFrames resumes at the next step. ok is false when the suspend step
// cannot be located, indicating snapshot corruption.
func (w *LinearWorkflow) resumeFrames(path []StepID, suspendStepID StepID) ([]stepFrame, bool) {
	frames := []stepFrame{{steps: w.steps, index: 0}}
	pathIdx := 0
	for len(frames) > 0 {
		top := len(frames) - 1
		if frames[top].index >= len(frames[top].steps) {
			frames = frames[:top]
			if len(frames) > 0 {
				frames[len(frames)-1].index++
			}
			continue
		}
		step := frames[top].steps[frames[top].index]
		if step.definition.ID == suspendStepID {
			frames[top].index++
			return frames, true
		}
		if len(step.branches) > 0 && pathIdx < len(path) {
			found := false
			for i := range step.branches {
				if step.branches[i].entryID == path[pathIdx] {
					pathIdx++
					frames = append(frames, stepFrame{steps: step.branches[i].steps, index: 0})
					found = true
					break
				}
			}
			if found {
				continue
			}
		}
		frames[top].index++
	}
	return nil, false
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
	if run.WorkflowID != w.definition.ID {
		return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorStepFailed, Err: fmt.Errorf("lebro: run %q belongs to workflow %q, not %q", input.RunID, run.WorkflowID, w.definition.ID)}
	}
	if run.Status != RunStatusSuspended {
		return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorStepFailed, Err: fmt.Errorf("lebro: run %q %w: %s", input.RunID, ErrNotSuspended, run.Status)}
	}

	// Resume re-authorizes the run against the caller identity on the resume
	// context. A suspended run may be resumed by a different caller than the one
	// that started it, so the policy must gate the resumed execution rather than
	// trusting the original authorization. A denial fails the run without
	// executing any remaining step.
	if derr := authorize(ctx, w.policy, ActionWorkflowRun, Resource{Kind: ResourceKindWorkflow, ID: string(w.definition.ID)}); derr != nil {
		return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorUnauthorized, Err: derr}
	}

	_, envelope, err := w.loadSuspendSnapshot(ctx, input.RunID)
	if err != nil {
		return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorStepFailed, Err: err}
	}
	if envelope.Suspend != nil && envelope.Suspend.Wake != nil {
		return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorStepFailed, Err: ErrWorkflowSleepRequiresScheduler}
	}

	resumeInput := cloneRawMessage(input.Input)
	// Validate the resume input structurally against the suspending step's
	// SuspendSchema (the same schema that validated the persisted contract at
	// the suspend boundary), then require it to match the persisted contract
	// value so the suspending step's published expectation constrains resume.
	// A step without a SuspendSchema cannot have suspended (the executor
	// rejects that at suspend time), so a missing compiled schema here is a
	// corruption error rather than a skip.
	compiled := w.suspendSchemaForStep(envelope.StepID)
	if compiled == nil {
		return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorStepFailed, Err: fmt.Errorf("lebro: cannot resolve SuspendSchema for step %q to validate resume input", envelope.StepID)}
	}
	if verr := validateStepValue(compiled, ValidationTargetResumeInput, resumeInput); verr != nil {
		return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorStepFailed, Err: fmt.Errorf("lebro: %w: %s", ErrInvalidResumeInput, verr)}
	}
	if len(envelope.Suspend.Contract) > 0 && !rawJSONEqual(resumeInput, envelope.Suspend.Contract) {
		return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorStepFailed, Err: fmt.Errorf("lebro: %w: resume input does not match the persisted contract", ErrInvalidResumeInput)}
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
		input:     WorkflowRunInput{Input: cloneRawMessage(run.Input), ThreadID: run.ThreadID, RunID: run.ID, Scope: RuntimeScope{Namespace: run.Namespace, OwnerID: run.OwnerID}, Metadata: mergedMetadata},
		metadata:  mergedMetadata,
		startedAt: run.StartedAt,
	}

	// The stored run record stays Suspended until the first resumed step
	// persists. A process crash before any step persists therefore leaves
	// the run resumable rather than orphaned in Running. Each persistStep in
	// executeFrames flips the record to Running with the current step, and
	// persistTerminal sets the final status.

	// Reconstruct the frame stack from the persisted path and the suspend
	// step's StepID so Resume continues at the next step within the correct
	// branch (or the parent list when the suspend step was a branch's last
	// step).
	frames, ok := w.resumeFrames(envelope.Path, envelope.StepID)
	if !ok {
		return WorkflowRunResult{}, &WorkflowError{Kind: WorkflowErrorStepFailed, Err: fmt.Errorf("lebro: cannot resolve resume frame for suspended step %q", envelope.StepID)}
	}

	emitter := newRunEmitter(ctx, w.listener, w.clock, w.idSource)
	resumePosition := envelope.Step + 1
	var resumeStepID StepID
	if top := len(frames) - 1; top >= 0 && frames[top].index < len(frames[top].steps) {
		resumeStepID = frames[top].steps[frames[top].index].definition.ID
	}
	emitter.emitResumed(run.ID, resumePosition, resumeStepID)

	completedOutputs := cloneRawOutputs(envelope.Outputs)
	return w.executeFrames(ctx, anchor, emitter, run.ID, mergedMetadata, resumeInput, completedOutputs, frames, cloneStepIDs(envelope.Path), cloneFanOutJoinResults(envelope.FanOut), cloneForEachResults(envelope.ForEach), envelope.Step, nil, run.ThreadID)
}

// resumeWake resumes a sleep boundary only when token identifies the currently
// persisted wait. It is intentionally scheduler-only; callers use Resume for
// explicit human/input suspension.
func (w *LinearWorkflow) resumeWake(ctx context.Context, runID RunID, token string) (WorkflowRunResult, error) {
	if w == nil || w.store == nil || isNilInterface(w.store) {
		return WorkflowRunResult{}, errWorkflowWakeupStale
	}
	if err := ctx.Err(); err != nil {
		return WorkflowRunResult{}, err
	}
	run, err := w.store.WorkflowRuns().GetWorkflowRun(ctx, runID)
	if err != nil {
		return WorkflowRunResult{}, err
	}
	if run.WorkflowID != w.definition.ID || run.Status != RunStatusSuspended {
		return WorkflowRunResult{}, errWorkflowWakeupStale
	}
	_, envelope, err := w.loadSuspendSnapshot(ctx, runID)
	if err != nil {
		return WorkflowRunResult{}, err
	}
	if envelope.Suspend == nil || envelope.Suspend.Wake == nil || envelope.Suspend.Wake.Token != token {
		return WorkflowRunResult{}, errWorkflowWakeupStale
	}
	if derr := authorize(ctx, w.policy, ActionWorkflowRun, Resource{Kind: ResourceKindWorkflow, ID: string(w.definition.ID)}); derr != nil {
		return WorkflowRunResult{}, derr
	}
	frames, ok := w.resumeFrames(envelope.Path, envelope.StepID)
	if !ok {
		return WorkflowRunResult{}, fmt.Errorf("lebro: cannot resolve resume frame for sleep step %q", envelope.StepID)
	}
	metadata := decodeMetadata(run.Metadata)
	anchor := runAnchor{runID: run.ID, input: WorkflowRunInput{Input: cloneRawMessage(run.Input), ThreadID: run.ThreadID, RunID: run.ID, Scope: RuntimeScope{Namespace: run.Namespace, OwnerID: run.OwnerID}, Metadata: metadata}, metadata: metadata, startedAt: run.StartedAt}
	emitter := newRunEmitter(ctx, w.listener, w.clock, w.idSource)
	resumePosition := envelope.Step + 1
	var resumeStepID StepID
	if top := len(frames) - 1; top >= 0 && frames[top].index < len(frames[top].steps) {
		resumeStepID = frames[top].steps[frames[top].index].definition.ID
	}
	emitter.emitResumed(run.ID, resumePosition, resumeStepID)
	return w.executeFrames(ctx, anchor, emitter, run.ID, metadata, cloneRawMessage(envelope.Suspend.Payload), cloneRawOutputs(envelope.Outputs), frames, cloneStepIDs(envelope.Path), cloneFanOutJoinResults(envelope.FanOut), cloneForEachResults(envelope.ForEach), envelope.Step, nil, run.ThreadID)
}

// loadSuspendSnapshot lists snapshots for runID across all pages and returns
// the suspend snapshot with the highest sequence. Only suspend boundaries
// populate the envelope's Suspend field, so non-suspend step snapshots are
// skipped. Pagination follows NextCursor so a run that accumulated more
// snapshots than a single page can still resume.
func (w *LinearWorkflow) loadSuspendSnapshot(ctx context.Context, runID RunID) (WorkflowSnapshotRecord, workflowSnapshotEnvelope, error) {
	var (
		best    WorkflowSnapshotRecord
		bestEnv workflowSnapshotEnvelope
		cursor  string
	)
	for {
		page, err := w.store.WorkflowSnapshots().ListWorkflowSnapshots(ctx, runID, PageRequest{Cursor: cursor, Limit: 1000})
		if err != nil {
			return WorkflowSnapshotRecord{}, workflowSnapshotEnvelope{}, fmt.Errorf("lebro: list snapshots for run %q: %w", runID, err)
		}
		if len(page.Records) == 0 && cursor == "" {
			return WorkflowSnapshotRecord{}, workflowSnapshotEnvelope{}, fmt.Errorf("lebro: no snapshot for suspended run %q", runID)
		}
		for _, cand := range page.Records {
			var env workflowSnapshotEnvelope
			if err := json.Unmarshal(cand.State, &env); err != nil {
				continue
			}
			if env.Suspend == nil {
				continue
			}
			if best.ID == "" || cand.Sequence > best.Sequence {
				best = cand
				bestEnv = env
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if best.ID == "" {
		return WorkflowSnapshotRecord{}, workflowSnapshotEnvelope{}, fmt.Errorf("lebro: no suspend snapshot for run %q", runID)
	}
	return best, bestEnv, nil
}

// suspendSchemaForStep returns the compiled SuspendSchema for the declared
// step, or nil when the step is not declared or has no SuspendSchema. Resume
// uses it to validate WorkflowResumeInput.Input against the same schema that
// validated the persisted SuspendSignal.Contract at the suspend boundary. It
// searches the full step tree including branch steps so a suspend within a
// branch resolves the correct schema.
func (w *LinearWorkflow) suspendSchemaForStep(stepID StepID) CompiledSchema {
	for _, step := range w.steps {
		if found := findSuspendSchema(step, stepID); found != nil {
			return found
		}
	}
	return nil
}

func findSuspendSchema(step compiledStep, stepID StepID) CompiledSchema {
	if step.definition.ID == stepID {
		return step.suspendSchema
	}
	for _, br := range step.branches {
		for _, bs := range br.steps {
			if found := findSuspendSchema(bs, stepID); found != nil {
				return found
			}
		}
	}
	return nil
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

// rawJSONEqual reports whether two JSON values are equal after canonical
// decoding. It is used by Resume to compare the resume input against the
// persisted suspend contract value. A nil or empty payload returns nil.
func rawJSONEqual(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// persistTerminalBestEffort writes the terminal run record on failure and
// cancellation paths. The run already reached a terminal error in memory, so
// a terminal persistence failure is best-effort and the original error is
// preserved for the caller.
func (w *LinearWorkflow) persistTerminalBestEffort(anchor runAnchor, path []StepID, currentStep int, currentStepID StepID, fanOutResults []FanOutJoinResult, completed []json.RawMessage, status RunStatus, failure *WorkflowError) {
	_ = w.persistTerminal(anchor, path, currentStep, currentStepID, fanOutResults, completed, status, failure)
}

// persistSuspend writes a suspend snapshot and updates the run record to
// Suspended inside a single Store.Transaction so the suspend boundary is
// atomic. The snapshot carries the validated resume contract, opaque
// payload, and cumulative branch path so Resume can validate
// WorkflowResumeInput.Input and reconstruct the frame stack without re-running
// the suspending step. The run record's CurrentStep/CurrentStepID identify
// the suspending step; FinishedAt stays nil because the run is resumable.
// When no Store is bound the suspend is in-memory only and the caller still
// receives a suspended WorkflowRunResult.
func (w *LinearWorkflow) persistSuspend(ctx context.Context, anchor runAnchor, path []StepID, fanOutResults []FanOutJoinResult, forEachResults []ForEachResult, position int, stepID StepID, completed []json.RawMessage, signal SuspendSignal) error {
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
		Path:    cloneStepIDs(path),
		FanOut:  cloneFanOutJoinResults(fanOutResults),
		ForEach: cloneForEachResults(forEachResults),
	}
	state, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("lebro: encode workflow suspend snapshot: %w", err)
	}
	now := w.clock.Now()
	snapshot := WorkflowSnapshotRecord{
		ID:            fmt.Sprintf("%s-snapshot-%d", anchor.runID, position),
		RunID:         anchor.runID,
		Sequence:      workflowTerminalSnapshotSequence(position),
		SchemaVersion: workflowSnapshotSchemaVersion,
		State:         state,
		CreatedAt:     now,
	}
	updated := w.baseRunRecord(anchor)
	updated.Status = RunStatusSuspended
	updated.CurrentStep = position
	updated.CurrentStepID = stepID
	updated.StepOutputs = cloneRawOutputs(completed)
	updated.Path = cloneStepIDs(path)
	updated.UpdatedAt = now
	return w.store.Transaction(ctx, func(ctx context.Context, repos Repositories) error {
		if err := repos.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, snapshot); err != nil {
			return err
		}
		return repos.WorkflowRuns().SaveWorkflowRun(ctx, updated)
	})
}

// persistSleep makes the suspended snapshot and its one-shot scheduler wakeup
// visible in one transaction. A restarted process therefore sees both or
// neither; the scheduler's token check makes a retry harmless.
func (w *LinearWorkflow) persistSleep(ctx context.Context, anchor runAnchor, path []StepID, fanOutResults []FanOutJoinResult, forEachResults []ForEachResult, position int, stepID StepID, completed []json.RawMessage, input json.RawMessage, at time.Time, token string) error {
	if w.store == nil || isNilInterface(w.store) {
		return errors.New("lebro: durable sleep requires a bound store")
	}
	envelope := workflowSnapshotEnvelope{Step: position, StepID: stepID, Outputs: cloneRawOutputs(completed), Suspend: &suspendEnvelope{Payload: cloneRawMessage(input), Wake: &wakeEnvelope{At: at, Token: token}}, Path: cloneStepIDs(path), FanOut: cloneFanOutJoinResults(fanOutResults), ForEach: cloneForEachResults(forEachResults)}
	state, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("lebro: encode workflow sleep snapshot: %w", err)
	}
	now := w.clock.Now().UTC()
	snapshot := WorkflowSnapshotRecord{ID: fmt.Sprintf("%s-snapshot-%d", anchor.runID, position), RunID: anchor.runID, Sequence: workflowTerminalSnapshotSequence(position), SchemaVersion: workflowSnapshotSchemaVersion, State: state, CreatedAt: now}
	updated := w.baseRunRecord(anchor)
	updated.Status, updated.CurrentStep, updated.CurrentStepID, updated.StepOutputs, updated.Path, updated.UpdatedAt = RunStatusSuspended, position, stepID, cloneRawOutputs(completed), cloneStepIDs(path), now
	wakeup := ScheduleRecord{ID: ScheduleID(fmt.Sprintf("%s-wake-%d", anchor.runID, position)), WorkflowID: w.definition.ID, Namespace: anchor.input.Scope.Namespace, OwnerID: anchor.input.Scope.OwnerID, Spec: "@once", WakeRunID: anchor.runID, WakeToken: token, NextFireAt: &at, CreatedAt: now, UpdatedAt: now}
	return w.store.Transaction(ctx, func(ctx context.Context, repos Repositories) error {
		if err := repos.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, snapshot); err != nil {
			return err
		}
		if err := repos.WorkflowRuns().SaveWorkflowRun(ctx, updated); err != nil {
			return err
		}
		return repos.Schedules().SaveSchedule(ctx, wakeup)
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

// persistFanOutStep writes a snapshot at a completed fan-out step boundary and
// updates the run record inside a single Store.Transaction so the boundary is
// atomic. The snapshot carries the joined output and the fan-out join records
// so each child branch's result is durable and inspectable.
func (w *LinearWorkflow) persistFanOutStep(ctx context.Context, anchor runAnchor, path []StepID, position int, stepID StepID, output json.RawMessage, completed []json.RawMessage, fanOutResults []FanOutJoinResult) error {
	if w.store == nil || isNilInterface(w.store) {
		return nil
	}
	stepOutputs := cloneRawOutputs(completed)
	if len(output) > 0 {
		stepOutputs = append(stepOutputs, cloneRawMessage(output))
	}
	envelope := workflowSnapshotEnvelope{
		Step:    position,
		StepID:  stepID,
		Output:  cloneRawMessage(output),
		Outputs: cloneRawOutputs(stepOutputs),
		Path:    cloneStepIDs(path),
		FanOut:  cloneFanOutJoinResults(fanOutResults),
	}
	state, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("lebro: encode workflow fan-out snapshot: %w", err)
	}
	now := w.clock.Now()
	snapshot := WorkflowSnapshotRecord{
		ID:            fmt.Sprintf("%s-snapshot-%d", anchor.runID, position),
		RunID:         anchor.runID,
		Sequence:      workflowTerminalSnapshotSequence(position),
		SchemaVersion: workflowSnapshotSchemaVersion,
		State:         state,
		CreatedAt:     now,
	}
	updated := w.baseRunRecord(anchor)
	updated.Status = RunStatusRunning
	updated.CurrentStep = position
	updated.CurrentStepID = stepID
	updated.StepOutputs = stepOutputs
	updated.Path = cloneStepIDs(path)
	updated.UpdatedAt = now
	return w.store.Transaction(ctx, func(ctx context.Context, repos Repositories) error {
		if err := repos.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, snapshot); err != nil {
			return err
		}
		return repos.WorkflowRuns().SaveWorkflowRun(ctx, updated)
	})
}

func (w *LinearWorkflow) persistForEachStep(ctx context.Context, anchor runAnchor, path []StepID, position int, stepID StepID, output json.RawMessage, completed []json.RawMessage, results []ForEachResult) error {
	if w.store == nil || isNilInterface(w.store) {
		return nil
	}
	stepOutputs := cloneRawOutputs(completed)
	if len(output) > 0 {
		stepOutputs = append(stepOutputs, cloneRawMessage(output))
	}
	state, err := json.Marshal(workflowSnapshotEnvelope{Step: position, StepID: stepID, Output: cloneRawMessage(output), Outputs: cloneRawOutputs(stepOutputs), Path: cloneStepIDs(path), ForEach: cloneForEachResults(results)})
	if err != nil {
		return fmt.Errorf("lebro: encode workflow foreach snapshot: %w", err)
	}
	now := w.clock.Now()
	snapshot := WorkflowSnapshotRecord{ID: fmt.Sprintf("%s-snapshot-%d", anchor.runID, position), RunID: anchor.runID, Sequence: workflowTerminalSnapshotSequence(position), SchemaVersion: workflowSnapshotSchemaVersion, State: state, CreatedAt: now}
	updated := w.baseRunRecord(anchor)
	updated.Status = RunStatusRunning
	updated.CurrentStep = position
	updated.CurrentStepID = stepID
	updated.StepOutputs = stepOutputs
	updated.Path = cloneStepIDs(path)
	updated.UpdatedAt = now
	return w.store.Transaction(ctx, func(ctx context.Context, repos Repositories) error {
		if err := repos.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, snapshot); err != nil {
			return err
		}
		return repos.WorkflowRuns().SaveWorkflowRun(ctx, updated)
	})
}

// persistStep writes a snapshot at the completed step boundary and updates
// the run record inside a single Store.Transaction so the boundary is
// atomic. The updated run record carries the anchor's stable fields (input,
// thread ID, metadata, original start time) and the ordered completed
// outputs through the current step, so a process stop after a committed
// step leaves a resumable, inspectable Running record. When no Store is
// bound it is a no-op.
func (w *LinearWorkflow) persistStep(ctx context.Context, anchor runAnchor, path []StepID, position int, stepID StepID, output json.RawMessage, completed []json.RawMessage, forEachResults []ForEachResult) error {
	if w.store == nil || isNilInterface(w.store) {
		return nil
	}
	stepOutputs := append(cloneRawOutputs(completed), cloneRawMessage(output))
	envelope := workflowSnapshotEnvelope{
		Step:    position,
		StepID:  stepID,
		Output:  cloneRawMessage(output),
		Outputs: cloneRawOutputs(stepOutputs),
		Path:    cloneStepIDs(path),
		ForEach: cloneForEachResults(forEachResults),
	}
	state, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("lebro: encode workflow snapshot: %w", err)
	}
	now := w.clock.Now()
	snapshot := WorkflowSnapshotRecord{
		ID:            fmt.Sprintf("%s-snapshot-%d", anchor.runID, position),
		RunID:         anchor.runID,
		Sequence:      workflowTerminalSnapshotSequence(position),
		SchemaVersion: workflowSnapshotSchemaVersion,
		State:         state,
		CreatedAt:     now,
	}
	updated := w.baseRunRecord(anchor)
	updated.Status = RunStatusRunning
	updated.CurrentStep = position
	updated.CurrentStepID = stepID
	updated.StepOutputs = stepOutputs
	updated.Path = cloneStepIDs(path)
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
func (w *LinearWorkflow) persistTerminal(anchor runAnchor, path []StepID, currentStep int, currentStepID StepID, fanOutResults []FanOutJoinResult, completed []json.RawMessage, status RunStatus, failure *WorkflowError) error {
	if w.store == nil || isNilInterface(w.store) {
		return nil
	}
	now := w.clock.Now()
	record := w.baseRunRecord(anchor)
	record.Status = status
	record.CurrentStep = currentStep
	record.CurrentStepID = currentStepID
	record.Path = cloneStepIDs(path)
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
	if len(fanOutResults) > 0 {
		record.FanOut = cloneFanOutJoinResults(fanOutResults)
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
	record.Namespace = anchor.input.Scope.Namespace
	record.OwnerID = anchor.input.Scope.OwnerID
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

func cloneStepIDs(ids []StepID) []StepID {
	if len(ids) == 0 {
		return nil
	}
	cloned := make([]StepID, len(ids))
	copy(cloned, ids)
	return cloned
}

func cloneFanOutJoinResults(results []FanOutJoinResult) []FanOutJoinResult {
	if len(results) == 0 {
		return nil
	}
	cloned := make([]FanOutJoinResult, len(results))
	for i, r := range results {
		cloned[i] = FanOutJoinResult{
			StepID:   r.StepID,
			Status:   r.Status,
			Branches: cloneFanOutBranchResults(r.Branches),
		}
	}
	return cloned
}

func cloneForEachResults(results []ForEachResult) []ForEachResult {
	if len(results) == 0 {
		return nil
	}
	cloned := make([]ForEachResult, len(results))
	for i, result := range results {
		cloned[i] = ForEachResult{StepID: result.StepID, Status: result.Status, Items: make([]ForEachItemResult, len(result.Items))}
		for j, item := range result.Items {
			cloned[i].Items[j] = ForEachItemResult{Index: item.Index, Status: item.Status, Output: cloneRawMessage(item.Output), Failure: cloneWorkflowFailureData(item.Failure)}
		}
	}
	return cloned
}

func cloneFanOutBranchResults(results []FanOutBranchResult) []FanOutBranchResult {
	if len(results) == 0 {
		return nil
	}
	cloned := make([]FanOutBranchResult, len(results))
	for i, r := range results {
		cloned[i] = FanOutBranchResult{
			Name:    r.Name,
			Status:  r.Status,
			Output:  cloneRawMessage(r.Output),
			Failure: cloneWorkflowFailureData(r.Failure),
		}
	}
	return cloned
}

func cloneWorkflowFailureData(d *WorkflowFailureData) *WorkflowFailureData {
	if d == nil {
		return nil
	}
	copy := *d
	return &copy
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

func (w *LinearWorkflow) fail(runID RunID, metadata map[string]string, path []StepID, fanOutResults []FanOutJoinResult, stepErr *WorkflowError) (WorkflowRunResult, error) {
	return WorkflowRunResult{
		ID:       runID,
		Status:   RunStatusFailed,
		Metadata: metadata,
		Path:     cloneStepIDs(path),
		FanOut:   cloneFanOutJoinResults(fanOutResults),
	}, stepErr
}

func (w *LinearWorkflow) cancelled(runID RunID, metadata map[string]string, path []StepID, fanOutResults []FanOutJoinResult, step int, stepID StepID, err error) (WorkflowRunResult, error) {
	return WorkflowRunResult{
		ID:       runID,
		Status:   RunStatusCancelled,
		Metadata: metadata,
		Path:     cloneStepIDs(path),
		FanOut:   cloneFanOutJoinResults(fanOutResults),
	}, &WorkflowError{Kind: WorkflowErrorCancelled, Step: step, StepID: stepID, Err: preferContextError(err, err)}
}

type compiledStep struct {
	definition    StepDefinition
	handler       StepHandler
	inputSchema   CompiledSchema
	outputSchema  CompiledSchema
	suspendSchema CompiledSchema
	retry         *RetryPolicy
	branches      []compiledBranch
	defaultBranch string
	fanOut        *compiledFanOut
	mapFields     map[string]string
	mapPath       string
	forEach       *compiledForEach
	loop          *compiledLoop
}

// compiledBranch is the compiled form of a Branch. entryID is the StepID of
// the branch's first step; it is recorded in the run Path when the branch is
// selected so Resume can reconstruct the branch frame without branch names.
type compiledBranch struct {
	name      string
	condition BranchPredicate
	steps     []compiledStep
	entryID   StepID
}

// compiledFanOut is the compiled form of a FanOut. maxParallel bounds active
// branches; failurePolicy controls sibling cancellation; branches holds the
// compiled child branches with their input mappers.
type compiledFanOut struct {
	branches      []compiledFanOutBranch
	maxParallel   int
	failurePolicy FanOutFailurePolicy
}

// compiledFanOutBranch is the compiled form of a FanOutBranch.
type compiledFanOutBranch struct {
	name        string
	inputMapper FanOutInputMapper
	steps       []compiledStep
}

type compiledForEach struct {
	steps       []compiledStep
	maxParallel int
}

type loopMode string

const (
	loopWhile loopMode = "while"
	loopUntil loopMode = "until"
)

type compiledLoop struct {
	steps         []compiledStep
	maxIterations int
	condition     LoopPredicate
	mode          loopMode
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
	if len(step.Definition.Branches) > 0 {
		cs.defaultBranch = step.Definition.Default
		cs.branches = make([]compiledBranch, 0, len(step.Definition.Branches))
		for _, br := range step.Definition.Branches {
			compiledBranchSteps := make([]compiledStep, 0, len(br.Steps))
			for _, bs := range br.Steps {
				compiled, err := newCompiledStep(compiler, bs)
				if err != nil {
					return cs, fmt.Errorf("lebro: compile branch %q step %q: %w", br.Name, bs.Definition.ID, err)
				}
				compiledBranchSteps = append(compiledBranchSteps, compiled)
			}
			entryID := compiledBranchSteps[0].definition.ID
			cs.branches = append(cs.branches, compiledBranch{
				name:      br.Name,
				condition: br.Condition,
				steps:     compiledBranchSteps,
				entryID:   entryID,
			})
		}
	}
	if step.Definition.FanOut != nil {
		fo := step.Definition.FanOut
		compiledFO := &compiledFanOut{
			maxParallel:   fo.MaxParallel,
			failurePolicy: fo.FailurePolicy,
			branches:      make([]compiledFanOutBranch, 0, len(fo.Branches)),
		}
		if compiledFO.maxParallel < 1 {
			compiledFO.maxParallel = 1
		}
		if compiledFO.failurePolicy == "" {
			compiledFO.failurePolicy = FanOutFailFast
		}
		for _, fb := range fo.Branches {
			compiledBranchSteps := make([]compiledStep, 0, len(fb.Steps))
			for _, bs := range fb.Steps {
				compiled, err := newCompiledStep(compiler, bs)
				if err != nil {
					return cs, fmt.Errorf("lebro: compile fan-out branch %q step %q: %w", fb.Name, bs.Definition.ID, err)
				}
				compiledBranchSteps = append(compiledBranchSteps, compiled)
			}
			compiledFO.branches = append(compiledFO.branches, compiledFanOutBranch{
				name:        fb.Name,
				inputMapper: fb.InputMapper,
				steps:       compiledBranchSteps,
			})
		}
		cs.fanOut = compiledFO
	}
	if step.Definition.Map != nil {
		cs.mapPath = step.Definition.Map.Path
		cs.mapFields = make(map[string]string, len(step.Definition.Map.Fields))
		for k, v := range step.Definition.Map.Fields {
			cs.mapFields[k] = v
		}
	}
	if step.Definition.ForEach != nil {
		compiled := &compiledForEach{maxParallel: step.Definition.ForEach.MaxParallel}
		if compiled.maxParallel < 1 {
			compiled.maxParallel = 1
		}
		compiled.steps = make([]compiledStep, 0, len(step.Definition.ForEach.Steps))
		for _, child := range step.Definition.ForEach.Steps {
			result, err := newCompiledStep(compiler, child)
			if err != nil {
				return cs, fmt.Errorf("lebro: compile foreach step %q: %w", child.Definition.ID, err)
			}
			compiled.steps = append(compiled.steps, result)
		}
		cs.forEach = compiled
	}
	if step.Definition.DoWhile != nil || step.Definition.DoUntil != nil {
		var source []Step
		var maxIterations int
		var condition LoopPredicate
		mode := loopWhile
		if step.Definition.DoWhile != nil {
			source = step.Definition.DoWhile.Steps
			maxIterations = step.Definition.DoWhile.MaxIterations
			condition = step.Definition.DoWhile.Condition
		} else {
			source = step.Definition.DoUntil.Steps
			maxIterations = step.Definition.DoUntil.MaxIterations
			condition = step.Definition.DoUntil.Condition
			mode = loopUntil
		}
		compiled := &compiledLoop{maxIterations: maxIterations, condition: condition, mode: mode, steps: make([]compiledStep, 0, len(source))}
		for _, child := range source {
			result, err := newCompiledStep(compiler, child)
			if err != nil {
				return cs, fmt.Errorf("lebro: compile loop step %q: %w", child.Definition.ID, err)
			}
			compiled.steps = append(compiled.steps, result)
		}
		cs.loop = compiled
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

// validateStepTree walks the step tree (top-level and branch steps recursively)
// enforcing structural invariants and collecting every StepID into seen for
// global uniqueness. A branching step must not declare a Handler,
// OutputSchema, SuspendSchema, or Retry; its branches must have unique
// non-empty names, at least one step, and non-nil conditions; Default must
// name a declared branch. A non-branching step must have a non-nil handler.
// hasSchema is set to true when any step (at any depth) declares a schema.
func validateStepTree(steps []Step, seen map[StepID]struct{}, hasSchema *bool) error {
	for _, step := range steps {
		if step.Definition.ID == "" {
			return errors.New("lebro: workflow step ID is required")
		}
		if _, exists := seen[step.Definition.ID]; exists {
			return fmt.Errorf("lebro: workflow step ID %q is already registered", step.Definition.ID)
		}
		seen[step.Definition.ID] = struct{}{}
		controlCount := 0
		if len(step.Definition.Branches) > 0 {
			controlCount++
		}
		if step.Definition.FanOut != nil {
			controlCount++
		}
		if step.Definition.Map != nil {
			controlCount++
		}
		if step.Definition.ForEach != nil {
			controlCount++
		}
		if step.Definition.DoWhile != nil {
			controlCount++
		}
		if step.Definition.DoUntil != nil {
			controlCount++
		}
		if step.Definition.Sleep != nil {
			controlCount++
		}
		if step.Definition.SleepUntil != nil {
			controlCount++
		}
		if len(step.Definition.Branches) > 0 && step.Definition.FanOut != nil {
			return fmt.Errorf("lebro: workflow step %q cannot declare both Branches and FanOut", step.Definition.ID)
		}
		if controlCount > 1 {
			return fmt.Errorf("lebro: workflow step %q cannot combine control-flow definitions", step.Definition.ID)
		}
		if len(step.Definition.Branches) > 0 {
			if step.Handler != nil && !isNilInterface(step.Handler) {
				return fmt.Errorf("lebro: workflow branching step %q must not declare a Handler", step.Definition.ID)
			}
			if len(step.Definition.OutputSchema) > 0 {
				return fmt.Errorf("lebro: workflow branching step %q must not declare OutputSchema", step.Definition.ID)
			}
			if len(step.Definition.SuspendSchema) > 0 {
				return fmt.Errorf("lebro: workflow branching step %q must not declare SuspendSchema", step.Definition.ID)
			}
			if step.Definition.Retry != nil {
				return fmt.Errorf("lebro: workflow branching step %q must not declare Retry", step.Definition.ID)
			}
			if len(step.Definition.InputSchema) > 0 {
				*hasSchema = true
			}
			branchNames := make(map[string]struct{}, len(step.Definition.Branches))
			for _, br := range step.Definition.Branches {
				if br.Name == "" {
					return fmt.Errorf("lebro: workflow branching step %q has a branch with empty Name", step.Definition.ID)
				}
				if _, exists := branchNames[br.Name]; exists {
					return fmt.Errorf("lebro: workflow branching step %q has duplicate branch name %q", step.Definition.ID, br.Name)
				}
				branchNames[br.Name] = struct{}{}
				if len(br.Steps) == 0 {
					return fmt.Errorf("lebro: workflow branch %q at step %q has no steps", br.Name, step.Definition.ID)
				}
				if br.Condition == nil {
					return fmt.Errorf("lebro: workflow branch %q at step %q has nil Condition", br.Name, step.Definition.ID)
				}
				if err := validateStepTree(br.Steps, seen, hasSchema); err != nil {
					return err
				}
			}
			if step.Definition.Default != "" {
				if _, ok := branchNames[step.Definition.Default]; !ok {
					return fmt.Errorf("lebro: workflow branching step %q default %q is not a declared branch", step.Definition.ID, step.Definition.Default)
				}
			}
		} else if step.Definition.FanOut != nil {
			if step.Handler != nil && !isNilInterface(step.Handler) {
				return fmt.Errorf("lebro: workflow fan-out step %q must not declare a Handler", step.Definition.ID)
			}
			if len(step.Definition.OutputSchema) > 0 {
				return fmt.Errorf("lebro: workflow fan-out step %q must not declare OutputSchema", step.Definition.ID)
			}
			if len(step.Definition.SuspendSchema) > 0 {
				return fmt.Errorf("lebro: workflow fan-out step %q must not declare SuspendSchema", step.Definition.ID)
			}
			if step.Definition.Retry != nil {
				return fmt.Errorf("lebro: workflow fan-out step %q must not declare Retry", step.Definition.ID)
			}
			if len(step.Definition.InputSchema) > 0 {
				*hasSchema = true
			}
			fo := step.Definition.FanOut
			if len(fo.Branches) == 0 {
				return fmt.Errorf("lebro: workflow fan-out step %q has no branches", step.Definition.ID)
			}
			if fo.MaxParallel < 0 {
				return fmt.Errorf("lebro: workflow fan-out step %q MaxParallel must be >= 0", step.Definition.ID)
			}
			switch fo.FailurePolicy {
			case "", FanOutFailFast, FanOutCollectAll:
			default:
				return fmt.Errorf("lebro: workflow fan-out step %q has invalid FailurePolicy %q", step.Definition.ID, fo.FailurePolicy)
			}
			branchNames := make(map[string]struct{}, len(fo.Branches))
			for _, fb := range fo.Branches {
				if fb.Name == "" {
					return fmt.Errorf("lebro: workflow fan-out step %q has a branch with empty Name", step.Definition.ID)
				}
				if _, exists := branchNames[fb.Name]; exists {
					return fmt.Errorf("lebro: workflow fan-out step %q has duplicate branch name %q", step.Definition.ID, fb.Name)
				}
				branchNames[fb.Name] = struct{}{}
				if len(fb.Steps) == 0 {
					return fmt.Errorf("lebro: workflow fan-out branch %q at step %q has no steps", fb.Name, step.Definition.ID)
				}
				if err := validateStepTree(fb.Steps, seen, hasSchema); err != nil {
					return err
				}
			}
		} else if step.Definition.Map != nil {
			if step.Handler != nil && !isNilInterface(step.Handler) {
				return fmt.Errorf("lebro: workflow map step %q must not declare a Handler", step.Definition.ID)
			}
			if len(step.Definition.SuspendSchema) > 0 || step.Definition.Retry != nil {
				return fmt.Errorf("lebro: workflow map step %q must not declare SuspendSchema or Retry", step.Definition.ID)
			}
			if len(step.Definition.InputSchema) > 0 || len(step.Definition.OutputSchema) > 0 {
				*hasSchema = true
			}
			if step.Definition.Map.Path != "" && len(step.Definition.Map.Fields) > 0 {
				return fmt.Errorf("lebro: workflow map step %q cannot declare both Path and Fields", step.Definition.ID)
			}
			if step.Definition.Map.Path == "" && len(step.Definition.Map.Fields) == 0 {
				return fmt.Errorf("lebro: workflow map step %q has no Path or Fields", step.Definition.ID)
			}
			if path := step.Definition.Map.Path; path != "" && path != "." && (strings.HasPrefix(path, ".") || strings.HasSuffix(path, ".") || strings.Contains(path, "..")) {
				return fmt.Errorf("lebro: workflow map step %q has invalid Path %q", step.Definition.ID, path)
			}
			for key, path := range step.Definition.Map.Fields {
				if key == "" || path == "" {
					return fmt.Errorf("lebro: workflow map step %q has empty field name or path", step.Definition.ID)
				}
				if path != "." && (strings.HasPrefix(path, ".") || strings.HasSuffix(path, ".") || strings.Contains(path, "..")) {
					return fmt.Errorf("lebro: workflow map step %q field %q has invalid path %q", step.Definition.ID, key, path)
				}
			}
		} else if step.Definition.ForEach != nil {
			if step.Handler != nil && !isNilInterface(step.Handler) {
				return fmt.Errorf("lebro: workflow foreach step %q must not declare a Handler", step.Definition.ID)
			}
			if len(step.Definition.OutputSchema) > 0 || len(step.Definition.SuspendSchema) > 0 || step.Definition.Retry != nil {
				return fmt.Errorf("lebro: workflow foreach step %q must not declare OutputSchema, SuspendSchema, or Retry", step.Definition.ID)
			}
			if len(step.Definition.InputSchema) > 0 {
				*hasSchema = true
			}
			if step.Definition.ForEach.MaxParallel < 0 {
				return fmt.Errorf("lebro: workflow foreach step %q MaxParallel must be >= 0", step.Definition.ID)
			}
			if len(step.Definition.ForEach.Steps) == 0 {
				return fmt.Errorf("lebro: workflow foreach step %q has no steps", step.Definition.ID)
			}
			for _, child := range step.Definition.ForEach.Steps {
				if len(child.Definition.Branches) > 0 || child.Definition.FanOut != nil || child.Definition.Map != nil || child.Definition.ForEach != nil || child.Definition.DoWhile != nil || child.Definition.DoUntil != nil || child.Definition.Sleep != nil || child.Definition.SleepUntil != nil {
					return fmt.Errorf("lebro: workflow foreach step %q cannot contain child control-flow step %q", step.Definition.ID, child.Definition.ID)
				}
			}
			if err := validateStepTree(step.Definition.ForEach.Steps, seen, hasSchema); err != nil {
				return err
			}
		} else if step.Definition.DoWhile != nil || step.Definition.DoUntil != nil {
			if step.Handler != nil && !isNilInterface(step.Handler) {
				return fmt.Errorf("lebro: workflow loop step %q must not declare a Handler", step.Definition.ID)
			}
			if len(step.Definition.OutputSchema) > 0 || len(step.Definition.SuspendSchema) > 0 || step.Definition.Retry != nil {
				return fmt.Errorf("lebro: workflow loop step %q must not declare OutputSchema, SuspendSchema, or Retry", step.Definition.ID)
			}
			if len(step.Definition.InputSchema) > 0 {
				*hasSchema = true
			}
			var loopSteps []Step
			var maxIterations int
			var condition LoopPredicate
			if step.Definition.DoWhile != nil {
				loopSteps, maxIterations, condition = step.Definition.DoWhile.Steps, step.Definition.DoWhile.MaxIterations, step.Definition.DoWhile.Condition
			} else {
				loopSteps, maxIterations, condition = step.Definition.DoUntil.Steps, step.Definition.DoUntil.MaxIterations, step.Definition.DoUntil.Condition
			}
			if maxIterations < 1 || maxIterations > maxLoopIterations {
				return fmt.Errorf("lebro: workflow loop step %q MaxIterations must be between 1 and %d", step.Definition.ID, maxLoopIterations)
			}
			if condition == nil {
				return fmt.Errorf("lebro: workflow loop step %q has nil Condition", step.Definition.ID)
			}
			if len(loopSteps) == 0 {
				return fmt.Errorf("lebro: workflow loop step %q has no steps", step.Definition.ID)
			}
			for _, child := range loopSteps {
				if len(child.Definition.Branches) > 0 || child.Definition.FanOut != nil || child.Definition.Map != nil || child.Definition.ForEach != nil || child.Definition.DoWhile != nil || child.Definition.DoUntil != nil || child.Definition.Sleep != nil || child.Definition.SleepUntil != nil {
					return fmt.Errorf("lebro: workflow loop step %q cannot contain child control-flow step %q", step.Definition.ID, child.Definition.ID)
				}
				if len(child.Definition.SuspendSchema) > 0 {
					return fmt.Errorf("lebro: workflow loop step %q cannot contain suspendable child step %q", step.Definition.ID, child.Definition.ID)
				}
			}
			if err := validateStepTree(loopSteps, seen, hasSchema); err != nil {
				return err
			}
		} else if step.Definition.Sleep != nil || step.Definition.SleepUntil != nil {
			if step.Handler != nil && !isNilInterface(step.Handler) {
				return fmt.Errorf("lebro: workflow sleep step %q must not declare a Handler", step.Definition.ID)
			}
			if len(step.Definition.SuspendSchema) > 0 || step.Definition.Retry != nil {
				return fmt.Errorf("lebro: workflow sleep step %q must not declare SuspendSchema or Retry", step.Definition.ID)
			}
			if step.Definition.Sleep != nil && step.Definition.Sleep.Duration < 0 {
				return fmt.Errorf("lebro: workflow sleep step %q Duration must be >= 0", step.Definition.ID)
			}
			if step.Definition.SleepUntil != nil && step.Definition.SleepUntil.At.IsZero() {
				return fmt.Errorf("lebro: workflow sleep-until step %q At is required", step.Definition.ID)
			}
			if len(step.Definition.InputSchema) > 0 || len(step.Definition.OutputSchema) > 0 {
				*hasSchema = true
			}
		} else {
			if step.Handler == nil || isNilInterface(step.Handler) {
				return fmt.Errorf("lebro: workflow step %q handler is required", step.Definition.ID)
			}
			if len(step.Definition.InputSchema) > 0 || len(step.Definition.OutputSchema) > 0 || len(step.Definition.SuspendSchema) > 0 {
				*hasSchema = true
			}
		}
	}
	return nil
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
	if len(def.Branches) > 0 {
		branches := make([]Branch, len(def.Branches))
		copy(branches, def.Branches)
		def.Branches = branches
	}
	if def.FanOut != nil {
		fo := *def.FanOut
		if len(fo.Branches) > 0 {
			branches := make([]FanOutBranch, len(fo.Branches))
			copy(branches, fo.Branches)
			fo.Branches = branches
		}
		def.FanOut = &fo
	}
	if def.Map != nil {
		mapped := *def.Map
		mapped.Fields = make(map[string]string, len(def.Map.Fields))
		for k, v := range def.Map.Fields {
			mapped.Fields[k] = v
		}
		def.Map = &mapped
	}
	if def.ForEach != nil {
		foreach := *def.ForEach
		foreach.Steps = append([]Step(nil), def.ForEach.Steps...)
		def.ForEach = &foreach
	}
	if def.DoWhile != nil {
		loop := *def.DoWhile
		loop.Steps = append([]Step(nil), def.DoWhile.Steps...)
		def.DoWhile = &loop
	}
	if def.DoUntil != nil {
		loop := *def.DoUntil
		loop.Steps = append([]Step(nil), def.DoUntil.Steps...)
		def.DoUntil = &loop
	}
	if def.Sleep != nil {
		sleep := *def.Sleep
		def.Sleep = &sleep
	}
	if def.SleepUntil != nil {
		sleepUntil := *def.SleepUntil
		def.SleepUntil = &sleepUntil
	}
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
				// An empty contract publishes no pinned resume value: the step
				// declares a SuspendSchema to validate the resume input at Resume
				// but does not constrain resume to a single value. Skipping
				// validation here (rather than validating empty bytes, which no
				// schema accepts) lets a step suspend to await free-form input —
				// a human approval decision, say — while every existing caller,
				// which always publishes a non-empty contract, is unaffected.
				if len(signal.Contract) > 0 {
					if err := validateStepValue(step.suspendSchema, ValidationTargetSuspendContract, signal.Contract); err != nil {
						if attempt != 1 {
							emitter.emitStepAttemptFinished(runID, position, stepID, attempt, delay, attemptStart, err)
						}
						return nil, &retryOutcome{kind: retryInvalidOutput, cause: err}
					}
				}
				// Normalize the suspend signal's step identity to the
				// executing step so the persisted snapshot and public result
				// match the suspend boundary regardless of what the handler
				// populated. A handler that sets a mismatched StepID cannot
				// leak into durable state.
				signal.StepID = stepID
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

// fanOutBranchOutcome records the terminal result of one fan-out branch.
type fanOutBranchOutcome struct {
	output  json.RawMessage
	status  RunStatus
	failure *WorkflowError
}

// executeFanOut runs all branches of a fan-out step concurrently within the
// configured MaxParallel bound, joins their outputs in declaration order, and
// returns the joined JSON array, a FanOutJoinResult recording each branch's
// terminal state, and a *WorkflowError when the fan-out failed or was
// cancelled.
//
// Fail-fast cancels remaining and in-flight siblings after the first genuine
// child failure and waits for them to drain. Collect-all lets every branch
// finish. In both modes the primary error is the lowest declared-index
// genuine child failure (cancellation caused by sibling fail-fast is not a
// genuine failure). External context cancellation returns a cancelled error.
//
// The joined output is a JSON array of objects in declaration order:
//
//	[{"name":"a","output":...},{"name":"b","output":...}]
//
// A branch with no output (failed or cancelled) has a null output entry.
func (w *LinearWorkflow) executeFanOut(ctx context.Context, anchor runAnchor, emitter *runEmitter, runID RunID, metadata map[string]string, input json.RawMessage, position int, stepID StepID, step compiledStep, retryOverrides map[StepID]RetryPolicy, threadID ThreadID) (json.RawMessage, FanOutJoinResult, *WorkflowError) {
	fo := step.fanOut
	numBranches := len(fo.branches)

	fanCtx, cancelFan := context.WithCancelCause(ctx)
	defer cancelFan(nil)

	results := make([]fanOutBranchOutcome, numBranches)

	sem := make(chan struct{}, fo.maxParallel)
	var wg sync.WaitGroup

	for i, br := range fo.branches {
		wg.Add(1)
		go func(idx int, branch compiledFanOutBranch) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			results[idx] = w.executeFanOutBranch(fanCtx, emitter, runID, position, stepID, branch, input, retryOverrides, threadID, metadata)

			if fo.failurePolicy == FanOutFailFast && results[idx].failure != nil {
				cancelFan(fmt.Errorf("lebro: fan-out branch %q failed", branch.name))
			}
		}(i, br)
	}
	wg.Wait()

	branchResults := make([]FanOutBranchResult, numBranches)
	var primaryErr *WorkflowError
	joinStatus := RunStatusSucceeded

	for i, br := range fo.branches {
		out := results[i]
		brResult := FanOutBranchResult{
			Name:   br.name,
			Status: out.status,
		}
		if out.status == RunStatusSucceeded {
			brResult.Output = cloneRawMessage(out.output)
		} else if out.failure != nil {
			brResult.Failure = &WorkflowFailureData{
				Kind:    out.failure.Kind,
				Step:    out.failure.Step,
				StepID:  out.failure.StepID,
				Message: failureMessage(out.failure),
			}
			if joinStatus != RunStatusCancelled {
				joinStatus = RunStatusFailed
			}
		}
		branchResults[i] = brResult

		if out.failure != nil && primaryErr == nil {
			if out.failure.Kind == WorkflowErrorCancelled {
				if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
					joinStatus = RunStatusCancelled
					primaryErr = out.failure
				}
			} else {
				primaryErr = out.failure
			}
		}
	}

	if joinStatus == RunStatusCancelled && primaryErr == nil {
		primaryErr = &WorkflowError{Kind: WorkflowErrorCancelled, Step: position, StepID: stepID, Err: ctx.Err()}
	}

	joinResult := FanOutJoinResult{
		StepID:   stepID,
		Status:   joinStatus,
		Branches: branchResults,
	}

	if primaryErr != nil {
		wrappedErr := &WorkflowError{
			Kind:   primaryErr.Kind,
			Step:   position,
			StepID: primaryErr.StepID,
			Err:    fmt.Errorf("lebro: fan-out branch failed at step %q: %w", primaryErr.StepID, primaryErr.Err),
		}
		if primaryErr.Kind != WorkflowErrorCancelled && primaryErr.Kind != WorkflowErrorFanOutInputMapperFailed {
			wrappedErr.Kind = WorkflowErrorFanOutBranchFailed
		}
		return nil, joinResult, wrappedErr
	}

	joined, err := json.Marshal(buildFanOutJoinedOutput(branchResults))
	if err != nil {
		return nil, joinResult, &WorkflowError{Kind: WorkflowErrorStepFailed, Step: position, StepID: stepID, Err: fmt.Errorf("lebro: encode fan-out joined output: %w", err)}
	}

	return joined, joinResult, nil
}

// buildFanOutJoinedOutput constructs the declaration-ordered joined output
// from branch results. Each entry is an object with "name" and "output"
// fields. A failed or cancelled branch has a null output.
func buildFanOutJoinedOutput(results []FanOutBranchResult) []map[string]any {
	out := make([]map[string]any, len(results))
	for i, r := range results {
		var outputVal any
		if len(r.Output) > 0 {
			_ = json.Unmarshal(r.Output, &outputVal)
		}
		out[i] = map[string]any{
			"name":   r.Name,
			"output": outputVal,
		}
	}
	return out
}

func executeMap(path string, fields map[string]string, input json.RawMessage) (json.RawMessage, error) {
	var root any
	if err := json.Unmarshal(input, &root); err != nil {
		return nil, fmt.Errorf("lebro: decode map input: %w", err)
	}
	if path != "" {
		value, err := mapPathValue(root, path)
		if err != nil {
			return nil, fmt.Errorf("lebro: map path %q: %w", path, err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("lebro: encode map output: %w", err)
		}
		return encoded, nil
	}
	output := make(map[string]any, len(fields))
	for key, path := range fields {
		value, err := mapPathValue(root, path)
		if err != nil {
			return nil, fmt.Errorf("lebro: map field %q path %q: %w", key, path, err)
		}
		output[key] = value
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return nil, fmt.Errorf("lebro: encode map output: %w", err)
	}
	return encoded, nil
}

func mapPathValue(root any, path string) (any, error) {
	value := root
	if path == "." {
		return value, nil
	}
	for _, part := range splitMapPath(path) {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("is not an object")
		}
		var found bool
		value, found = object[part]
		if !found {
			return nil, errors.New("does not exist")
		}
	}
	return value, nil
}

func splitMapPath(path string) []string {
	var parts []string
	for _, part := range strings.Split(path, ".") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}

func (w *LinearWorkflow) executeForEach(ctx context.Context, emitter *runEmitter, runID RunID, metadata map[string]string, input json.RawMessage, position int, stepID StepID, foreach *compiledForEach, retryOverrides map[StepID]RetryPolicy, threadID ThreadID) (json.RawMessage, ForEachResult, *WorkflowError) {
	var items []json.RawMessage
	if trimmed := bytes.TrimSpace(input); len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, ForEachResult{}, &WorkflowError{Kind: WorkflowErrorInvalidStepInput, Step: position, StepID: stepID, Err: errors.New("lebro: foreach input must be a JSON array")}
	}
	if err := json.Unmarshal(input, &items); err != nil {
		return nil, ForEachResult{}, &WorkflowError{Kind: WorkflowErrorInvalidStepInput, Step: position, StepID: stepID, Err: errors.New("lebro: foreach input must be a JSON array")}
	}
	outputs := make([]json.RawMessage, len(items))
	errs := make([]*WorkflowError, len(items))
	sem := make(chan struct{}, foreach.maxParallel)
	var wg sync.WaitGroup
	for index, item := range items {
		wg.Add(1)
		go func(index int, item json.RawMessage) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errs[index] = &WorkflowError{Kind: WorkflowErrorCancelled, Step: position, StepID: stepID, Err: ctx.Err()}
				return
			}
			defer func() { <-sem }()
			current := cloneRawMessage(item)
			for _, child := range foreach.steps {
				if err := ctx.Err(); err != nil {
					errs[index] = &WorkflowError{Kind: WorkflowErrorCancelled, Step: position, StepID: stepID, Err: err}
					return
				}
				if err := validateStepValue(child.inputSchema, ValidationTargetStepInput, current); err != nil {
					errs[index] = &WorkflowError{Kind: WorkflowErrorInvalidStepInput, Step: position, StepID: child.definition.ID, Err: err}
					return
				}
				childStart := emitter.emitFanOutBranchStepStarted(runID, position, child.definition.ID, fmt.Sprintf("%d", index))
				stepCtx := withWorkflowInvocation(ctx, runID, position, child.definition.ID, threadID, metadata)
				output, outcome := w.runStepWithRetry(stepCtx, child, position, child.definition.ID, childStart, current, resolveRetryPolicy(child.retry, retryOverrides, child.definition.ID), emitter, runID)
				if outcome != nil {
					kind := WorkflowErrorStepFailed
					switch outcome.kind {
					case retryCancelled:
						kind = WorkflowErrorCancelled
					case retryPanicked:
						kind = WorkflowErrorStepPanicked
					case retryInvalidOutput, retrySuspended:
						kind = WorkflowErrorInvalidStepOutput
					}
					errs[index] = &WorkflowError{Kind: kind, Step: position, StepID: child.definition.ID, Err: outcome.cause}
					emitter.emitFanOutBranchStepFinished(runID, position, child.definition.ID, fmt.Sprintf("%d", index), childStart, errs[index])
					return
				}
				emitter.emitFanOutBranchStepFinished(runID, position, child.definition.ID, fmt.Sprintf("%d", index), childStart, nil)
				current = cloneRawMessage(output)
			}
			outputs[index] = current
		}(index, item)
	}
	wg.Wait()
	result := ForEachResult{StepID: stepID, Status: RunStatusSucceeded, Items: make([]ForEachItemResult, len(items))}
	for index, itemErr := range errs {
		result.Items[index] = ForEachItemResult{Index: index, Status: RunStatusSucceeded, Output: cloneRawMessage(outputs[index])}
		if itemErr != nil {
			result.Status = RunStatusFailed
			result.Items[index].Status = RunStatusFailed
			result.Items[index].Failure = &WorkflowFailureData{Kind: itemErr.Kind, Step: itemErr.Step, StepID: itemErr.StepID, Message: failureMessage(itemErr)}
		}
	}
	for _, itemErr := range errs {
		if itemErr != nil {
			return nil, result, itemErr
		}
	}
	output, err := json.Marshal(outputs)
	if err != nil {
		return nil, result, &WorkflowError{Kind: WorkflowErrorStepFailed, Step: position, StepID: stepID, Err: fmt.Errorf("lebro: encode foreach output: %w", err)}
	}
	return output, result, nil
}

// executeLoop runs a post-condition loop serially. Every completed body is
// checkpointed before its condition is evaluated, so durable stores retain the
// last confirmed iteration even when a later condition or cancellation fails.
// Resume only supports persisted SuspendSignal boundaries, not these running
// loop inspection checkpoints.
func (w *LinearWorkflow) executeLoop(ctx context.Context, anchor runAnchor, emitter *runEmitter, runID RunID, metadata map[string]string, input json.RawMessage, position int, stepID StepID, loop *compiledLoop, completed []json.RawMessage, retryOverrides map[StepID]RetryPolicy, threadID ThreadID) (json.RawMessage, *WorkflowError) {
	current := cloneRawMessage(input)
	for iteration := 1; iteration <= loop.maxIterations; iteration++ {
		if err := ctx.Err(); err != nil {
			return nil, &WorkflowError{Kind: WorkflowErrorCancelled, Step: position, StepID: stepID, Err: err}
		}
		emitter.emitLoopIteration(runID, position, stepID, iteration, RunEventLoopIterationStarted, nil)
		for _, child := range loop.steps {
			if err := ctx.Err(); err != nil {
				loopErr := &WorkflowError{Kind: WorkflowErrorCancelled, Step: position, StepID: child.definition.ID, Err: err}
				emitter.emitLoopIteration(runID, position, stepID, iteration, RunEventLoopIterationFinished, loopErr)
				return nil, loopErr
			}
			if err := validateStepValue(child.inputSchema, ValidationTargetStepInput, current); err != nil {
				loopErr := &WorkflowError{Kind: WorkflowErrorInvalidStepInput, Step: position, StepID: child.definition.ID, Err: err}
				emitter.emitLoopIteration(runID, position, stepID, iteration, RunEventLoopIterationFinished, loopErr)
				return nil, loopErr
			}
			childStart := emitter.emitFanOutBranchStepStarted(runID, position, child.definition.ID, fmt.Sprintf("iteration-%d", iteration))
			stepCtx := withWorkflowInvocation(ctx, runID, position, child.definition.ID, threadID, metadata)
			output, outcome := w.runStepWithRetry(stepCtx, child, position, child.definition.ID, childStart, current, resolveRetryPolicy(child.retry, retryOverrides, child.definition.ID), emitter, runID)
			if outcome != nil {
				kind := WorkflowErrorStepFailed
				switch outcome.kind {
				case retryCancelled:
					kind = WorkflowErrorCancelled
				case retryPanicked:
					kind = WorkflowErrorStepPanicked
				case retryInvalidOutput, retrySuspended:
					kind = WorkflowErrorInvalidStepOutput
				}
				loopErr := &WorkflowError{Kind: kind, Step: position, StepID: child.definition.ID, Err: outcome.cause}
				emitter.emitFanOutBranchStepFinished(runID, position, child.definition.ID, fmt.Sprintf("iteration-%d", iteration), childStart, loopErr)
				emitter.emitLoopIteration(runID, position, stepID, iteration, RunEventLoopIterationFinished, loopErr)
				return nil, loopErr
			}
			emitter.emitFanOutBranchStepFinished(runID, position, child.definition.ID, fmt.Sprintf("iteration-%d", iteration), childStart, nil)
			current = cloneRawMessage(output)
		}
		if err := w.persistLoopCheckpoint(ctx, anchor, position, stepID, iteration, current, completed); err != nil {
			loopErr := &WorkflowError{Kind: WorkflowErrorStepFailed, Step: position, StepID: stepID, Err: fmt.Errorf("lebro: persist workflow loop checkpoint %q iteration %d: %w", stepID, iteration, err)}
			emitter.emitLoopIteration(runID, position, stepID, iteration, RunEventLoopIterationFinished, loopErr)
			return nil, loopErr
		}
		if err := ctx.Err(); err != nil {
			loopErr := &WorkflowError{Kind: WorkflowErrorCancelled, Step: position, StepID: stepID, Err: err}
			emitter.emitLoopIteration(runID, position, stepID, iteration, RunEventLoopIterationFinished, loopErr)
			return nil, loopErr
		}
		matched, err := loop.condition(ctx, cloneRawMessage(current), iteration)
		if err != nil {
			loopErr := &WorkflowError{Kind: WorkflowErrorStepFailed, Step: position, StepID: stepID, Err: fmt.Errorf("lebro: loop condition failed: %w", err)}
			emitter.emitLoopIteration(runID, position, stepID, iteration, RunEventLoopIterationFinished, loopErr)
			return nil, loopErr
		}
		if err := ctx.Err(); err != nil {
			loopErr := &WorkflowError{Kind: WorkflowErrorCancelled, Step: position, StepID: stepID, Err: err}
			emitter.emitLoopIteration(runID, position, stepID, iteration, RunEventLoopIterationFinished, loopErr)
			return nil, loopErr
		}
		continueLoop := matched
		if loop.mode == loopUntil {
			continueLoop = !matched
		}
		if !continueLoop {
			emitter.emitLoopIteration(runID, position, stepID, iteration, RunEventLoopIterationFinished, nil)
			return current, nil
		}
		if iteration == loop.maxIterations {
			loopErr := &WorkflowError{Kind: WorkflowErrorMaxIterations, Step: position, StepID: stepID, Err: fmt.Errorf("%w: step %q (%d)", ErrWorkflowMaxIterations, stepID, loop.maxIterations)}
			emitter.emitLoopIteration(runID, position, stepID, iteration, RunEventLoopIterationFinished, loopErr)
			return nil, loopErr
		}
		emitter.emitLoopIteration(runID, position, stepID, iteration, RunEventLoopIterationFinished, nil)
	}
	return nil, &WorkflowError{Kind: WorkflowErrorMaxIterations, Step: position, StepID: stepID, Err: ErrWorkflowMaxIterations}
}

func (w *LinearWorkflow) persistLoopCheckpoint(ctx context.Context, anchor runAnchor, position int, stepID StepID, iteration int, output json.RawMessage, completed []json.RawMessage) error {
	if w.store == nil || isNilInterface(w.store) {
		return nil
	}
	stepOutputs := append(cloneRawOutputs(completed), cloneRawMessage(output))
	state, err := json.Marshal(workflowSnapshotEnvelope{Step: position, StepID: stepID, Output: cloneRawMessage(output), Outputs: stepOutputs, Loops: []loopCheckpoint{{StepID: stepID, Iterations: iteration, Output: cloneRawMessage(output)}}})
	if err != nil {
		return fmt.Errorf("lebro: encode workflow loop checkpoint: %w", err)
	}
	now := w.clock.Now()
	snapshot := WorkflowSnapshotRecord{ID: fmt.Sprintf("%s-snapshot-%d-loop-%d", anchor.runID, position, iteration), RunID: anchor.runID, Sequence: workflowSnapshotSequence(position, iteration), SchemaVersion: workflowSnapshotSchemaVersion, State: state, CreatedAt: now}
	updated := w.baseRunRecord(anchor)
	updated.Status, updated.CurrentStep, updated.CurrentStepID, updated.StepOutputs, updated.UpdatedAt = RunStatusRunning, position, stepID, stepOutputs, now
	return w.store.Transaction(ctx, func(ctx context.Context, repos Repositories) error {
		if err := repos.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, snapshot); err != nil {
			return err
		}
		return repos.WorkflowRuns().SaveWorkflowRun(ctx, updated)
	})
}

// executeFanOutBranch runs one fan-out branch's steps sequentially and returns
// the terminal outcome. It validates each step's input and output, runs the
// handler with retry, and emits step_started/step_finished events with the
// branch name set on the Branch field. A SuspendError from a child step is
// treated as an invalid output since concurrent suspend contracts are not
// supported within fan-out.
func (w *LinearWorkflow) executeFanOutBranch(ctx context.Context, emitter *runEmitter, runID RunID, position int, fanOutStepID StepID, branch compiledFanOutBranch, input json.RawMessage, retryOverrides map[StepID]RetryPolicy, threadID ThreadID, metadata map[string]string) fanOutBranchOutcome {
	// Check context before invoking input mapper to avoid unnecessary work
	// after fail-fast or external cancellation.
	if err := ctx.Err(); err != nil {
		return fanOutBranchOutcome{
			status: RunStatusCancelled,
			failure: &WorkflowError{
				Kind:   WorkflowErrorCancelled,
				Step:   position,
				StepID: fanOutStepID,
				Err:    err,
			},
		}
	}

	branchInput := cloneRawMessage(input)
	if branch.inputMapper != nil {
		mapped, err := branch.inputMapper(ctx, cloneRawMessage(input))
		if err != nil {
			return fanOutBranchOutcome{
				status: RunStatusFailed,
				failure: &WorkflowError{
					Kind:   WorkflowErrorFanOutInputMapperFailed,
					Step:   position,
					StepID: fanOutStepID,
					Err:    fmt.Errorf("lebro: fan-out branch %q input mapper failed: %w", branch.name, err),
				},
			}
		}
		branchInput = cloneRawMessage(mapped)
	}

	current := branchInput
	remaining := branch.steps
	for len(remaining) > 0 {
		bs := remaining[0]
		remaining = remaining[1:]
		childStepID := bs.definition.ID

		if err := ctx.Err(); err != nil {
			return fanOutBranchOutcome{
				status: RunStatusCancelled,
				failure: &WorkflowError{
					Kind:   WorkflowErrorCancelled,
					Step:   position,
					StepID: childStepID,
					Err:    err,
				},
			}
		}

		stepStart := emitter.emitFanOutBranchStepStarted(runID, position, childStepID, branch.name)

		if len(bs.branches) > 0 {
			if err := validateStepValue(bs.inputSchema, ValidationTargetStepInput, current); err != nil {
				stepErr := &WorkflowError{Kind: WorkflowErrorInvalidBranchInput, Step: position, StepID: childStepID, Err: err}
				emitter.emitFanOutBranchStepFinished(runID, position, childStepID, branch.name, stepStart, stepErr)
				return fanOutBranchOutcome{status: RunStatusFailed, failure: stepErr}
			}

			selected, matchErr := w.selectBranch(ctx, bs, current)
			if matchErr != nil {
				kind := WorkflowErrorBranchConditionFailed
				if errors.Is(matchErr, ErrWorkflowNoBranchMatched) {
					kind = WorkflowErrorNoBranchMatched
				}
				stepErr := &WorkflowError{Kind: kind, Step: position, StepID: childStepID, Err: matchErr}
				emitter.emitFanOutBranchStepFinished(runID, position, childStepID, branch.name, stepStart, stepErr)
				return fanOutBranchOutcome{status: RunStatusFailed, failure: stepErr}
			}

			emitter.emitBranchSelected(runID, position, childStepID, selected.name)
			emitter.emitFanOutBranchStepFinished(runID, position, childStepID, branch.name, stepStart, nil)
			remaining = append(selected.steps, remaining...)
			continue
		}

		if err := validateStepValue(bs.inputSchema, ValidationTargetStepInput, current); err != nil {
			stepErr := &WorkflowError{Kind: WorkflowErrorInvalidStepInput, Step: position, StepID: childStepID, Err: err}
			emitter.emitFanOutBranchStepFinished(runID, position, childStepID, branch.name, stepStart, stepErr)
			return fanOutBranchOutcome{status: RunStatusFailed, failure: stepErr}
		}

		stepCtx := withWorkflowInvocation(ctx, runID, position, childStepID, threadID, metadata)
		policy := resolveRetryPolicy(bs.retry, retryOverrides, childStepID)

		output, outcome := w.runStepWithRetry(stepCtx, bs, position, childStepID, stepStart, current, policy, emitter, runID)

		if outcome != nil {
			var stepErr *WorkflowError
			switch outcome.kind {
			case retrySuspended:
				stepErr = &WorkflowError{
					Kind:   WorkflowErrorInvalidStepOutput,
					Step:   position,
					StepID: childStepID,
					Err:    fmt.Errorf("lebro: fan-out branch %q step %q signalled suspend, which is not supported within fan-out", branch.name, childStepID),
				}
			case retryCancelled:
				stepErr = &WorkflowError{Kind: WorkflowErrorCancelled, Step: position, StepID: childStepID, Err: outcome.cause}
				emitter.emitFanOutBranchStepFinished(runID, position, childStepID, branch.name, stepStart, stepErr)
				return fanOutBranchOutcome{status: RunStatusCancelled, failure: stepErr}
			case retryPanicked:
				stepErr = &WorkflowError{Kind: WorkflowErrorStepPanicked, Step: position, StepID: childStepID, Err: outcome.cause}
			case retryInvalidOutput:
				stepErr = &WorkflowError{Kind: WorkflowErrorInvalidStepOutput, Step: position, StepID: childStepID, Err: outcome.cause}
			default:
				stepErr = &WorkflowError{Kind: WorkflowErrorStepFailed, Step: position, StepID: childStepID, Err: outcome.cause}
			}
			emitter.emitFanOutBranchStepFinished(runID, position, childStepID, branch.name, stepStart, stepErr)
			return fanOutBranchOutcome{status: RunStatusFailed, failure: stepErr}
		}

		emitter.emitFanOutBranchStepFinished(runID, position, childStepID, branch.name, stepStart, nil)
		current = cloneRawMessage(output)
	}

	return fanOutBranchOutcome{output: current, status: RunStatusSucceeded}
}
