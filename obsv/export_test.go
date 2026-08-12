package obsv_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
	"github.com/tesh254/lebro/obsv"
)

// runOutcome is the result of a run, reduced to the values a caller can observe.
type runOutcome struct {
	status   lebro.RunStatus
	messages []lebro.Message
	err      string
}

// runAgentWith executes a fixed agent scenario against the given listener and
// returns the caller-visible outcome. A nil listener produces the baseline: what
// the run does with no observability attached at all.
//
// It must be called from the test goroutine, since it fails the test on a broken
// fixture. Use agentRunner when the run itself has to happen on another
// goroutine.
func runAgentWith(t *testing.T, listener lebro.RunListener) runOutcome {
	t.Helper()
	return agentRunner(t, listener)()
}

// agentRunner builds the scenario on the calling goroutine and returns a closure
// that executes it. Splitting construction from execution keeps every t.Fatalf on
// the test goroutine while letting the run itself block elsewhere.
func agentRunner(t *testing.T, listener lebro.RunListener) func() runOutcome {
	t.Helper()
	registry := newRegistry(t, echoTool{id: "lookup"})
	model := testkit.NewModel(
		testkit.ToolCallResponse(testkit.ToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{"query":"handbook"}`)}),
		testkit.Text("done"),
	)
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "agent", Model: "fixture", Tools: []lebro.ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
		Listener:   listener,
		Clock:      newSteppingClock(time.Millisecond),
		IDSource:   lebro.NewFixedIDSource([]lebro.RunID{"run-1"}, []lebro.StepID{"step-1", "step-2"}),
	})
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}
	return func() runOutcome {
		result, runErr := agent.Run(context.Background(), lebro.RunInput{
			Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "look it up"}},
		})
		outcome := runOutcome{status: result.Status, messages: result.Messages}
		if runErr != nil {
			outcome.err = runErr.Error()
		}
		return outcome
	}
}

// TestExporterFailureNeverChangesRunResult is the ticket's second acceptance
// criterion. Each broken exporter is compared against a baseline run with no
// observer, so the assertion cannot pass vacuously: it fails if observability
// changes the status, the transcript, or the returned error in any way.
func TestExporterFailureNeverChangesRunResult(t *testing.T) {
	baseline := runAgentWith(t, nil)
	if baseline.status != lebro.RunStatusSucceeded {
		t.Fatalf("baseline status = %s, want %s; the scenario must succeed for this test to mean anything", baseline.status, lebro.RunStatusSucceeded)
	}
	if len(baseline.messages) == 0 {
		t.Fatal("baseline produced no messages; the scenario must produce a transcript")
	}

	exportErr := errors.New("backend unreachable")

	tests := []struct {
		name   string
		config obsv.Config
		// wantFailures is whether the broken exporter should be counted as a
		// failure. A blocked exporter that eventually returns nil is not one.
		wantFailures bool
	}{
		{
			name:         "span exporter returns an error",
			config:       obsv.Config{Spans: obsv.SpanExporterFunc(func(context.Context, []obsv.Span) error { return exportErr })},
			wantFailures: true,
		},
		{
			name: "span exporter panics",
			config: obsv.Config{Spans: obsv.SpanExporterFunc(func(context.Context, []obsv.Span) error {
				panic("exporter blew up")
			})},
			wantFailures: true,
		},
		{
			name: "span exporter panics with a nil dereference",
			config: obsv.Config{Spans: obsv.SpanExporterFunc(func(context.Context, []obsv.Span) error {
				var spans *[]obsv.Span
				_ = (*spans)[0]
				return nil
			})},
			wantFailures: true,
		},
		{
			name:         "log exporter returns an error",
			config:       obsv.Config{Logs: obsv.LogExporterFunc(func(context.Context, []obsv.LogRecord) error { return exportErr })},
			wantFailures: true,
		},
		{
			name:         "metric exporter returns an error",
			config:       obsv.Config{Metrics: obsv.MetricExporterFunc(func(context.Context, []obsv.Metric) error { return exportErr })},
			wantFailures: true,
		},
		{
			name:         "repository returns an error",
			config:       obsv.Config{Repository: failingRepository{err: exportErr}},
			wantFailures: true,
		},
		{
			name: "every destination fails at once",
			config: obsv.Config{
				Spans:      obsv.SpanExporterFunc(func(context.Context, []obsv.Span) error { return exportErr }),
				Logs:       obsv.LogExporterFunc(func(context.Context, []obsv.LogRecord) error { panic("logs") }),
				Metrics:    obsv.MetricExporterFunc(func(context.Context, []obsv.Metric) error { return exportErr }),
				Repository: failingRepository{err: exportErr},
			},
			wantFailures: true,
		},
		{
			name: "error handler itself panics",
			config: obsv.Config{
				Spans:        obsv.SpanExporterFunc(func(context.Context, []obsv.Span) error { return exportErr }),
				ErrorHandler: func(error) { panic("handler blew up") },
			},
			wantFailures: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			var reported []error
			config := test.config
			if config.ErrorHandler == nil {
				config.ErrorHandler = func(err error) {
					mu.Lock()
					reported = append(reported, err)
					mu.Unlock()
				}
			}
			// Export synchronously so every failure has certainly happened by
			// the time the run returns. An asynchronous exporter could let a
			// broken destination pass simply by not having run yet.
			observer := newObserver(t, synchronousConfig(config))

			got := runAgentWith(t, observer)
			if got.status != baseline.status {
				t.Errorf("status = %s, want baseline %s", got.status, baseline.status)
			}
			if got.err != baseline.err {
				t.Errorf("error = %q, want baseline %q", got.err, baseline.err)
			}
			if !reflect.DeepEqual(got.messages, baseline.messages) {
				t.Errorf("transcript differs from baseline:\n got %+v\nwant %+v", got.messages, baseline.messages)
			}

			stats := observer.Stats()
			if test.wantFailures && stats.ExportFailures == 0 {
				t.Error("no export failures counted; the broken exporter was never called, so this test proved nothing")
			}
			if stats.SpansExported == 0 {
				t.Error("no spans exported; the scenario produced no observability data")
			}

			// A failure the run swallows must still be visible to the
			// application, otherwise a dead backend looks like a healthy one.
			// The panicking-handler case supplies its own handler, so it
			// collects nothing.
			if test.config.ErrorHandler == nil {
				mu.Lock()
				count := len(reported)
				mu.Unlock()
				if test.wantFailures && count == 0 {
					t.Error("export failed but the ErrorHandler was never called; the failure would be invisible")
				}
			}
		})
	}
}

// TestSlowExporterDoesNotBlockRun checks the queue's purpose: an exporter that
// hangs must not hold up the run. The exporter blocks until the test releases it,
// which it does only after the run has already returned.
func TestSlowExporterDoesNotBlockRun(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	var once sync.Once

	observer := newObserver(t, obsv.Config{
		Spans: obsv.SpanExporterFunc(func(context.Context, []obsv.Span) error {
			once.Do(func() { entered <- struct{}{} })
			<-release
			return nil
		}),
		// A queue of one fills immediately, so the run reaches the drop path
		// rather than waiting for the blocked exporter.
		QueueSize: 1,
	})
	// Release the exporter before Close so the drain goroutine can finish; the
	// helper's Close runs after this cleanup, which is the order t.Cleanup gives.
	t.Cleanup(func() { close(release) })

	baseline := runAgentWith(t, nil)

	run := agentRunner(t, observer)
	done := make(chan runOutcome, 1)
	go func() { done <- run() }()

	select {
	case got := <-done:
		if got.status != baseline.status {
			t.Errorf("status = %s, want baseline %s", got.status, baseline.status)
		}
		if got.err != baseline.err {
			t.Errorf("error = %q, want baseline %q", got.err, baseline.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return while an exporter was blocked; export is not isolated from the run")
	}

	// The blocked exporter really was entered, so the run completed alongside a
	// genuinely stuck backend rather than one that was never reached.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("exporter was never called; the block was not exercised")
	}
}

// TestObserverCountsDroppedSpans checks that queue overflow is visible. A silent
// drop reads as complete coverage, which is worse than a reported gap.
//
// The overflow is forced rather than raced for: the exporter blocks until the
// test releases it, and the run emits far more spans than the queue plus the one
// in-flight batch can absorb, so at least one drop is certain regardless of
// scheduling.
func TestObserverCountsDroppedSpans(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once

	const queueSize = 2
	observer := newObserver(t, obsv.Config{
		Spans: obsv.SpanExporterFunc(func(context.Context, []obsv.Span) error {
			once.Do(func() { close(entered) })
			<-release
			return nil
		}),
		QueueSize: queueSize,
	})
	t.Cleanup(func() { close(release) })

	// Emit one span and wait for the drain goroutine to be parked inside the
	// blocked exporter. From here the queue can never be serviced again, so
	// every subsequent span either fills the queue or is dropped.
	emitRunSpans(observer, 1)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("exporter was never entered; the queue was never serviced")
	}

	const emitted = queueSize + 8
	emitRunSpans(observer, emitted)

	stats := observer.Stats()
	if stats.SpansDropped == 0 {
		t.Errorf("emitted %d spans into a queue of %d behind a blocked exporter but none were counted as dropped; stats = %+v", emitted, queueSize, stats)
	}
}

// emitRunSpans drives count complete run lifecycles through the listener, ending
// one run span each. It bypasses the agent so the span count is exact.
func emitRunSpans(listener lebro.RunListener, count int) {
	for i := 0; i < count; i++ {
		runID := lebro.RunID("drop-run-" + strconv.Itoa(i))
		listener.OnRunEvent(lebro.RunEvent{
			Type:      lebro.RunEventStarted,
			RunID:     runID,
			Timestamp: fixtureStart,
		})
		listener.OnRunEvent(lebro.RunEvent{
			Type:      lebro.RunEventSucceeded,
			RunID:     runID,
			Status:    lebro.RunStatusSucceeded,
			Timestamp: fixtureStart.Add(time.Millisecond),
		})
	}
}

// TestObserverAfterCloseDoesNotPanic checks that a listener still attached to a
// running agent after Close degrades to dropping spans. A closed-channel panic
// here would crash the run, which is the failure mode the whole design forbids.
func TestObserverAfterCloseDoesNotPanic(t *testing.T) {
	exporter := obsv.NewMemorySpanExporter()
	observer, err := obsv.New(obsv.Config{Spans: exporter})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := observer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := observer.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}

	baseline := runAgentWith(t, nil)
	got := runAgentWith(t, observer)
	if got.status != baseline.status {
		t.Errorf("status = %s, want baseline %s", got.status, baseline.status)
	}
	if got.err != baseline.err {
		t.Errorf("error = %q, want baseline %q", got.err, baseline.err)
	}
}

// TestObserverFlushesBufferedSpansOnClose checks that Close does not discard the
// tail of a run. A Close that returned before draining would silently lose the
// terminal spans, which are the ones a cost report needs.
func TestObserverFlushesBufferedSpansOnClose(t *testing.T) {
	exporter := obsv.NewMemorySpanExporter()
	observer, err := obsv.New(obsv.Config{Spans: exporter})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	runAgentWith(t, observer)

	if err := observer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	spans := exporter.Spans()
	if len(spans) == 0 {
		t.Fatal("Close() discarded every buffered span")
	}
	if len(spansByKind(spans, obsv.SpanKindRun)) == 0 {
		t.Errorf("Close() discarded the run span; got %s", describeSpans(spans))
	}
	stats := observer.Stats()
	if stats.SpansDropped != 0 {
		t.Errorf("dropped %d spans with a default queue and a fast exporter", stats.SpansDropped)
	}
}

// TestExporterOwnsTheSpansItReceives checks the documented ownership rule: an
// exporter may retain and mutate its slice without corrupting another
// destination's view. Two exporters run, the first mutates everything it gets,
// and the second must still see intact data.
func TestExporterOwnsTheSpansItReceives(t *testing.T) {
	var mutated []obsv.Span
	second := obsv.NewMemorySpanExporter()

	observer := newObserver(t, synchronousConfig(obsv.Config{
		Spans: obsv.SpanExporterFunc(func(_ context.Context, spans []obsv.Span) error {
			for i := range spans {
				spans[i].Name = "mutated"
				spans[i].TraceID = "mutated"
				for key := range spans[i].Attributes {
					spans[i].Attributes[key] = "mutated"
				}
			}
			mutated = append(mutated, spans...)
			return nil
		}),
		Repository: repositoryFor(second),
	}))

	runAgentWith(t, observer)

	if len(mutated) == 0 {
		t.Fatal("first exporter received no spans")
	}
	stored := second.Spans()
	if len(stored) == 0 {
		t.Fatal("second destination received no spans")
	}
	for _, span := range stored {
		if span.Name == "mutated" || span.TraceID == "mutated" {
			t.Fatalf("span %s was corrupted by another exporter's mutation", span.SpanID)
		}
		for key, value := range span.Attributes {
			if value == "mutated" {
				t.Fatalf("span %s attribute %q was corrupted by another exporter's mutation", span.SpanID, key)
			}
		}
	}
}

// failingRepository fails every write. Reads return nothing, which is all the
// isolation tests need.
type failingRepository struct {
	err error
}

func (r failingRepository) AppendSpans(context.Context, []obsv.Span) error     { return r.err }
func (r failingRepository) AppendLogs(context.Context, []obsv.LogRecord) error { return r.err }
func (r failingRepository) AppendFeedback(context.Context, []obsv.FeedbackRecord) error {
	return r.err
}
func (r failingRepository) SpansByTrace(context.Context, obsv.TraceID) ([]obsv.Span, error) {
	return nil, r.err
}
func (r failingRepository) SpansByRun(context.Context, obsv.TraceID, lebro.RunID, obsv.SpanID) ([]obsv.Span, error) {
	return nil, r.err
}
func (r failingRepository) FeedbackByRun(context.Context, obsv.TraceID, lebro.RunID, obsv.SpanID) ([]obsv.FeedbackRecord, error) {
	return nil, r.err
}

// spanOnlyRepository adapts a span exporter to the Repository interface so a
// test can attach a second independent span destination.
type spanOnlyRepository struct {
	spans *obsv.MemorySpanExporter
}

func repositoryFor(spans *obsv.MemorySpanExporter) obsv.Repository {
	return spanOnlyRepository{spans: spans}
}

func (r spanOnlyRepository) AppendSpans(ctx context.Context, spans []obsv.Span) error {
	return r.spans.ExportSpans(ctx, spans)
}
func (r spanOnlyRepository) AppendLogs(context.Context, []obsv.LogRecord) error { return nil }
func (r spanOnlyRepository) AppendFeedback(context.Context, []obsv.FeedbackRecord) error {
	return nil
}
func (r spanOnlyRepository) SpansByTrace(context.Context, obsv.TraceID) ([]obsv.Span, error) {
	return nil, nil
}
func (r spanOnlyRepository) SpansByRun(context.Context, obsv.TraceID, lebro.RunID, obsv.SpanID) ([]obsv.Span, error) {
	return nil, nil
}
func (r spanOnlyRepository) FeedbackByRun(context.Context, obsv.TraceID, lebro.RunID, obsv.SpanID) ([]obsv.FeedbackRecord, error) {
	return nil, nil
}
