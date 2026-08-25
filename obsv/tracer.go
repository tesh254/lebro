package obsv

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/tesh254/lebro"
)

// DefaultDeltaLimit bounds how many streaming deltas are retained as events on
// a single model span. A streaming run emits one delta per token, so an
// unbounded span would grow with the response length; past the limit, deltas
// are counted but not retained.
const DefaultDeltaLimit = 64

// SpanSink receives spans as they end. The tracer holds its internal lock while
// calling a sink, so an implementation must not call back into the tracer.
type SpanSink interface {
	OnSpanEnd(Span)
}

// SpanSinkFunc adapts a function to SpanSink.
type SpanSinkFunc func(Span)

// OnSpanEnd calls f.
func (f SpanSinkFunc) OnSpanEnd(span Span) { f(span) }

// IDGenerator produces trace and span identifiers. Implementations must be safe
// for concurrent use. Inject a deterministic generator in tests.
type IDGenerator interface {
	NewTraceID() TraceID
	NewSpanID() SpanID
}

// TracerConfig configures a Tracer.
type TracerConfig struct {
	// Sink receives every span as it ends. Required.
	Sink SpanSink
	// IDs generates trace and span identifiers. When nil, a sequential
	// generator is used.
	IDs IDGenerator
	// DeltaLimit bounds retained streaming-delta events per model span. Zero
	// selects DefaultDeltaLimit; a negative value retains none.
	DeltaLimit int
}

// Tracer converts ordered run events into spans. It implements
// lebro.RunListener, so it attaches wherever a listener already does, and it is
// safe for concurrent use across runs sharing one agent.
//
// The zero value is not usable; construct one with NewTracer.
type Tracer struct {
	sink       SpanSink
	ids        IDGenerator
	deltaLimit int

	mu sync.Mutex
	// runs maps a run to its in-flight state. A run is registered on its first
	// event and removed when its terminal or suspend event arrives.
	runs map[runKey]*runTrace
	// stepTraces maps a step of a run, addressed the way a nested run sees it,
	// to that run's trace and the step's span. It outlives the run entry so a
	// nested run whose events arrive after its parent finished still links.
	stepTraces map[stepKey]stepAnchor
	// anchorOrder records stepTraces keys in insertion order so the oldest can
	// be pruned once the bound is reached.
	anchorOrder []stepKey
}

// anchorLimit bounds retained step anchors. A long-lived process runs unbounded
// workflows, so the map must not grow with the process's lifetime; the bound is
// far above the number of steps whose nested runs could still be in flight.
const anchorLimit = 8192

// stepAnchor is the trace and span a nested run attaches to.
type stepAnchor struct {
	trace TraceID
	span  SpanID
}

// runKey identifies one run occurrence.
//
// A RunID alone is not sufficient. Each primitive mints IDs from its own
// IDSource, so a workflow and the agent it invokes both start at "agent-run-0001"
// unless the application injects distinct sources. Keying on the invocation that
// started the run — which the runtime reports on every event — separates them:
// a nested run always carries a parent, and a top-level run never does.
type runKey struct {
	run        lebro.RunID
	parentRun  lebro.RunID
	parentStep lebro.StepID
	parentPos  int
}

func keyFor(event lebro.RunEvent) runKey {
	return runKey{
		run:        event.RunID,
		parentRun:  event.ParentRunID,
		parentStep: event.ParentStepID,
		parentPos:  event.ParentStep,
	}
}

// stepKey identifies one step within one run.
//
// Unlike runKey this is keyed on the bare RunID: a nested run's events report
// only the parent's RunID and step, not the parent's own parentage, so a lookup
// from the child cannot supply more. Two runs sharing a RunID and a step ID are
// therefore indistinguishable here — which is why the tracer records the trace
// under this key too, and treats a mismatch as an unlinkable run rather than
// guessing.
type stepKey struct {
	run  lebro.RunID
	step lebro.StepID
	pos  int
}

// stepSlot identifies one open step within a run's in-flight state.
//
// Position alone is not unique: a fan-out child step is reported at the same
// position as the fan-out step that launched it, so keying on position would make
// the child overwrite its parent. StepID separates those two.
//
// fanBranch additionally separates concurrent fan-out branches. A workflow the
// runtime accepts cannot reach this case — step IDs are validated unique across
// the whole tree, branches included — but the tracer consumes an event stream, not
// a validated workflow, and dropping the branch would let one branch's span
// overwrite another's and never be exported.
//
// The selected-branch value from a branch_selected event is deliberately excluded:
// it names the branch a conditional step *chose*, not the slot the step occupies,
// so including it made the lookup miss the open step. slotFor is used for step
// lifecycle events, where Branch means the fan-out branch being executed.
type stepSlot struct {
	pos       int
	stepID    lebro.StepID
	fanBranch string
}

// slotFor derives the slot a step lifecycle event addresses. On these events
// Branch is the fan-out branch under execution, which is exactly what
// disambiguates concurrent branches.
func slotFor(event lebro.RunEvent) stepSlot {
	return stepSlot{pos: event.Step, stepID: event.StepID, fanBranch: event.Branch}
}

// resolveSlot finds the open step slot an event runs inside, recovering the
// fan-out branch the event does not report.
//
// Retry-attempt, model, and tool events inside a fan-out child carry no branch
// even though the child's step span was recorded with one, so an exact slot
// lookup misses and the operation would parent to the enclosing fan-out step
// instead of the child. Position and StepID identify the step; the branch is
// recovered from the open slots, and only when unambiguous — attaching to the
// wrong concurrent branch is worse than attaching to the run.
func (run *runTrace) resolveSlot(event lebro.RunEvent) (stepSlot, bool) {
	exact := slotFor(event)
	if _, ok := run.steps[exact]; ok {
		return exact, true
	}
	var (
		found stepSlot
		count int
	)
	for slot := range run.steps {
		if slot.pos != event.Step || slot.stepID != event.StepID {
			continue
		}
		found = slot
		count++
	}
	if count != 1 {
		return exact, false
	}
	return found, true
}

// runTrace holds the in-flight spans for a single run.
type runTrace struct {
	key     runKey
	traceID TraceID
	rootID  SpanID
	root    *Span
	// model is the open model span for the run's current step. A run makes one
	// model call at a time, so a single slot is sufficient.
	model *Span
	// tools maps an open tool span by tool-call ID. A step may request several
	// tool calls, and the runtime may execute them in any order.
	tools map[string]*Span
	// steps maps an open step span by slot: position and StepID, plus the
	// fan-out branch where one is reported. See stepSlot for why each part is
	// needed.
	steps map[stepSlot]*Span
	// attempts maps an open retry-attempt span by the slot of the step it
	// retries, resolved through resolveSlot so it matches the step's own slot.
	attempts map[stepSlot]*Span
	// modelAttempts is the open provider-attempt span for the current model
	// call.
	modelAttempt *Span
	// deltas counts retained delta events on the current model span so the
	// limit applies per span rather than per run.
	deltas int
	// dropped counts delta events discarded past the limit.
	dropped int
}

// NewTracer validates the configuration and returns a tracer ready to receive
// events.
func NewTracer(config TracerConfig) (*Tracer, error) {
	if config.Sink == nil {
		return nil, errors.New("lebro/obsv: tracer sink is required")
	}
	ids := config.IDs
	if ids == nil {
		ids = &sequentialIDGenerator{}
	}
	limit := config.DeltaLimit
	if limit == 0 {
		limit = DefaultDeltaLimit
	}
	if limit < 0 {
		limit = 0
	}
	return &Tracer{
		sink:       config.Sink,
		ids:        ids,
		deltaLimit: limit,
		runs:       make(map[runKey]*runTrace),
		stepTraces: make(map[stepKey]stepAnchor),
	}, nil
}

var _ lebro.RunListener = (*Tracer)(nil)

// OnRunEvent converts one lifecycle event into span state, emitting spans to
// the sink as they end.
func (t *Tracer) OnRunEvent(event lebro.RunEvent) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	run := t.runFor(event)
	if run == nil {
		return
	}

	switch event.Type {
	case lebro.RunEventStarted, lebro.RunEventResumed:
		// The run span opened in runFor; nothing further to record.
	case lebro.RunEventModelStarted:
		t.openModel(run, event)
	case lebro.RunEventModelFinished:
		t.closeModel(run, event)
	case lebro.RunEventDelta:
		t.recordDelta(run, event)
	case lebro.RunEventToolRequested:
		t.recordToolRequested(run, event)
	case lebro.RunEventToolStarted:
		t.openTool(run, event)
	case lebro.RunEventToolFinished:
		t.closeTool(run, event)
	case lebro.RunEventStepStarted:
		t.openStep(run, event)
	case lebro.RunEventStepFinished:
		t.closeStep(run, event)
	case lebro.RunEventStepAttemptStarted:
		t.openStepAttempt(run, event)
	case lebro.RunEventStepAttemptFinished:
		t.closeStepAttempt(run, event)
	case lebro.RunEventModelAttemptStarted:
		t.openModelAttempt(run, event)
	case lebro.RunEventModelAttemptFinished:
		t.closeModelAttempt(run, event)
	case lebro.RunEventBranchSelected:
		t.recordBranch(run, event)
	case lebro.RunEventSucceeded, lebro.RunEventFailed, lebro.RunEventCancelled:
		t.finishRun(run, event, statusForRunEvent(event))
	case lebro.RunEventSuspended:
		t.finishRun(run, event, SpanStatusSuspended)
	}
}

// runFor returns the in-flight state for the event's run, opening a run span on
// first sight. A run whose first observed event is terminal still produces a
// root span, so a run cancelled before it began is not silently dropped.
func (t *Tracer) runFor(event lebro.RunEvent) *runTrace {
	key := keyFor(event)
	if run, ok := t.runs[key]; ok {
		return run
	}

	traceID, parentSpan := t.locateParent(event)
	spanID := t.ids.NewSpanID()
	root := &Span{
		TraceID:      traceID,
		SpanID:       spanID,
		ParentSpanID: parentSpan,
		Kind:         SpanKindRun,
		Name:         "run",
		RunID:        event.RunID,
		RunSpanID:    spanID,
		Start:        event.Timestamp,
		Status:       SpanStatusUnset,
	}
	run := &runTrace{
		key:      key,
		traceID:  traceID,
		rootID:   spanID,
		root:     root,
		tools:    make(map[string]*Span),
		steps:    make(map[stepSlot]*Span),
		attempts: make(map[stepSlot]*Span),
	}
	t.runs[key] = run
	return run
}

// locateParent resolves the trace and parent span a new run belongs to. A run
// started by a workflow step joins the parent's trace and parents to that
// step's span; a top-level run starts a new trace.
func (t *Tracer) locateParent(event lebro.RunEvent) (TraceID, SpanID) {
	if event.ParentRunID == "" {
		return t.ids.NewTraceID(), ""
	}
	anchor, ok := t.stepTraces[stepKey{run: event.ParentRunID, step: event.ParentStepID, pos: event.ParentStep}]
	if !ok {
		// The launching step was never observed — its events went to a
		// different listener, or this tracer attached mid-flight. Start a new
		// trace rather than fabricating a link to a span that was never
		// exported.
		return t.ids.NewTraceID(), ""
	}
	return anchor.trace, anchor.span
}

// parentFor returns the span a step-scoped operation should attach to: the
// enclosing step span when the run has one open, otherwise the run span.
func (run *runTrace) parentFor(event lebro.RunEvent) SpanID {
	if span, ok := run.enclosingStep(event); ok {
		return span.SpanID
	}
	return run.rootID
}

// openStep resolves the open step span a step lifecycle event belongs to.
func (run *runTrace) openStep(event lebro.RunEvent) (*Span, bool) {
	span, ok := run.steps[slotFor(event)]
	return span, ok
}

// enclosingStep resolves the open step span a step-scoped operation runs inside.
// The slot is recovered rather than computed, since these events do not report the
// fan-out branch their step was opened under.
func (run *runTrace) enclosingStep(event lebro.RunEvent) (*Span, bool) {
	slot, ok := run.resolveSlot(event)
	if !ok {
		return nil, false
	}
	span, ok := run.steps[slot]
	return span, ok
}

// stepParent returns the span a step should attach to: the enclosing fan-out step
// when this is a fan-out child, otherwise the run span.
//
// A fan-out child is reported at the same position as the fan-out step that
// launched it and carries the branch name, which is how the two are told apart.
// Without this the child parented to the run root and the fan-out step's subtree
// read as empty.
func (t *Tracer) stepParent(run *runTrace, event lebro.RunEvent) SpanID {
	if event.Branch == "" {
		return run.rootID
	}
	// The enclosing fan-out step occupies the same position under a different
	// step ID and no branch of its own.
	for slot, span := range run.steps {
		if slot.pos == event.Step && slot.fanBranch == "" && slot.stepID != event.StepID {
			return span.SpanID
		}
	}
	return run.rootID
}

func (t *Tracer) newSpan(run *runTrace, event lebro.RunEvent, kind SpanKind, name string, parent SpanID) *Span {
	span := &Span{
		TraceID:      run.traceID,
		SpanID:       t.ids.NewSpanID(),
		ParentSpanID: parent,
		Kind:         kind,
		Name:         name,
		RunID:        event.RunID,
		RunSpanID:    run.rootID,
		StepID:       event.StepID,
		Step:         event.Step,
		Start:        event.Timestamp,
		Status:       SpanStatusUnset,
	}
	if event.Branch != "" {
		span.setAttr(AttrBranch, event.Branch)
	}
	return span
}

func (t *Tracer) openStep(run *runTrace, event lebro.RunEvent) {
	span := t.newSpan(run, event, SpanKindStep, stepName(event.StepID), t.stepParent(run, event))
	span.setAttr(AttrStepPosition, strconv.Itoa(event.Step))
	run.steps[slotFor(event)] = span
	// Record the step span so a nested run launched from it can parent to it.
	// The nested run reports this step by RunID, StepID, and position, so the
	// anchor is keyed the way the child will address it.
	if event.Branch != "" {
		// Fan-out branches share the same step key (same RunID, StepID, and
		// position). Overwriting would attach a later branch's nested run to
		// the wrong step span. Preserve the first branch's anchor — it is an
		// unambiguous parent for all branches' nested runs.
		t.recordAnchorIfAbsent(event, run.traceID, span.SpanID)
		return
	}
	key := stepKey{run: event.RunID, step: event.StepID, pos: event.Step}
	if _, exists := t.stepTraces[key]; !exists {
		t.anchorOrder = append(t.anchorOrder, key)
	}
	t.stepTraces[key] = stepAnchor{trace: run.traceID, span: span.SpanID}
}

// pruneAnchors drops the oldest step anchors once the bound is exceeded.
func (t *Tracer) pruneAnchors() {
	for len(t.anchorOrder) > anchorLimit {
		oldest := t.anchorOrder[0]
		t.anchorOrder = t.anchorOrder[1:]
		delete(t.stepTraces, oldest)
	}
}

func (t *Tracer) closeStep(run *runTrace, event lebro.RunEvent) {
	span, ok := run.openStep(event)
	if !ok {
		// A step can finish without a started event when input validation
		// rejects it before any attempt runs. Synthesize the span so the
		// rejection is still traced.
		span = t.newSpan(run, event, SpanKindStep, stepName(event.StepID), t.stepParent(run, event))
		span.setAttr(AttrStepPosition, strconv.Itoa(event.Step))
	}
	delete(run.steps, slotFor(event))
	// A step's own span is no longer a valid parent once it has ended, but the
	// mapping is left in place: a nested run's events may arrive after its
	// launching step finished, and parenting to the ended step span is still
	// correct.
	t.end(span, event, statusForError(event.Error))
}

func (t *Tracer) openStepAttempt(run *runTrace, event lebro.RunEvent) {
	span := t.newSpan(run, event, SpanKindStepAttempt, fmt.Sprintf("%s attempt %d", stepName(event.StepID), event.Attempt), run.parentFor(event))
	span.setAttr(AttrAttempt, strconv.Itoa(event.Attempt))
	if event.Delay > 0 {
		span.setAttr(AttrAttemptDelay, event.Delay.String())
	}
	slot, _ := run.resolveSlot(event)
	run.attempts[slot] = span
}

func (t *Tracer) closeStepAttempt(run *runTrace, event lebro.RunEvent) {
	slot, _ := run.resolveSlot(event)
	span, ok := run.attempts[slot]
	if !ok {
		return
	}
	delete(run.attempts, slot)
	t.end(span, event, statusForError(event.Error))
}

func (t *Tracer) openModel(run *runTrace, event lebro.RunEvent) {
	run.model = t.newSpan(run, event, SpanKindModel, "model", run.parentFor(event))
	run.deltas = 0
	run.dropped = 0
}

func (t *Tracer) closeModel(run *runTrace, event lebro.RunEvent) {
	span := run.model
	if span == nil {
		span = t.newSpan(run, event, SpanKindModel, "model", run.parentFor(event))
	}
	run.model = nil
	span.Usage = event.Usage
	if event.FinishReason != "" {
		span.setAttr(AttrFinishReason, string(event.FinishReason))
	}
	if run.dropped > 0 {
		span.setAttr("model.deltas_dropped", strconv.Itoa(run.dropped))
	}
	run.deltas = 0
	run.dropped = 0
	// Aggregate usage onto the run span so a run's cost is readable without
	// summing its model spans.
	run.root.Usage.InputTokens += event.Usage.InputTokens
	run.root.Usage.OutputTokens += event.Usage.OutputTokens
	run.root.Usage.ReasoningTokens += event.Usage.ReasoningTokens
	run.root.Usage.TotalTokens += event.Usage.TotalTokens
	t.end(span, event, statusForError(event.Error))
}

func (t *Tracer) openModelAttempt(run *runTrace, event lebro.RunEvent) {
	parent := run.parentFor(event)
	if run.model != nil {
		parent = run.model.SpanID
	}
	span := t.newSpan(run, event, SpanKindModelAttempt, providerAttemptName(event), parent)
	span.setAttr(AttrProvider, string(event.Provider))
	span.setAttr(AttrProviderModel, event.ProviderModel)
	run.modelAttempt = span
}

func (t *Tracer) closeModelAttempt(run *runTrace, event lebro.RunEvent) {
	span := run.modelAttempt
	if span == nil {
		return
	}
	run.modelAttempt = nil
	if event.AttemptStatus != "" {
		span.setAttr(AttrAttemptStatus, string(event.AttemptStatus))
	}
	t.end(span, event, statusForError(event.Error))
}

func (t *Tracer) openTool(run *runTrace, event lebro.RunEvent) {
	span := t.newSpan(run, event, SpanKindTool, toolName(event.ToolID), run.parentFor(event))
	span.setAttr(AttrToolID, string(event.ToolID))
	if event.ToolCallID != "" {
		span.setAttr(AttrToolCallID, event.ToolCallID)
	}
	run.tools[event.ToolCallID] = span
	// A subagent starts inside its tool handler. Agent loops do not emit
	// workflow step events, so this tool span is child run's concrete parent.
	t.recordAnchor(event, run.traceID, span.SpanID)
}

func (t *Tracer) recordAnchor(event lebro.RunEvent, traceID TraceID, spanID SpanID) {
	if event.Branch != "" {
		// Fan-out branches share the same step key. Overwriting the anchor
		// would attach a later branch's subagent to the wrong tool span.
		// Preserve the existing anchor — the outer step span is an unambiguous
		// parent for all branches' nested runs.
		t.recordAnchorIfAbsent(event, traceID, spanID)
		return
	}
	key := stepKey{run: event.RunID, step: event.StepID, pos: event.Step}
	if _, exists := t.stepTraces[key]; !exists {
		t.anchorOrder = append(t.anchorOrder, key)
	}
	t.stepTraces[key] = stepAnchor{trace: traceID, span: spanID}
}

// recordAnchorIfAbsent records a step anchor only when no anchor exists for the
// key. It is used for fan-out branch steps and tools, where concurrent branches
// share the same step key and overwriting would attach a later branch's nested
// run to the wrong span.
func (t *Tracer) recordAnchorIfAbsent(event lebro.RunEvent, traceID TraceID, spanID SpanID) {
	key := stepKey{run: event.RunID, step: event.StepID, pos: event.Step}
	if _, exists := t.stepTraces[key]; exists {
		return
	}
	t.anchorOrder = append(t.anchorOrder, key)
	t.stepTraces[key] = stepAnchor{trace: traceID, span: spanID}
}

func (t *Tracer) closeTool(run *runTrace, event lebro.RunEvent) {
	span, ok := run.tools[event.ToolCallID]
	if !ok {
		span = t.newSpan(run, event, SpanKindTool, toolName(event.ToolID), run.parentFor(event))
		span.setAttr(AttrToolID, string(event.ToolID))
		if event.ToolCallID != "" {
			span.setAttr(AttrToolCallID, event.ToolCallID)
		}
	}
	delete(run.tools, event.ToolCallID)
	if event.ToolState != "" {
		span.setAttr(AttrToolState, string(event.ToolState))
	}
	status := statusForError(event.Error)
	// A tool can report a non-succeeded state without returning an error to
	// the run: invalid input is fed back to the model rather than failing the
	// run. Reflect the state so the span is not misread as a clean call.
	if status == SpanStatusOK && event.ToolState != "" && event.ToolState != lebro.ToolExecutionSucceeded {
		status = SpanStatusError
	}
	t.end(span, event, status)
}

func (t *Tracer) recordToolRequested(run *runTrace, event lebro.RunEvent) {
	target := run.model
	if target == nil {
		target = run.root
	}
	attributes := map[string]string{AttrToolID: string(event.ToolID)}
	if event.ToolCallID != "" {
		attributes[AttrToolCallID] = event.ToolCallID
	}
	target.addEvent(SpanEvent{Name: "tool_requested", Timestamp: event.Timestamp, Attributes: attributes})
}

func (t *Tracer) recordDelta(run *runTrace, event lebro.RunEvent) {
	target := run.model
	if target == nil {
		target = run.root
	}
	if run.deltas >= t.deltaLimit {
		run.dropped++
		return
	}
	run.deltas++
	attributes := make(map[string]string, 3)
	if event.DeltaText != "" {
		attributes[AttrSensitiveDeltaText] = event.DeltaText
	}
	if event.DeltaReasoning.Text != "" {
		attributes[AttrSensitiveDeltaReasoning] = event.DeltaReasoning.Text
	}
	if event.DeltaStructuredOutput != "" {
		attributes[AttrSensitiveStructured] = string(event.DeltaStructuredOutput)
	}
	if event.ToolCallID != "" {
		attributes[AttrToolCallID] = event.ToolCallID
	}
	if event.Error != nil {
		attributes["error"] = event.Error.Error()
	}
	if len(attributes) == 0 {
		attributes = nil
	}
	target.addEvent(SpanEvent{Name: "model_delta", Timestamp: event.Timestamp, Attributes: attributes})
}

func (t *Tracer) recordBranch(run *runTrace, event lebro.RunEvent) {
	branch := event.Branch
	if branch == "" {
		// Branch selection historically reported the selected branch through
		// DeltaText; accept either so a trace is correct on both.
		branch = event.DeltaText
	}
	target := run.root
	// Resolve by position and StepID only: event.Branch here is the branch the
	// step *selected*, not the fan-out branch it runs in, so it must not take
	// part in the slot lookup. enclosingStep ignores it for exactly this reason.
	if span, ok := run.enclosingStep(event); ok {
		target = span
		span.setAttr(AttrBranch, branch)
	}
	target.addEvent(SpanEvent{
		Name:       "branch_selected",
		Timestamp:  event.Timestamp,
		Attributes: map[string]string{AttrBranch: branch},
	})
}

// finishRun ends every span still open for the run and then the run span
// itself, so a failure mid-step never leaks an unexported span.
func (t *Tracer) finishRun(run *runTrace, event lebro.RunEvent, status SpanStatus) {
	if span := run.model; span != nil {
		run.model = nil
		t.end(span, event, abandonedStatus(status))
	}
	if span := run.modelAttempt; span != nil {
		run.modelAttempt = nil
		t.end(span, event, abandonedStatus(status))
	}
	for callID, span := range run.tools {
		delete(run.tools, callID)
		t.end(span, event, abandonedStatus(status))
	}
	for step, span := range run.attempts {
		delete(run.attempts, step)
		t.end(span, event, abandonedStatus(status))
	}
	for step, span := range run.steps {
		delete(run.steps, step)
		t.end(span, event, abandonedStatus(status))
	}

	root := run.root
	if event.Status != "" {
		root.setAttr(AttrRunStatus, string(event.Status))
	}
	// A run span's duration spans the whole run, which the terminal event does
	// not carry, so derive it from the bracketing timestamps.
	if root.Duration == 0 && !root.Start.IsZero() && event.Timestamp.After(root.Start) {
		root.Duration = event.Timestamp.Sub(root.Start)
	}
	delete(t.runs, run.key)
	// Step anchors are retained past the run: a nested run's events can arrive
	// after its parent finished, and dropping them would split the trace. They
	// are pruned by count rather than by run lifetime.
	t.pruneAnchors()
	t.endRoot(root, event, status)
}

// abandonedStatus reports the status to record on a span left open when its run
// ended. A cancelled run cancels its in-flight work; any other terminal outcome
// leaves the work unfinished, which is an error.
func abandonedStatus(runStatus SpanStatus) SpanStatus {
	if runStatus == SpanStatusCancelled {
		return SpanStatusCancelled
	}
	return SpanStatusError
}

// end finalizes a non-root span from the event that closed it and emits it.
func (t *Tracer) end(span *Span, event lebro.RunEvent, status SpanStatus) {
	span.End = event.Timestamp
	span.Duration = event.Duration
	span.Status = status
	if event.Error != nil {
		span.Err = event.Error
		span.Error = event.Error.Error()
	}
	t.sink.OnSpanEnd(span.Clone())
}

// endRoot finalizes a run span. It keeps the duration already computed for the
// whole run rather than the terminal event's own duration, which is zero.
func (t *Tracer) endRoot(span *Span, event lebro.RunEvent, status SpanStatus) {
	span.End = event.Timestamp
	span.Status = status
	if event.Error != nil {
		span.Err = event.Error
		span.Error = event.Error.Error()
	}
	t.sink.OnSpanEnd(span.Clone())
}

func statusForRunEvent(event lebro.RunEvent) SpanStatus {
	switch event.Type {
	case lebro.RunEventSucceeded:
		return SpanStatusOK
	case lebro.RunEventCancelled:
		return SpanStatusCancelled
	case lebro.RunEventFailed:
		return SpanStatusError
	default:
		return statusForError(event.Error)
	}
}

func (s *Span) setAttr(key, value string) {
	if value == "" {
		return
	}
	if s.Attributes == nil {
		s.Attributes = make(map[string]string, 4)
	}
	s.Attributes[key] = value
}

func (s *Span) addEvent(event SpanEvent) {
	s.Events = append(s.Events, event)
}

func stepName(stepID lebro.StepID) string {
	if stepID == "" {
		return "step"
	}
	return string(stepID)
}

func toolName(toolID lebro.ToolID) string {
	if toolID == "" {
		return "tool"
	}
	return string(toolID)
}

func providerAttemptName(event lebro.RunEvent) string {
	switch {
	case event.Provider != "" && event.ProviderModel != "":
		return fmt.Sprintf("%s/%s", event.Provider, event.ProviderModel)
	case event.Provider != "":
		return string(event.Provider)
	case event.ProviderModel != "":
		return event.ProviderModel
	default:
		return "model_attempt"
	}
}

// isCancellation reports whether err represents a context cancellation or
// deadline, including lebro's typed wrappers around them.
func isCancellation(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var agentErr *lebro.AgentError
	if errors.As(err, &agentErr) && agentErr.Kind == lebro.AgentErrorCancelled {
		return true
	}
	var workflowErr *lebro.WorkflowError
	if errors.As(err, &workflowErr) && workflowErr.Kind == lebro.WorkflowErrorCancelled {
		return true
	}
	var subagentErr *lebro.SubagentError
	if errors.As(err, &subagentErr) && subagentErr.Kind == lebro.SubagentErrorCancelled {
		return true
	}
	return false
}

// sequentialIDGenerator produces monotonic trace and span IDs. IDs are unique
// within a process, which is what in-process correlation requires; an exporter
// that needs globally unique IDs supplies its own generator.
type sequentialIDGenerator struct {
	mu    sync.Mutex
	trace int
	span  int
}

func (g *sequentialIDGenerator) NewTraceID() TraceID {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.trace++
	return TraceID(fmt.Sprintf("trace-%08d", g.trace))
}

func (g *sequentialIDGenerator) NewSpanID() SpanID {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.span++
	return SpanID(fmt.Sprintf("span-%08d", g.span))
}

// NewSequentialIDGenerator returns an IDGenerator producing monotonic
// "trace-NNNNNNNN" and "span-NNNNNNNN" identifiers. It makes span structure
// assertable in tests and is the default when Config.IDs is nil.
func NewSequentialIDGenerator() IDGenerator { return &sequentialIDGenerator{} }
