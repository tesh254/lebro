package obsv

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tesh254/lebro"
)

// DefaultQueueSize is the number of ended spans an Observer buffers when
// Config.QueueSize is zero. The queue exists so a slow exporter delays export
// rather than the run; when it fills, spans are dropped and counted.
const DefaultQueueSize = 1024

// DefaultExportTimeout bounds a single exporter call when Config.ExportTimeout
// is zero. An exporter that ignores its context can still overrun it — the
// timeout bounds the context, not the goroutine — but it never blocks the run.
const DefaultExportTimeout = 30 * time.Second

// Config assembles an Observer.
//
// Every field is optional. A zero-valued Config produces a working Observer that
// derives spans and drops them, which is useless but harmless; supply at least
// one exporter or a Repository for it to do work.
type Config struct {
	// Spans, Logs, Metrics, and Feedback receive the corresponding signal.
	// Logs and Metrics are derived from the same filtered spans as Spans, so a
	// field dropped by a Filter is absent from all three.
	Spans    SpanExporter
	Logs     LogExporter
	Metrics  MetricExporter
	Feedback FeedbackExporter
	// Repository persists spans, logs, and feedback separately from thread and
	// workflow state. It receives the same filtered data as the exporters.
	Repository Repository
	// Filter rewrites each span before export. Nil selects DefaultFilter, which
	// drops payload attributes; pass PassthroughFilter to export them.
	Filter Filter
	// FeedbackFilter rewrites each feedback record before export. Nil selects
	// DefaultFeedbackFilter, which drops reviewer comments.
	FeedbackFilter FeedbackFilter
	// ErrorHandler receives export errors and recovered exporter panics. Nil
	// drops them, which keeps a broken backend from becoming a run's problem.
	ErrorHandler ErrorHandler
	// QueueSize bounds buffered spans. Zero selects DefaultQueueSize. A
	// negative value exports synchronously on the emitting goroutine, which
	// makes tests deterministic but lets a slow exporter delay a run; failures
	// are still isolated.
	QueueSize int
	// BatchSize bounds how many spans are handed to an exporter in one call.
	// Zero or negative exports each drained batch whole.
	BatchSize int
	// ExportTimeout bounds the context passed to each exporter call. Zero
	// selects DefaultExportTimeout; a negative value passes an uncancelled
	// context.
	ExportTimeout time.Duration
	// IDs generates trace and span identifiers. Nil selects a sequential
	// generator.
	IDs IDGenerator
	// DeltaLimit bounds retained streaming-delta events per model span. Zero
	// selects DefaultDeltaLimit; a negative value retains none.
	DeltaLimit int
	// Clock supplies timestamps for feedback records that carry none. Nil uses
	// the system clock.
	Clock lebro.Clock
}

// Stats reports an Observer's export counters. They make loss observable: a
// dropped span is not a silent one.
type Stats struct {
	// SpansExported counts spans handed to at least one exporter or repository.
	SpansExported int64
	// SpansDropped counts spans discarded because the queue was full or the
	// observer was already closed.
	SpansDropped int64
	// SpansFiltered counts spans suppressed by a Filter.
	SpansFiltered int64
	// ExportFailures counts exporter calls that returned an error or panicked.
	ExportFailures int64
	// FeedbackExported counts feedback records handed to an exporter or
	// repository.
	FeedbackExported int64
	// FeedbackFiltered counts feedback records suppressed by a FeedbackFilter.
	FeedbackFiltered int64
}

// Observer converts run lifecycle events into filtered, exported observability
// data. It implements lebro.RunListener, so it attaches to any agent or
// workflow that accepts a listener:
//
//	observer, err := obsv.New(obsv.Config{Spans: exporter})
//	defer observer.Close()
//	agent, err := lebro.NewAgent(lebro.AgentConfig{Listener: observer, /* ... */})
//
// Export runs on the Observer's own goroutine. An exporter that fails, panics,
// or blocks cannot change or delay a run; the run's only interaction with the
// Observer is a bounded channel send that drops rather than waits.
//
// The zero value is not usable; construct one with New.
type Observer struct {
	tracer   *Tracer
	filter   Filter
	feedback FeedbackFilter
	clock    lebro.Clock

	spans      SpanExporter
	logs       LogExporter
	metrics    MetricExporter
	feedbackEx FeedbackExporter
	repository Repository

	onError   ErrorHandler
	batchSize int
	timeout   time.Duration

	queue chan Span
	// synchronous exports on the emitting goroutine; queue is nil.
	synchronous bool

	// traces maps a run to its trace and run root span so RecordFeedback can
	// correlate a record from a RunID alone. Bounded by traceMemoryLimit.
	traceMu sync.Mutex
	traces  map[lebro.RunID]runTraceRef
	traceLR []lebro.RunID

	stats struct {
		exported         atomic.Int64
		dropped          atomic.Int64
		filtered         atomic.Int64
		failures         atomic.Int64
		feedbacks        atomic.Int64
		feedbackFiltered atomic.Int64
		droppedReported  atomic.Int64
		failuresReported atomic.Int64
	}

	emissionMu sync.Mutex
	closed     bool

	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup
}

// traceMemoryLimit bounds how many run-to-trace mappings an Observer retains for
// feedback correlation. Feedback arrives after a run, so the mapping must
// outlive it, but an unbounded map would grow with every run for the process's
// lifetime.
const traceMemoryLimit = 4096

// New validates the configuration and returns an Observer ready to receive
// events. Close it when the process is shutting down so buffered spans are
// flushed.
func New(config Config) (*Observer, error) {
	filter := config.Filter
	if filter == nil {
		filter = DefaultFilter
	}
	feedbackFilter := config.FeedbackFilter
	if feedbackFilter == nil {
		feedbackFilter = DefaultFeedbackFilter
	}
	clock := config.Clock
	if clock == nil {
		clock = systemClock{}
	}
	timeout := config.ExportTimeout
	if timeout == 0 {
		timeout = DefaultExportTimeout
	}

	observer := &Observer{
		filter:      filter,
		feedback:    feedbackFilter,
		clock:       clock,
		spans:       config.Spans,
		logs:        config.Logs,
		metrics:     config.Metrics,
		feedbackEx:  config.Feedback,
		repository:  config.Repository,
		onError:     config.ErrorHandler,
		batchSize:   config.BatchSize,
		timeout:     timeout,
		synchronous: config.QueueSize < 0,
		traces:      make(map[lebro.RunID]runTraceRef),
		done:        make(chan struct{}),
	}

	tracer, err := NewTracer(TracerConfig{
		Sink:       SpanSinkFunc(observer.onSpanEnd),
		IDs:        config.IDs,
		DeltaLimit: config.DeltaLimit,
	})
	if err != nil {
		return nil, err
	}
	observer.tracer = tracer

	if !observer.synchronous {
		size := config.QueueSize
		if size == 0 {
			size = DefaultQueueSize
		}
		observer.queue = make(chan Span, size)
		observer.wg.Add(1)
		go observer.drain()
	}
	return observer, nil
}

var _ lebro.RunListener = (*Observer)(nil)

// OnRunEvent forwards the event to the tracer, which converts it into spans.
// It never blocks on an exporter and never returns an error to the run.
func (o *Observer) OnRunEvent(event lebro.RunEvent) {
	if o == nil {
		return
	}
	o.tracer.OnRunEvent(event)
}

// onSpanEnd receives an ended span from the tracer. It runs on the goroutine
// executing the run, so it must not export: it filters, records the trace
// mapping, and hands the span to the queue.
func (o *Observer) onSpanEnd(span Span) {
	if span.Kind == SpanKindRun {
		o.rememberTrace(span.RunID, span.TraceID, span.SpanID)
	}

	filtered := o.filter(span)
	if filtered.SpanID == "" {
		o.stats.filtered.Add(1)
		return
	}

	o.emissionMu.Lock()
	if o.closed {
		o.emissionMu.Unlock()
		o.stats.dropped.Add(1)
		return
	}
	if o.synchronous {
		o.emissionMu.Unlock()
		o.export([]Span{filtered})
		return
	}
	select {
	case o.queue <- filtered:
	default:
		// The queue is full: a slow exporter must not become the run's
		// problem, so drop rather than block. The counter and the dropped
		// metric make the loss visible.
		o.stats.dropped.Add(1)
	}
	o.emissionMu.Unlock()
}

// drain exports queued spans until Close. It batches whatever is already
// buffered so a burst becomes one exporter call rather than many.
func (o *Observer) drain() {
	defer o.wg.Done()
	for {
		select {
		case span := <-o.queue:
			o.export(o.collect(span))
		case <-o.done:
			// Flush what is buffered before returning so Close does not
			// silently discard the tail of a run.
			for {
				select {
				case span := <-o.queue:
					o.export(o.collect(span))
				default:
					return
				}
			}
		}
	}
}

// collect drains spans already buffered alongside first, up to BatchSize.
func (o *Observer) collect(first Span) []Span {
	batch := []Span{first}
	limit := o.batchSize
	for limit <= 0 || len(batch) < limit {
		select {
		case span := <-o.queue:
			batch = append(batch, span)
		default:
			return batch
		}
	}
	return batch
}

// export delivers a batch of filtered spans to every configured destination.
// Each destination is called under its own recover, so one failing exporter does
// not stop the others.
func (o *Observer) export(spans []Span) {
	if len(spans) == 0 {
		return
	}
	if o.spans == nil && o.logs == nil && o.metrics == nil && o.repository == nil {
		return
	}
	o.stats.exported.Add(int64(len(spans)))

	if o.spans != nil {
		o.exportCall("spans", context.Background(), func(ctx context.Context) error { return o.spans.ExportSpans(ctx, cloneSpans(spans)) })
	}
	if o.repository != nil {
		o.exportCall("repository spans", context.Background(), func(ctx context.Context) error { return o.repository.AppendSpans(ctx, cloneSpans(spans)) })
	}
	if o.logs != nil || o.repository != nil {
		records := make([]LogRecord, 0, len(spans))
		for _, span := range spans {
			records = append(records, logForSpan(span))
		}
		if o.logs != nil {
			o.exportCall("logs", context.Background(), func(ctx context.Context) error { return o.logs.ExportLogs(ctx, cloneLogRecords(records)) })
		}
		if o.repository != nil {
			o.exportCall("repository logs", context.Background(), func(ctx context.Context) error { return o.repository.AppendLogs(ctx, cloneLogRecords(records)) })
		}
	}
	if o.metrics != nil {
		metrics := make([]Metric, 0, len(spans)*2)
		for _, span := range spans {
			metrics = append(metrics, metricsForSpan(span)...)
		}
		if dropped := o.counterDelta(&o.stats.dropped, &o.stats.droppedReported); dropped > 0 {
			metrics = append(metrics, Metric{
				Name:      MetricSpansDropped,
				Kind:      MetricKindCounter,
				Value:     dropped,
				Timestamp: o.clock.Now(),
			})
		}
		if failures := o.counterDelta(&o.stats.failures, &o.stats.failuresReported); failures > 0 {
			metrics = append(metrics, Metric{Name: MetricExportFailures, Kind: MetricKindCounter, Value: failures, Timestamp: o.clock.Now()})
		}
		if len(metrics) > 0 {
			o.exportCall("metrics", context.Background(), func(ctx context.Context) error { return o.metrics.ExportMetrics(ctx, cloneMetrics(metrics)) })
		}
	}
}

// RecordFeedback filters and exports a feedback record for a finished run.
//
// It returns an error only when the record itself is unusable — a missing run ID
// or unknown kind — because that is a caller mistake the caller can fix. Export
// failures are reported to the ErrorHandler and are never returned, keeping the
// isolation guarantee identical to span export.
//
// TraceID is resolved from the Observer's record of the run when the caller
// leaves it empty. A run the Observer never saw, or one evicted from its bounded
// history, yields a record correlated by RunID alone.
func (o *Observer) RecordFeedback(ctx context.Context, record FeedbackRecord) error {
	if o == nil {
		return errors.New("lebro/obsv: observer is nil")
	}
	if err := record.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidFeedback, err)
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = o.clock.Now()
	}
	if record.TraceID == "" || record.RunSpanID == "" {
		ref := o.traceFor(record.RunID)
		if record.TraceID == "" {
			record.TraceID = ref.traceID
		}
		if record.RunSpanID == "" {
			record.RunSpanID = ref.runSpanID
		}
	}
	record.Metadata = cloneAttributes(record.Metadata)

	filtered := o.feedback(record)
	if filtered.RunID == "" {
		o.stats.feedbackFiltered.Add(1)
		return nil
	}
	if o.feedbackEx == nil && o.repository == nil {
		return nil
	}
	o.stats.feedbacks.Add(1)

	if ctx == nil {
		ctx = context.Background()
	}
	if o.feedbackEx != nil {
		o.exportCall("feedback", ctx, func(exportCtx context.Context) error {
			return o.feedbackEx.ExportFeedback(exportCtx, []FeedbackRecord{filtered.Clone()})
		})
	}
	if o.repository != nil {
		o.exportCall("repository feedback", ctx, func(exportCtx context.Context) error {
			return o.repository.AppendFeedback(exportCtx, []FeedbackRecord{filtered.Clone()})
		})
	}
	return nil
}

// Stats returns a snapshot of the Observer's export counters.
func (o *Observer) Stats() Stats {
	if o == nil {
		return Stats{}
	}
	return Stats{
		SpansExported:    o.stats.exported.Load(),
		SpansDropped:     o.stats.dropped.Load(),
		SpansFiltered:    o.stats.filtered.Load(),
		ExportFailures:   o.stats.failures.Load(),
		FeedbackExported: o.stats.feedbacks.Load(),
		FeedbackFiltered: o.stats.feedbackFiltered.Load(),
	}
}

// Close stops the export goroutine after flushing buffered spans. It is
// idempotent and safe to call concurrently. Spans emitted after Close are
// dropped and counted rather than panicking on a closed channel.
func (o *Observer) Close() error {
	if o == nil {
		return nil
	}
	o.closeOnce.Do(func() {
		o.emissionMu.Lock()
		o.closed = true
		close(o.done)
		o.emissionMu.Unlock()
		o.wg.Wait()
	})
	return nil
}

// call runs one export operation with its failures contained: a returned error
// and a recovered panic are both reported to the ErrorHandler and counted.
func (o *Observer) call(name string, export func() error) {
	err := func() (err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("lebro/obsv: %s exporter panicked: %v", name, recovered)
			}
		}()
		return export()
	}()
	if err == nil {
		return
	}
	o.stats.failures.Add(1)
	o.report(fmt.Errorf("lebro/obsv: %s export failed: %w", name, err))
}

func (o *Observer) exportCall(name string, parent context.Context, export func(context.Context) error) {
	ctx, cancel := o.exportContextFrom(parent)
	defer cancel()
	o.call(name, func() error { return export(ctx) })
}

func (o *Observer) counterDelta(counter, reported *atomic.Int64) int64 {
	current := counter.Load()
	return current - reported.Swap(current)
}

// report hands an error to the ErrorHandler, containing a panic from the handler
// itself so a faulty handler cannot escalate an export failure.
func (o *Observer) report(err error) {
	if o.onError == nil {
		return
	}
	defer func() { _ = recover() }()
	o.onError(err)
}

func (o *Observer) exportContextFrom(parent context.Context) (context.Context, context.CancelFunc) {
	if o.timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, o.timeout)
}

// runTraceRef records the trace and run root span for a finished run, so
// RecordFeedback can correlate a record from a RunID alone.
type runTraceRef struct {
	traceID   TraceID
	runSpanID SpanID
}

// rememberTrace records a run's trace and root span for later feedback
// correlation, evicting the oldest entry once the bound is reached.
func (o *Observer) rememberTrace(runID lebro.RunID, traceID TraceID, runSpanID SpanID) {
	if runID == "" || traceID == "" {
		return
	}
	o.traceMu.Lock()
	defer o.traceMu.Unlock()
	if _, ok := o.traces[runID]; ok {
		return
	}
	if len(o.traceLR) >= traceMemoryLimit {
		oldest := o.traceLR[0]
		o.traceLR = o.traceLR[1:]
		delete(o.traces, oldest)
	}
	o.traces[runID] = runTraceRef{traceID: traceID, runSpanID: runSpanID}
	o.traceLR = append(o.traceLR, runID)
}

func (o *Observer) traceFor(runID lebro.RunID) runTraceRef {
	o.traceMu.Lock()
	defer o.traceMu.Unlock()
	return o.traces[runID]
}

// systemClock reads the wall clock. It mirrors the runtime's default so an
// Observer needs no clock injection to work.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// FixedIDGenerator returns an IDGenerator producing the given IDs in order,
// falling back to indexed values once exhausted. It makes span identifiers
// assertable in tests.
func FixedIDGenerator(traceIDs []TraceID, spanIDs []SpanID) IDGenerator {
	return &fixedIDGenerator{
		traces: append([]TraceID(nil), traceIDs...),
		spans:  append([]SpanID(nil), spanIDs...),
	}
}

type fixedIDGenerator struct {
	mu       sync.Mutex
	traces   []TraceID
	spans    []SpanID
	traceIdx int
	spanIdx  int
}

func (g *fixedIDGenerator) NewTraceID() TraceID {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.traceIdx++
	if g.traceIdx <= len(g.traces) {
		return g.traces[g.traceIdx-1]
	}
	return TraceID("trace-" + strconv.Itoa(g.traceIdx))
}

func (g *fixedIDGenerator) NewSpanID() SpanID {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.spanIdx++
	if g.spanIdx <= len(g.spans) {
		return g.spans[g.spanIdx-1]
	}
	return SpanID("span-" + strconv.Itoa(g.spanIdx))
}
