package obsv

import (
	"time"

	"github.com/tesh254/lebro"
)

// Severity classifies a log record.
type Severity string

const (
	SeverityDebug Severity = "debug"
	SeverityInfo  Severity = "info"
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

// LogRecord is one log line derived from a span, correlated to that span by ID
// rather than by parsing text. Attributes are copied from the span after
// filtering, so a filtered payload never reappears in a log line.
type LogRecord struct {
	Timestamp  time.Time         `json:"timestamp"`
	Severity   Severity          `json:"severity"`
	Message    string            `json:"message"`
	TraceID    TraceID           `json:"trace_id,omitempty"`
	SpanID     SpanID            `json:"span_id,omitempty"`
	RunID      lebro.RunID       `json:"run_id,omitempty"`
	StepID     lebro.StepID      `json:"step_id,omitempty"`
	Kind       SpanKind          `json:"kind,omitempty"`
	Duration   time.Duration     `json:"duration,omitempty"`
	Error      string            `json:"error,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// Clone returns a deep copy of the record.
func (r LogRecord) Clone() LogRecord {
	r.Attributes = cloneAttributes(r.Attributes)
	return r
}

// logForSpan derives the log record for an ended span. It is called after
// filtering, so span carries only what the filter allowed.
func logForSpan(span Span) LogRecord {
	timestamp := span.End
	if timestamp.IsZero() {
		timestamp = span.Start
	}
	return LogRecord{
		Timestamp:  timestamp,
		Severity:   severityForStatus(span.Status),
		Message:    logMessage(span),
		TraceID:    span.TraceID,
		SpanID:     span.SpanID,
		RunID:      span.RunID,
		StepID:     span.StepID,
		Kind:       span.Kind,
		Duration:   span.Duration,
		Error:      span.Error,
		Attributes: cloneAttributes(span.Attributes),
	}
}

func logMessage(span Span) string {
	return string(span.Kind) + " " + span.Name + " " + string(span.Status)
}

func severityForStatus(status SpanStatus) Severity {
	switch status {
	case SpanStatusError:
		return SeverityError
	case SpanStatusCancelled, SpanStatusSuspended:
		return SeverityWarn
	default:
		return SeverityInfo
	}
}

func cloneLogRecords(records []LogRecord) []LogRecord {
	if len(records) == 0 {
		return nil
	}
	cloned := make([]LogRecord, len(records))
	for i, record := range records {
		cloned[i] = record.Clone()
	}
	return cloned
}
