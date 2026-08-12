package obsv_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/obsv"
)

// TestRecordFeedbackCorrelatesToItsRun checks the ordinary path: a record naming
// only a RunID is filled in with the trace and run span the Observer saw, so the
// repository can retrieve it.
func TestRecordFeedbackCorrelatesToItsRun(t *testing.T) {
	repository := obsv.NewMemoryRepository()
	exporter := obsv.NewMemoryFeedbackExporter()
	observer := newObserver(t, synchronousConfig(obsv.Config{
		Feedback:       exporter,
		Repository:     repository,
		FeedbackFilter: obsv.PassthroughFeedbackFilter,
	}))

	emitRunSpans(observer, 1)
	run := lebro.RunID("drop-run-0")

	if err := observer.RecordFeedback(context.Background(), obsv.FeedbackRecord{
		RunID:   run,
		Kind:    obsv.FeedbackKindRating,
		Score:   0.75,
		Comment: "useful answer",
	}); err != nil {
		t.Fatalf("RecordFeedback() error = %v", err)
	}

	records := exporter.Records()
	if len(records) != 1 {
		t.Fatalf("exported %d feedback records, want 1", len(records))
	}
	got := records[0]
	if got.TraceID == "" {
		t.Error("feedback record was not given a TraceID; it cannot be correlated to the run's spans")
	}
	if got.RunSpanID == "" {
		t.Error("feedback record was not given a RunSpanID")
	}
	if got.Score != 0.75 {
		t.Errorf("score = %v, want 0.75", got.Score)
	}
	if got.CreatedAt.IsZero() {
		t.Error("feedback record has no CreatedAt")
	}

	// The stored record is retrievable by exactly the identifiers the Observer
	// filled in — which is the whole point of filling them in.
	stored, err := repository.FeedbackByRun(context.Background(), got.TraceID, got.RunID, got.RunSpanID)
	if err != nil {
		t.Fatalf("FeedbackByRun() error = %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("FeedbackByRun() returned %d records, want 1", len(stored))
	}
}

// TestRecordFeedbackDoesNotMixOccurrences pins the fix for a correlation defect.
//
// Run IDs are not unique across independently-configured primitives, so one RunID
// can name several occurrences. The Observer previously remembered only the first
// and filled any missing identifier from it — so a record supplying the *second*
// occurrence's TraceID received the *first* occurrence's run span. That pairing
// matches no stored record, making the feedback unretrievable.
func TestRecordFeedbackDoesNotMixOccurrences(t *testing.T) {
	repository := obsv.NewMemoryRepository()
	exporter := obsv.NewMemoryFeedbackExporter()
	observer := newObserver(t, synchronousConfig(obsv.Config{
		Feedback:       exporter,
		Repository:     repository,
		FeedbackFilter: obsv.PassthroughFeedbackFilter,
	}))

	// Two runs sharing one RunID, each in its own trace.
	const shared = lebro.RunID("shared-run")
	emitRun := func() {
		observer.OnRunEvent(lebro.RunEvent{Type: lebro.RunEventStarted, RunID: shared, Timestamp: fixtureStart})
		observer.OnRunEvent(lebro.RunEvent{
			Type:      lebro.RunEventSucceeded,
			RunID:     shared,
			Status:    lebro.RunStatusSucceeded,
			Timestamp: fixtureStart.Add(time.Millisecond),
		})
	}
	emitRun()
	emitRun()

	spans := repositorySpansForRun(t, repository, shared)
	if len(spans) != 2 {
		t.Fatalf("stored %d run spans for the shared RunID, want 2 occurrences", len(spans))
	}
	first, second := spans[0], spans[1]
	if first.TraceID == second.TraceID {
		t.Fatalf("both occurrences share trace %q; the test needs distinct traces", first.TraceID)
	}

	// A record naming the second occurrence's trace must not receive the first
	// occurrence's run span.
	if err := observer.RecordFeedback(context.Background(), obsv.FeedbackRecord{
		RunID:   shared,
		TraceID: second.TraceID,
		Kind:    obsv.FeedbackKindThumb,
		Score:   1,
	}); err != nil {
		t.Fatalf("RecordFeedback() error = %v", err)
	}

	records := exporter.Records()
	if len(records) != 1 {
		t.Fatalf("exported %d feedback records, want 1", len(records))
	}
	got := records[0]
	if got.RunSpanID == first.SpanID {
		t.Errorf("record for trace %q received the other occurrence's run span %s; it is unretrievable",
			second.TraceID, first.SpanID)
	}
	if got.RunSpanID != second.SpanID {
		t.Errorf("RunSpanID = %q, want the matching occurrence's run span %q", got.RunSpanID, second.SpanID)
	}

	// It round-trips through the repository, which filters on all three
	// identifiers.
	stored, err := repository.FeedbackByRun(context.Background(), second.TraceID, shared, second.SpanID)
	if err != nil {
		t.Fatalf("FeedbackByRun() error = %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("FeedbackByRun() returned %d records, want 1; the record was stored with a mismatched pairing", len(stored))
	}
}

// TestRecordFeedbackLeavesAmbiguousRunUnqualified checks that an unresolvable
// RunID is left alone rather than guessed. Two occurrences and nothing to choose
// between them means no run span can be attributed; inventing one would store a
// record no query can find.
func TestRecordFeedbackLeavesAmbiguousRunUnqualified(t *testing.T) {
	exporter := obsv.NewMemoryFeedbackExporter()
	observer := newObserver(t, synchronousConfig(obsv.Config{
		Feedback:       exporter,
		FeedbackFilter: obsv.PassthroughFeedbackFilter,
	}))

	const shared = lebro.RunID("ambiguous-run")
	for i := 0; i < 2; i++ {
		observer.OnRunEvent(lebro.RunEvent{Type: lebro.RunEventStarted, RunID: shared, Timestamp: fixtureStart})
		observer.OnRunEvent(lebro.RunEvent{
			Type:      lebro.RunEventSucceeded,
			RunID:     shared,
			Status:    lebro.RunStatusSucceeded,
			Timestamp: fixtureStart.Add(time.Millisecond),
		})
	}

	if err := observer.RecordFeedback(context.Background(), obsv.FeedbackRecord{
		RunID: shared,
		Kind:  obsv.FeedbackKindComment,
	}); err != nil {
		t.Fatalf("RecordFeedback() error = %v", err)
	}

	records := exporter.Records()
	if len(records) != 1 {
		t.Fatalf("exported %d feedback records, want 1", len(records))
	}
	if got := records[0]; got.TraceID != "" || got.RunSpanID != "" {
		t.Errorf("ambiguous RunID resolved to trace %q span %q; it should stay unqualified rather than guess",
			got.TraceID, got.RunSpanID)
	}
}

// TestRecordFeedbackRejectsUnusableRecords checks the one error RecordFeedback
// reports to its caller. A caller who omitted the RunID can fix that; export
// failures they cannot, which is why those stay internal.
func TestRecordFeedbackRejectsUnusableRecords(t *testing.T) {
	observer := newObserver(t, synchronousConfig(obsv.Config{Feedback: obsv.NewMemoryFeedbackExporter()}))

	tests := []struct {
		name   string
		record obsv.FeedbackRecord
	}{
		{"no run ID", obsv.FeedbackRecord{Kind: obsv.FeedbackKindRating}},
		{"no kind", obsv.FeedbackRecord{RunID: "run-1"}},
		{"unknown kind", obsv.FeedbackRecord{RunID: "run-1", Kind: "telepathy"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := observer.RecordFeedback(context.Background(), test.record)
			if err == nil {
				t.Fatal("RecordFeedback() error = nil, want a validation failure")
			}
			if !errors.Is(err, obsv.ErrInvalidFeedback) {
				t.Errorf("error = %v, want it to wrap ErrInvalidFeedback", err)
			}
		})
	}
}

// TestRecordFeedbackIsolatesExporterFailure checks that the isolation guarantee
// covers feedback too: a broken exporter is reported, not returned.
func TestRecordFeedbackIsolatesExporterFailure(t *testing.T) {
	exportErr := errors.New("feedback backend down")
	var reported []error
	observer := newObserver(t, synchronousConfig(obsv.Config{
		Feedback: obsv.FeedbackExporterFunc(func(context.Context, []obsv.FeedbackRecord) error {
			return exportErr
		}),
		ErrorHandler: func(err error) { reported = append(reported, err) },
	}))

	if err := observer.RecordFeedback(context.Background(), obsv.FeedbackRecord{
		RunID: "run-1",
		Kind:  obsv.FeedbackKindRating,
		Score: 1,
	}); err != nil {
		t.Fatalf("RecordFeedback() returned the export failure %v; export errors must stay internal", err)
	}
	if len(reported) == 0 {
		t.Error("export failed but the ErrorHandler was never called")
	}
	if observer.Stats().ExportFailures == 0 {
		t.Error("export failure was not counted")
	}
}

// TestDefaultFeedbackFilterDropsComment checks the default policy: a reviewer's
// free text is sensitive, so a zero-valued Config exports the score without it.
func TestDefaultFeedbackFilterDropsComment(t *testing.T) {
	exporter := obsv.NewMemoryFeedbackExporter()
	observer := newObserver(t, synchronousConfig(obsv.Config{Feedback: exporter}))

	if err := observer.RecordFeedback(context.Background(), obsv.FeedbackRecord{
		RunID:   "run-1",
		Kind:    obsv.FeedbackKindCorrection,
		Comment: secretPayload,
	}); err != nil {
		t.Fatalf("RecordFeedback() error = %v", err)
	}

	records := exporter.Records()
	if len(records) != 1 {
		t.Fatalf("exported %d feedback records, want 1", len(records))
	}
	if records[0].Comment != "" {
		t.Errorf("DefaultFeedbackFilter kept the comment %q", records[0].Comment)
	}
	if records[0].Kind != obsv.FeedbackKindCorrection {
		t.Errorf("kind = %q, want it preserved", records[0].Kind)
	}
}

// repositorySpansForRun returns the run-kind spans stored for a RunID, in
// insertion order.
func repositorySpansForRun(t *testing.T, repository *obsv.MemoryRepository, runID lebro.RunID) []obsv.Span {
	t.Helper()
	var matched []obsv.Span
	for _, span := range repository.Spans() {
		if span.Kind == obsv.SpanKindRun && span.RunID == runID {
			matched = append(matched, span)
		}
	}
	return matched
}
