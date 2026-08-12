package obsv

import (
	"context"
	"sync"

	"github.com/tesh254/lebro"
)

// Repository persists observability data.
//
// It is deliberately separate from lebro.Store: spans, logs, and feedback can
// live in a different database from threads and workflow state, and an
// observability write can never join the transaction that persists a workflow
// step. A Repository failure is an export failure — it reaches the ErrorHandler
// and never the run.
//
// Implementations must be safe for concurrent use. The slices passed to Append
// methods are owned by the implementation and may be retained.
type Repository interface {
	AppendSpans(ctx context.Context, spans []Span) error
	AppendLogs(ctx context.Context, records []LogRecord) error
	AppendFeedback(ctx context.Context, records []FeedbackRecord) error
	// SpansByTrace returns every stored span for a trace, in insertion order.
	SpansByTrace(ctx context.Context, traceID TraceID) ([]Span, error)
	// SpansByRun returns every stored span for one run occurrence, in insertion
	// order. Run IDs can collide across independently configured primitives, so
	// TraceID is required alongside RunID. When runSpanID is non-empty it
	// identifies the specific run root span and disambiguates nested runs that
	// reuse the same RunID within one trace; when empty, all spans matching
	// TraceID and RunID are returned.
	SpansByRun(ctx context.Context, traceID TraceID, runID lebro.RunID, runSpanID SpanID) ([]Span, error)
	// FeedbackByRun returns every stored feedback record for one run occurrence.
	// runSpanID disambiguates nested runs that reuse the same RunID within one
	// trace when non-empty; when empty, all records matching TraceID and RunID
	// are returned.
	FeedbackByRun(ctx context.Context, traceID TraceID, runID lebro.RunID, runSpanID SpanID) ([]FeedbackRecord, error)
}

// MemoryRepository is an in-memory Repository. It requires no database, making
// it suitable for tests, local development, and single-process deployments that
// only need recent history. It is safe for concurrent use.
//
// Its zero value is ready for use. NewMemoryRepository is a convenience for
// callers who prefer an explicit constructor.
type MemoryRepository struct {
	mu       sync.RWMutex
	spans    []Span
	logs     []LogRecord
	feedback []FeedbackRecord
}

// NewMemoryRepository returns an empty in-memory repository.
func NewMemoryRepository() *MemoryRepository { return &MemoryRepository{} }

var _ Repository = (*MemoryRepository)(nil)

// AppendSpans stores copies of the spans.
func (r *MemoryRepository) AppendSpans(_ context.Context, spans []Span) error {
	if r == nil || len(spans) == 0 {
		return nil
	}
	stored := cloneSpans(spans)
	r.mu.Lock()
	r.spans = append(r.spans, stored...)
	r.mu.Unlock()
	return nil
}

// AppendLogs stores copies of the log records.
func (r *MemoryRepository) AppendLogs(_ context.Context, records []LogRecord) error {
	if r == nil || len(records) == 0 {
		return nil
	}
	stored := cloneLogRecords(records)
	r.mu.Lock()
	r.logs = append(r.logs, stored...)
	r.mu.Unlock()
	return nil
}

// AppendFeedback stores copies of the feedback records.
func (r *MemoryRepository) AppendFeedback(_ context.Context, records []FeedbackRecord) error {
	if r == nil || len(records) == 0 {
		return nil
	}
	stored := cloneFeedbackRecords(records)
	r.mu.Lock()
	r.feedback = append(r.feedback, stored...)
	r.mu.Unlock()
	return nil
}

// SpansByTrace returns caller-owned copies of the trace's spans.
func (r *MemoryRepository) SpansByTrace(_ context.Context, traceID TraceID) ([]Span, error) {
	if r == nil {
		return nil, nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var matched []Span
	for _, span := range r.spans {
		if span.TraceID == traceID {
			matched = append(matched, span.Clone())
		}
	}
	return matched, nil
}

// SpansByRun returns caller-owned copies of the run's spans. When runSpanID is
// non-empty, only spans belonging to that run root span are returned; otherwise
// all spans matching TraceID and RunID are returned.
func (r *MemoryRepository) SpansByRun(_ context.Context, traceID TraceID, runID lebro.RunID, runSpanID SpanID) ([]Span, error) {
	if r == nil {
		return nil, nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var matched []Span
	for _, span := range r.spans {
		if span.TraceID != traceID || span.RunID != runID {
			continue
		}
		if runSpanID != "" && span.RunSpanID != runSpanID {
			continue
		}
		matched = append(matched, span.Clone())
	}
	return matched, nil
}

// FeedbackByRun returns caller-owned copies of the run's feedback records.
// When runSpanID is non-empty, only records for that run root span are
// returned; otherwise all records matching TraceID and RunID are returned.
func (r *MemoryRepository) FeedbackByRun(_ context.Context, traceID TraceID, runID lebro.RunID, runSpanID SpanID) ([]FeedbackRecord, error) {
	if r == nil {
		return nil, nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var matched []FeedbackRecord
	for _, record := range r.feedback {
		if record.TraceID != traceID || record.RunID != runID {
			continue
		}
		if runSpanID != "" && record.RunSpanID != runSpanID {
			continue
		}
		matched = append(matched, record.Clone())
	}
	return matched, nil
}

// Spans returns caller-owned copies of every stored span in insertion order.
func (r *MemoryRepository) Spans() []Span {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return cloneSpans(r.spans)
}

// Logs returns caller-owned copies of every stored log record in insertion
// order.
func (r *MemoryRepository) Logs() []LogRecord {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return cloneLogRecords(r.logs)
}

// Feedback returns caller-owned copies of every stored feedback record in
// insertion order.
func (r *MemoryRepository) Feedback() []FeedbackRecord {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return cloneFeedbackRecords(r.feedback)
}
