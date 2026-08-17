package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// RunEventType identifies a lifecycle event emitted during an agent run.
type RunEventType string

const (
	// RunEventStarted is emitted when a run begins, before the first model
	// call.
	RunEventStarted RunEventType = "run_started"
	// RunEventModelStarted is emitted before each model Generate call.
	RunEventModelStarted RunEventType = "model_started"
	// RunEventModelFinished is emitted after a model Generate call returns,
	// carrying usage and finish reason.
	RunEventModelFinished RunEventType = "model_finished"
	// RunEventDelta is emitted for each text, tool-call, or structured-output
	// delta produced by a streaming model call. Multiple deltas may be emitted
	// between RunEventModelStarted and RunEventModelFinished for a single call.
	RunEventDelta RunEventType = "model_delta"
	// RunEventToolRequested is emitted when the model requests tool calls,
	// once per individual tool call.
	RunEventToolRequested RunEventType = "tool_requested"
	// RunEventToolStarted is emitted before a tool handler is invoked.
	RunEventToolStarted RunEventType = "tool_started"
	// RunEventToolFinished is emitted after a tool handler returns, carrying
	// the execution state.
	RunEventToolFinished RunEventType = "tool_finished"
	// RunEventStepStarted is emitted before a workflow step's first attempt
	// is invoked. One per step; per-attempt lifecycle uses
	// RunEventStepAttemptStarted/Finished.
	RunEventStepStarted RunEventType = "step_started"
	// RunEventStepFinished is emitted after a workflow step reaches a terminal
	// outcome (success, failure, or rejection by input validation before any
	// attempt ran). A non-nil Error distinguishes failures from success.
	RunEventStepFinished RunEventType = "step_finished"
	// RunEventStepAttemptStarted is emitted before each step handler attempt
	// past the first. The first attempt is bracketed by RunEventStepStarted;
	// retries use this event and RunEventStepAttemptFinished. Attempt is the
	// 1-indexed attempt number and Delay is the backoff waited before it.
	RunEventStepAttemptStarted RunEventType = "step_attempt_started"
	// RunEventStepAttemptFinished is emitted after each step handler attempt
	// past the first. Attempt matches the paired started event and Error
	// carries the attempt's failure (nil on success).
	RunEventStepAttemptFinished RunEventType = "step_attempt_finished"
	// RunEventSucceeded is the terminal event for a successful run.
	RunEventSucceeded RunEventType = "run_succeeded"
	// RunEventFailed is the terminal event for a failed run.
	RunEventFailed RunEventType = "run_failed"
	// RunEventCancelled is the terminal event for a cancelled run.
	RunEventCancelled RunEventType = "run_cancelled"
	// RunEventSuspended is emitted when a run suspends at a step boundary.
	// It is non-terminal: a subsequent Resume emits RunEventResumed and the
	// run continues to a terminal succeeded/failed/cancelled event.
	RunEventSuspended RunEventType = "run_suspended"
	// RunEventResumed is emitted when a previously suspended run resumes from
	// its durable snapshot. It is followed by per-step step_started/finished
	// events for the remaining steps.
	RunEventResumed RunEventType = "run_resumed"
	// RunEventBranchSelected is emitted when a branching step evaluates its
	// predicates and selects a branch. It carries the selected branch name
	// in the DeltaText field and the branching step's StepID. It is
	// non-terminal and sits between step_started and step_finished for the
	// branching step.
	RunEventBranchSelected RunEventType = "branch_selected"
	// RunEventRouteSelected records a Network router decision. ToolID is the
	// selected specialist and DeltaText contains its JSON-encoded candidates.
	RunEventRouteSelected RunEventType = "route_selected"
	// RunEventLoopIterationStarted and RunEventLoopIterationFinished bracket
	// each post-condition loop body execution. Iteration is 1-indexed.
	RunEventLoopIterationStarted  RunEventType = "loop_iteration_started"
	RunEventLoopIterationFinished RunEventType = "loop_iteration_finished"
	// RunEventModelAttemptStarted is emitted before each provider attempt
	// when using a ModelRouter. It carries the Provider ID and model name.
	RunEventModelAttemptStarted RunEventType = "model_attempt_started"
	// RunEventModelAttemptFinished is emitted after each provider attempt
	// when using a ModelRouter. It carries the Provider ID, model name,
	// attempt status, and any error.
	RunEventModelAttemptFinished RunEventType = "model_attempt_finished"
	// RunEventProcessor records a processor decision without recording content.
	RunEventProcessor RunEventType = "processor"
)

// IsTerminal reports whether the event type is a terminal run event.
func (t RunEventType) IsTerminal() bool {
	switch t {
	case RunEventSucceeded, RunEventFailed, RunEventCancelled:
		return true
	default:
		return false
	}
}

// IsSuspended reports whether the event type marks a run-level suspension or
// resumption boundary. These events are non-terminal.
func (t RunEventType) IsSuspended() bool {
	switch t {
	case RunEventSuspended, RunEventResumed:
		return true
	default:
		return false
	}
}

// RunEvent records one ordered lifecycle event during an agent or workflow
// run. The Sequence field is 1-indexed and monotonic within a single run. Step
// is the 1-indexed step within the run (0 for run-level events). Duration is
// the elapsed time since the paired start event; it is zero for events without
// a paired start. Error is non-nil for events that report a failure or
// cancellation, including non-terminal model and tool failures as well as
// terminal events. ParentRunID, ParentStepID, and ParentStep identify the
// workflow invocation that started a nested run; they are zero values for a
// top-level run.
//
// Attempt and Delay carry retry-attempt metadata for
// RunEventStepAttemptStarted/Finished events only. Attempt is the 1-indexed
// retry attempt (2 for the first retry). Delay is the backoff waited before
// the attempt began. Both are zero on other event types.
type RunEvent struct {
	Sequence              int
	Type                  RunEventType
	RunID                 RunID
	ParentRunID           RunID
	ParentStepID          StepID
	ParentStep            int
	StepID                StepID
	Step                  int
	Attempt               int
	Iteration             int
	Delay                 time.Duration
	Timestamp             time.Time
	Duration              time.Duration
	FinishReason          FinishReason
	Usage                 ModelUsage
	ToolCallID            string
	ToolID                ToolID
	ToolState             ToolExecutionState
	DeltaText             string
	DeltaStructuredOutput ModelStructuredOutput
	Branch                string
	Provider              ProviderID
	ProviderModel         string
	AttemptStatus         ModelAttemptStatus
	ProcessorPhase        ProcessorPhase
	ProcessorAction       ProcessorDecisionKind
	Status                RunStatus
	Error                 error
}

// RunListener receives ordered run events. Implementations must be safe for
// concurrent use when an agent is shared across goroutines. A nil listener
// disables event recording entirely and must not alter agent behavior.
type RunListener interface {
	OnRunEvent(event RunEvent)
}

// Clock supplies timestamps for run events.
type Clock interface {
	Now() time.Time
}

// IDSource generates stable run and step identifiers. Implementations must be
// safe for concurrent use.
type IDSource interface {
	NewRunID() RunID
	NewStepID() StepID
}

// defaultClock uses time.Now.
type defaultClock struct{}

func (defaultClock) Now() time.Time { return time.Now() }

// sequentialIDSource generates monotonic run and step IDs using a mutex. Run
// IDs are formatted as "agent-run-NNNN" and step IDs as "step-NNN" to stay
// stable across runs with the same sequence.
type sequentialIDSource struct {
	mu      sync.Mutex
	runSeq  int
	stepSeq int
}

func (s *sequentialIDSource) NewRunID() RunID {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runSeq++
	return RunID(fmt.Sprintf("agent-run-%04d", s.runSeq))
}

func (s *sequentialIDSource) NewStepID() StepID {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stepSeq++
	return StepID(fmt.Sprintf("step-%03d", s.stepSeq))
}

// RunRecorder collects run events into an ordered slice. It is safe for
// concurrent use and does not require an observability backend, making it
// suitable for tests, local development, and programmatic inspection.
type RunRecorder struct {
	mu     sync.Mutex
	events []RunEvent
}

// NewRunRecorder creates an empty recorder ready to receive events.
func NewRunRecorder() *RunRecorder {
	return &RunRecorder{}
}

// OnRunEvent appends a copy of the event to the recorder.
func (r *RunRecorder) OnRunEvent(event RunEvent) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

// Events returns a caller-owned copy of all recorded events in sequence order.
func (r *RunRecorder) Events() []RunEvent {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RunEvent(nil), r.events...)
}

// EventCount returns the number of recorded events.
func (r *RunRecorder) EventCount() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// TerminalEvent returns the last terminal event recorded, or false if no
// terminal event has been recorded yet.
func (r *RunRecorder) TerminalEvent() (RunEvent, bool) {
	if r == nil {
		return RunEvent{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.events) - 1; i >= 0; i-- {
		if r.events[i].Type.IsTerminal() {
			return r.events[i], true
		}
	}
	return RunEvent{}, false
}

// runEmitter manages the event sequence for a single agent run and dispatches
// events to a listener. When the listener is nil, all emit calls and clock
// reads are no-ops, ensuring recording does not alter agent behavior even
// with a stateful or blocking Clock.
type runEmitter struct {
	listener   RunListener
	clock      Clock
	parentRun  RunID
	parentStep StepID
	parentPos  int
	seq        int
	mu         sync.Mutex
}

func newRunEmitter(ctx context.Context, listener RunListener, clock Clock, _ IDSource) *runEmitter {
	parent := workflowInvocationFromContext(ctx)
	return &runEmitter{listener: listener, clock: clock, parentRun: parent.runID, parentStep: parent.stepID, parentPos: parent.step}
}

func (e *runEmitter) dispatch(event RunEvent) {
	e.mu.Lock()
	e.seq++
	event.Sequence = e.seq
	event.ParentRunID = e.parentRun
	event.ParentStepID = e.parentStep
	event.ParentStep = e.parentPos
	e.mu.Unlock()
	e.listener.OnRunEvent(event)
}

// enabled reports whether the emitter will dispatch events. When false, all
// emit calls and clock reads are skipped.
func (e *runEmitter) enabled() bool {
	return e != nil && e.listener != nil
}

// now returns the current clock time, or the zero value when the emitter is
// disabled. This prevents Clock side-effects when recording is off.
func (e *runEmitter) now() time.Time {
	if !e.enabled() {
		return time.Time{}
	}
	return e.clock.Now()
}

func (e *runEmitter) emit(runID RunID, step int, stepID StepID, eventType RunEventType) {
	if !e.enabled() {
		return
	}
	e.dispatch(RunEvent{
		Type:      eventType,
		RunID:     runID,
		StepID:    stepID,
		Step:      step,
		Timestamp: e.clock.Now(),
	})
}

func (e *runEmitter) emitModelStarted(runID RunID, step int, stepID StepID) time.Time {
	ts := e.now()
	if !e.enabled() {
		return ts
	}
	e.dispatch(RunEvent{
		Type:      RunEventModelStarted,
		RunID:     runID,
		StepID:    stepID,
		Step:      step,
		Timestamp: ts,
	})
	return ts
}

func (e *runEmitter) emitDelta(runID RunID, step int, stepID StepID, delta StreamDelta) {
	if !e.enabled() {
		return
	}
	toolCallID := ""
	var toolID ToolID
	if delta.ToolCall != nil {
		toolCallID = delta.ToolCall.ID
		toolID = delta.ToolCall.ToolID
	}
	e.dispatch(RunEvent{
		Type:                  RunEventDelta,
		RunID:                 runID,
		StepID:                stepID,
		Step:                  step,
		Timestamp:             e.clock.Now(),
		DeltaText:             delta.Text,
		DeltaStructuredOutput: delta.StructuredOutput,
		ToolCallID:            toolCallID,
		ToolID:                toolID,
		FinishReason:          delta.FinishReason,
		Usage:                 delta.Usage,
		Error:                 delta.Err,
	})
}

func (e *runEmitter) emitProcessor(runID RunID, step int, stepID StepID, phase ProcessorPhase, action ProcessorDecisionKind) {
	if !e.enabled() {
		return
	}
	e.dispatch(RunEvent{Type: RunEventProcessor, RunID: runID, StepID: stepID, Step: step, Timestamp: e.clock.Now(), ProcessorPhase: phase, ProcessorAction: action})
}

func (e *runEmitter) emitModelFinished(runID RunID, step int, stepID StepID, start time.Time, finishReason FinishReason, usage ModelUsage, err error) {
	if !e.enabled() {
		return
	}
	now := e.clock.Now()
	e.dispatch(RunEvent{
		Type:         RunEventModelFinished,
		RunID:        runID,
		StepID:       stepID,
		Step:         step,
		Timestamp:    now,
		Duration:     now.Sub(start),
		FinishReason: finishReason,
		Usage:        usage,
		Error:        err,
	})
}

func (e *runEmitter) emitToolRequested(runID RunID, step int, stepID StepID, toolCallID string, toolID ToolID) {
	if !e.enabled() {
		return
	}
	e.dispatch(RunEvent{
		Type:       RunEventToolRequested,
		RunID:      runID,
		StepID:     stepID,
		Step:       step,
		Timestamp:  e.clock.Now(),
		ToolCallID: toolCallID,
		ToolID:     toolID,
	})
}

func (e *runEmitter) emitToolStarted(runID RunID, step int, stepID StepID, toolCallID string, toolID ToolID) time.Time {
	ts := e.now()
	if !e.enabled() {
		return ts
	}
	e.dispatch(RunEvent{
		Type:       RunEventToolStarted,
		RunID:      runID,
		StepID:     stepID,
		Step:       step,
		Timestamp:  ts,
		ToolCallID: toolCallID,
		ToolID:     toolID,
	})
	return ts
}

func (e *runEmitter) emitToolFinished(runID RunID, step int, stepID StepID, start time.Time, toolCallID string, toolID ToolID, toolState ToolExecutionState, err error) {
	if !e.enabled() {
		return
	}
	now := e.clock.Now()
	e.dispatch(RunEvent{
		Type:       RunEventToolFinished,
		RunID:      runID,
		StepID:     stepID,
		Step:       step,
		Timestamp:  now,
		Duration:   now.Sub(start),
		ToolCallID: toolCallID,
		ToolID:     toolID,
		ToolState:  toolState,
		Error:      err,
	})
}

func (e *runEmitter) emitStepStarted(runID RunID, step int, stepID StepID) time.Time {
	ts := e.now()
	if !e.enabled() {
		return ts
	}
	e.dispatch(RunEvent{
		Type:      RunEventStepStarted,
		RunID:     runID,
		StepID:    stepID,
		Step:      step,
		Timestamp: ts,
	})
	return ts
}

func (e *runEmitter) emitStepFinished(runID RunID, step int, stepID StepID, start time.Time, err error) {
	if !e.enabled() {
		return
	}
	now := e.clock.Now()
	e.dispatch(RunEvent{
		Type:      RunEventStepFinished,
		RunID:     runID,
		StepID:    stepID,
		Step:      step,
		Timestamp: now,
		Duration:  now.Sub(start),
		Error:     err,
	})
}

// emitStepAttemptStarted records the beginning of a retry attempt for a step.
// attempt is the 1-indexed attempt number (>= 2 for retries). delay is the
// backoff waited before this attempt.
func (e *runEmitter) emitStepAttemptStarted(runID RunID, step int, stepID StepID, attempt int, delay time.Duration) time.Time {
	ts := e.now()
	if !e.enabled() {
		return ts
	}
	e.dispatch(RunEvent{
		Type:      RunEventStepAttemptStarted,
		RunID:     runID,
		StepID:    stepID,
		Step:      step,
		Attempt:   attempt,
		Delay:     delay,
		Timestamp: ts,
	})
	return ts
}

// emitStepAttemptFinished records the end of a retry attempt for a step.
// delay is the same backoff waited before the attempt and is mirrored onto
// the event so consumers can read the configured delay from either lifecycle
// event (started or finished) without cross-referencing.
func (e *runEmitter) emitStepAttemptFinished(runID RunID, step int, stepID StepID, attempt int, delay time.Duration, start time.Time, err error) {
	if !e.enabled() {
		return
	}
	now := e.clock.Now()
	e.dispatch(RunEvent{
		Type:      RunEventStepAttemptFinished,
		RunID:     runID,
		StepID:    stepID,
		Step:      step,
		Attempt:   attempt,
		Delay:     delay,
		Timestamp: now,
		Duration:  now.Sub(start),
		Error:     err,
	})
}

func (e *runEmitter) terminal(runID RunID, step int, stepID StepID, eventType RunEventType, status RunStatus, err error) {
	if !e.enabled() {
		return
	}
	e.dispatch(RunEvent{
		Type:      eventType,
		RunID:     runID,
		StepID:    stepID,
		Step:      step,
		Timestamp: e.clock.Now(),
		Status:    status,
		Error:     err,
	})
}

// emitSuspended records a run_suspended event at the suspend boundary. The
// step is the 1-indexed position of the suspending step and stepID is its
// declared identifier.
func (e *runEmitter) emitSuspended(runID RunID, step int, stepID StepID) {
	if !e.enabled() {
		return
	}
	e.dispatch(RunEvent{
		Type:      RunEventSuspended,
		RunID:     runID,
		StepID:    stepID,
		Step:      step,
		Timestamp: e.clock.Now(),
		Status:    RunStatusSuspended,
	})
}

// emitResumed records a run_resumed event at the start of Resume. The step is
// the 1-indexed position of the next step to execute (envelope.Step + 1) and
// stepID is its declared identifier ("" when resuming past the last step,
// which only happens when the workflow had no remaining steps at suspend —
// Resume treats that as an immediate terminal success).
func (e *runEmitter) emitResumed(runID RunID, step int, stepID StepID) {
	if !e.enabled() {
		return
	}
	e.dispatch(RunEvent{
		Type:      RunEventResumed,
		RunID:     runID,
		StepID:    stepID,
		Step:      step,
		Timestamp: e.clock.Now(),
		Status:    RunStatusRunning,
	})
}

// emitBranchSelected records a branch_selected event at a branching step.
// step is the 1-indexed position of the branching step, stepID is its
// declared identifier, and branch is the selected branch name. The event is
// non-terminal and sits between step_started and step_finished for the
// branching step.
func (e *runEmitter) emitBranchSelected(runID RunID, step int, stepID StepID, branch string) {
	if !e.enabled() {
		return
	}
	e.dispatch(RunEvent{
		Type:      RunEventBranchSelected,
		RunID:     runID,
		StepID:    stepID,
		Step:      step,
		Branch:    branch,
		Timestamp: e.clock.Now(),
	})
}

func (e *runEmitter) emitRouteSelected(runID RunID, step int, selected ToolID, candidates []ToolID) {
	if !e.enabled() {
		return
	}
	raw, _ := json.Marshal(candidates)
	e.dispatch(RunEvent{Type: RunEventRouteSelected, RunID: runID, Step: step, Timestamp: e.clock.Now(), ToolID: selected, DeltaText: string(raw)})
}

func (e *runEmitter) emitLoopIteration(runID RunID, step int, stepID StepID, iteration int, eventType RunEventType, err error) {
	if !e.enabled() {
		return
	}
	e.dispatch(RunEvent{Type: eventType, RunID: runID, StepID: stepID, Step: step, Iteration: iteration, Timestamp: e.clock.Now(), Error: err})
}

// emitModelAttemptStarted records a model_attempt_started event before each
// provider attempt when using a ModelRouter.
func (e *runEmitter) emitModelAttemptStarted(runID RunID, step int, stepID StepID, provider ProviderID, model string) time.Time {
	ts := e.now()
	if !e.enabled() {
		return ts
	}
	e.dispatch(RunEvent{
		Type:          RunEventModelAttemptStarted,
		RunID:         runID,
		StepID:        stepID,
		Step:          step,
		Provider:      provider,
		ProviderModel: model,
		Timestamp:     ts,
	})
	return ts
}

// emitModelAttemptFinished records a model_attempt_finished event after each
// provider attempt when using a ModelRouter.
func (e *runEmitter) emitModelAttemptFinished(runID RunID, step int, stepID StepID, provider ProviderID, model string, status ModelAttemptStatus, start time.Time, err error) {
	if !e.enabled() {
		return
	}
	now := e.clock.Now()
	e.dispatch(RunEvent{
		Type:          RunEventModelAttemptFinished,
		RunID:         runID,
		StepID:        stepID,
		Step:          step,
		Provider:      provider,
		ProviderModel: model,
		AttemptStatus: status,
		Timestamp:     now,
		Duration:      now.Sub(start),
		Error:         err,
	})
}

// deterministic tests.
type fixedClock struct {
	t time.Time
}

// NewFixedClock creates a Clock that always returns t.
func NewFixedClock(t time.Time) Clock {
	return fixedClock{t: t}
}

func (c fixedClock) Now() time.Time { return c.t }

// emitFanOutBranchStepStarted records a step_started event for a child step
// within a fan-out branch. The Branch field carries the fan-out branch name so
// consumers can correlate concurrent child events to their branch.
func (e *runEmitter) emitFanOutBranchStepStarted(runID RunID, step int, stepID StepID, branch string) time.Time {
	ts := e.now()
	if !e.enabled() {
		return ts
	}
	e.dispatch(RunEvent{
		Type:      RunEventStepStarted,
		RunID:     runID,
		StepID:    stepID,
		Step:      step,
		Branch:    branch,
		Timestamp: ts,
	})
	return ts
}

// emitFanOutBranchStepFinished records a step_finished event for a child step
// within a fan-out branch. The Branch field carries the fan-out branch name.
func (e *runEmitter) emitFanOutBranchStepFinished(runID RunID, step int, stepID StepID, branch string, start time.Time, err error) {
	if !e.enabled() {
		return
	}
	now := e.clock.Now()
	e.dispatch(RunEvent{
		Type:      RunEventStepFinished,
		RunID:     runID,
		StepID:    stepID,
		Step:      step,
		Branch:    branch,
		Timestamp: now,
		Duration:  now.Sub(start),
		Error:     err,
	})
}

// fixedIDSource returns predetermined IDs in order. It is intended for
// deterministic tests.
type fixedIDSource struct {
	runIDs  []RunID
	stepIDs []StepID
	runIdx  int
	stepIdx int
	mu      sync.Mutex
}

// NewFixedIDSource creates an IDSource that returns the given run and step IDs
// in order. If a source is exhausted, subsequent calls return the last ID.
// Both input slices are copied so subsequent caller mutation does not affect
// the source.
func NewFixedIDSource(runIDs []RunID, stepIDs []StepID) IDSource {
	return &fixedIDSource{
		runIDs:  append([]RunID(nil), runIDs...),
		stepIDs: append([]StepID(nil), stepIDs...),
	}
}

func (s *fixedIDSource) NewRunID() RunID {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runIdx < len(s.runIDs) {
		id := s.runIDs[s.runIdx]
		s.runIdx++
		return id
	}
	if len(s.runIDs) > 0 {
		return s.runIDs[len(s.runIDs)-1]
	}
	return RunID("fixed-run")
}

func (s *fixedIDSource) NewStepID() StepID {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stepIdx < len(s.stepIDs) {
		id := s.stepIDs[s.stepIdx]
		s.stepIdx++
		return id
	}
	if len(s.stepIDs) > 0 {
		return s.stepIDs[len(s.stepIDs)-1]
	}
	return StepID("fixed-step")
}
