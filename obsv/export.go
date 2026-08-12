package obsv

import "context"

// SpanExporter delivers ended spans to a backend.
//
// The Observer calls an exporter on its own goroutine, never on the goroutine
// running the agent, and treats every outcome as non-fatal: a returned error is
// reported to the ErrorHandler and a panic is recovered. An exporter therefore
// cannot change a run's result, but it can still delay its own queue, so a slow
// backend should buffer internally rather than blocking here.
//
// The spans slice and everything reachable from it is owned by the exporter: the
// Observer built it from copies and does not read it again. Retaining it is
// safe.
type SpanExporter interface {
	ExportSpans(ctx context.Context, spans []Span) error
}

// LogExporter delivers correlated log records to a backend. The same ownership
// and isolation rules as SpanExporter apply.
type LogExporter interface {
	ExportLogs(ctx context.Context, records []LogRecord) error
}

// MetricExporter delivers usage and latency metrics to a backend. The same
// ownership and isolation rules as SpanExporter apply.
type MetricExporter interface {
	ExportMetrics(ctx context.Context, metrics []Metric) error
}

// FeedbackExporter delivers feedback records to a backend. Feedback is submitted
// by an application after a run finishes rather than derived from run events, so
// unlike the other exporters it is driven by Observer.RecordFeedback. The same
// ownership and isolation rules as SpanExporter apply.
type FeedbackExporter interface {
	ExportFeedback(ctx context.Context, records []FeedbackRecord) error
}

// SpanExporterFunc adapts a function to SpanExporter.
type SpanExporterFunc func(context.Context, []Span) error

// ExportSpans calls f.
func (f SpanExporterFunc) ExportSpans(ctx context.Context, spans []Span) error {
	return f(ctx, spans)
}

// LogExporterFunc adapts a function to LogExporter.
type LogExporterFunc func(context.Context, []LogRecord) error

// ExportLogs calls f.
func (f LogExporterFunc) ExportLogs(ctx context.Context, records []LogRecord) error {
	return f(ctx, records)
}

// MetricExporterFunc adapts a function to MetricExporter.
type MetricExporterFunc func(context.Context, []Metric) error

// ExportMetrics calls f.
func (f MetricExporterFunc) ExportMetrics(ctx context.Context, metrics []Metric) error {
	return f(ctx, metrics)
}

// FeedbackExporterFunc adapts a function to FeedbackExporter.
type FeedbackExporterFunc func(context.Context, []FeedbackRecord) error

// ExportFeedback calls f.
func (f FeedbackExporterFunc) ExportFeedback(ctx context.Context, records []FeedbackRecord) error {
	return f(ctx, records)
}

// ErrorHandler receives errors raised while exporting. It runs on the
// Observer's goroutine and must not panic or block; a handler that does either
// only delays export, never the run.
//
// A nil ErrorHandler drops export errors, which is the correct default for a
// signal that must not affect the workload it observes.
type ErrorHandler func(error)

// MemorySpanExporter collects exported spans in memory. It requires no backend,
// making it suitable for tests, local development, and programmatic inspection.
type MemorySpanExporter struct {
	guarded[Span]
}

// NewMemorySpanExporter returns an empty in-memory span exporter.
func NewMemorySpanExporter() *MemorySpanExporter { return &MemorySpanExporter{} }

// ExportSpans records the spans and never fails.
func (e *MemorySpanExporter) ExportSpans(_ context.Context, spans []Span) error {
	e.append(spans...)
	return nil
}

// Spans returns a caller-owned copy of every exported span in export order.
func (e *MemorySpanExporter) Spans() []Span { return cloneSpans(e.snapshot()) }

// MemoryLogExporter collects exported log records in memory.
type MemoryLogExporter struct {
	guarded[LogRecord]
}

// NewMemoryLogExporter returns an empty in-memory log exporter.
func NewMemoryLogExporter() *MemoryLogExporter { return &MemoryLogExporter{} }

// ExportLogs records the log records and never fails.
func (e *MemoryLogExporter) ExportLogs(_ context.Context, records []LogRecord) error {
	e.append(records...)
	return nil
}

// Records returns a caller-owned copy of every exported log record in export
// order.
func (e *MemoryLogExporter) Records() []LogRecord { return cloneLogRecords(e.snapshot()) }

// MemoryMetricExporter collects exported metrics in memory.
type MemoryMetricExporter struct {
	guarded[Metric]
}

// NewMemoryMetricExporter returns an empty in-memory metric exporter.
func NewMemoryMetricExporter() *MemoryMetricExporter { return &MemoryMetricExporter{} }

// ExportMetrics records the metrics and never fails.
func (e *MemoryMetricExporter) ExportMetrics(_ context.Context, metrics []Metric) error {
	e.append(metrics...)
	return nil
}

// Metrics returns a caller-owned copy of every exported metric in export order.
func (e *MemoryMetricExporter) Metrics() []Metric { return cloneMetrics(e.snapshot()) }

// MemoryFeedbackExporter collects exported feedback records in memory.
type MemoryFeedbackExporter struct {
	guarded[FeedbackRecord]
}

// NewMemoryFeedbackExporter returns an empty in-memory feedback exporter.
func NewMemoryFeedbackExporter() *MemoryFeedbackExporter { return &MemoryFeedbackExporter{} }

// ExportFeedback records the feedback and never fails.
func (e *MemoryFeedbackExporter) ExportFeedback(_ context.Context, records []FeedbackRecord) error {
	e.append(records...)
	return nil
}

// Records returns a caller-owned copy of every exported feedback record in
// export order.
func (e *MemoryFeedbackExporter) Records() []FeedbackRecord {
	return cloneFeedbackRecords(e.snapshot())
}
