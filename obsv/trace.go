package obsv

import (
	"time"

	"github.com/tesh254/lebro"
)

// Stable identifiers keep exported observability data portable across backends.
type (
	// TraceID identifies one correlated tree of spans. Every span produced for
	// a top-level run and for the nested runs it starts shares one TraceID.
	TraceID string
	// SpanID identifies a single span within a trace.
	SpanID string
)

// SpanKind classifies the operation a span measures.
type SpanKind string

const (
	// SpanKindRun measures a complete agent or workflow run. It is the root
	// span of a trace, or the child of a step span when a workflow step starts
	// a nested run.
	SpanKindRun SpanKind = "run"
	// SpanKindStep measures one workflow step, covering every attempt.
	SpanKindStep SpanKind = "step"
	// SpanKindModel measures one model call, carrying token usage and the
	// finish reason.
	SpanKindModel SpanKind = "model"
	// SpanKindTool measures one tool handler invocation, carrying the tool ID
	// and execution state.
	SpanKindTool SpanKind = "tool"
	// SpanKindStepAttempt measures one retry attempt past the first. The first
	// attempt is covered by the step span itself.
	SpanKindStepAttempt SpanKind = "step_attempt"
	// SpanKindModelAttempt measures one provider attempt when a ModelRouter
	// resolves a model call through a fallback chain.
	SpanKindModelAttempt SpanKind = "model_attempt"
)

// SpanStatus is the outcome of the operation a span measures.
type SpanStatus string

const (
	// SpanStatusUnset marks a span that has not been ended. Exported spans are
	// always ended, so this appears only on in-flight spans.
	SpanStatusUnset SpanStatus = "unset"
	// SpanStatusOK marks an operation that completed without error.
	SpanStatusOK SpanStatus = "ok"
	// SpanStatusError marks an operation that failed.
	SpanStatusError SpanStatus = "error"
	// SpanStatusCancelled marks an operation ended by context cancellation.
	SpanStatusCancelled SpanStatus = "cancelled"
	// SpanStatusSuspended marks a run that suspended at a step boundary rather
	// than reaching a terminal outcome. The run span is exported at the
	// suspend boundary; a later Resume produces a new trace.
	SpanStatusSuspended SpanStatus = "suspended"
)

// SensitiveAttr is the prefix on every attribute key whose value may carry
// model- or tool-supplied payload data. Filters use the prefix to drop payloads
// without enumerating each producing site.
const SensitiveAttr = "sensitive."

// Attribute keys set by the tracer. Keys under SensitiveAttr carry payload data
// and are removed by DefaultFilter.
const (
	AttrToolID        = "tool.id"
	AttrToolCallID    = "tool.call_id"
	AttrToolState     = "tool.state"
	AttrFinishReason  = "model.finish_reason"
	AttrProvider      = "model.provider"
	AttrProviderModel = "model.provider_model"
	AttrAttemptStatus = "model.attempt_status"
	AttrAttempt       = "attempt.number"
	AttrAttemptDelay  = "attempt.delay"
	AttrBranch        = "workflow.branch"
	AttrRunStatus     = "run.status"
	AttrStepPosition  = "step.position"
	AttrThreadID      = "thread.id"

	// AttrSensitiveDeltaText accumulates streamed assistant text.
	AttrSensitiveDeltaText = SensitiveAttr + "model.delta_text"
	// AttrSensitiveStructured carries streamed structured output.
	AttrSensitiveStructured = SensitiveAttr + "model.structured_output"
)

// SpanEvent is a point-in-time occurrence within a span. Streaming deltas and
// tool-call requests are recorded as events because they have no duration of
// their own and would otherwise multiply spans without adding structure.
type SpanEvent struct {
	Name       string            `json:"name"`
	Timestamp  time.Time         `json:"timestamp"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// Span measures one operation within a run.
//
// Start and End bracket the operation; End is zero on a span that has not
// ended. Duration is the runtime-reported elapsed time for the operation, which
// is authoritative even when a fixed Clock makes End equal Start. ParentSpanID
// is empty on the root span of a trace. Usage is populated on model spans and
// aggregated onto run spans. Err is the Go error the run reported, retained for
// in-process exporters; Error is its message, which is what a backend receives.
//
// A Span is a value: exporters receive a copy and own the Attributes and Events
// maps and slices reachable from it, so retaining a span past the export call
// is safe.
type Span struct {
	TraceID      TraceID           `json:"trace_id"`
	SpanID       SpanID            `json:"span_id"`
	ParentSpanID SpanID            `json:"parent_span_id,omitempty"`
	Kind         SpanKind          `json:"kind"`
	Name         string            `json:"name"`
	RunID        lebro.RunID       `json:"run_id,omitempty"`
	StepID       lebro.StepID      `json:"step_id,omitempty"`
	Step         int               `json:"step,omitempty"`
	Start        time.Time         `json:"start"`
	End          time.Time         `json:"end,omitempty"`
	Duration     time.Duration     `json:"duration,omitempty"`
	Status       SpanStatus        `json:"status"`
	Usage        lebro.ModelUsage  `json:"usage,omitzero"`
	Attributes   map[string]string `json:"attributes,omitempty"`
	Events       []SpanEvent       `json:"events,omitempty"`
	Error        string            `json:"error,omitempty"`
	Err          error             `json:"-"`
}

// Clone returns a deep copy of the span. The copy shares no maps or slices with
// the original, so a caller may retain or mutate it freely.
func (s Span) Clone() Span {
	cloned := s
	cloned.Attributes = cloneAttributes(s.Attributes)
	if len(s.Events) > 0 {
		events := make([]SpanEvent, len(s.Events))
		for i, event := range s.Events {
			event.Attributes = cloneAttributes(event.Attributes)
			events[i] = event
		}
		cloned.Events = events
	} else {
		cloned.Events = nil
	}
	return cloned
}

// IsRoot reports whether the span roots its trace.
func (s Span) IsRoot() bool { return s.ParentSpanID == "" }

func cloneAttributes(attributes map[string]string) map[string]string {
	if len(attributes) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(attributes))
	for key, value := range attributes {
		cloned[key] = value
	}
	return cloned
}

func cloneSpans(spans []Span) []Span {
	if len(spans) == 0 {
		return nil
	}
	cloned := make([]Span, len(spans))
	for i, span := range spans {
		cloned[i] = span.Clone()
	}
	return cloned
}

// statusForError maps a run-reported error onto a span status. A nil error is
// OK; a cancellation is reported distinctly so a cancelled run is not counted
// as a failure.
func statusForError(err error) SpanStatus {
	switch {
	case err == nil:
		return SpanStatusOK
	case isCancellation(err):
		return SpanStatusCancelled
	default:
		return SpanStatusError
	}
}
