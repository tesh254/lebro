package studio_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tesh254/lebro/obsv"
	"github.com/tesh254/lebro/studio"
)

// appendSpans writes spans straight into a MemoryRepository, which is the
// TraceLister the trace views read. It lets the trace tests build an exact span
// stream without driving a whole run, so an assertion about ordering or
// summarization is not entangled with agent behavior.
func appendSpans(t *testing.T, repo *obsv.MemoryRepository, spans ...obsv.Span) {
	t.Helper()
	if err := repo.AppendSpans(context.Background(), spans); err != nil {
		t.Fatalf("append spans: %v", err)
	}
}

func at(seconds int) time.Time {
	return time.Date(2026, 1, 1, 0, 0, seconds, 0, time.UTC)
}

func TestListTracesSummarizesAndOrdersNewestFirst(t *testing.T) {
	repo := obsv.NewMemoryRepository()
	// Two traces recorded oldest-first; the listing must return newest-first.
	appendSpans(t, repo,
		obsv.Span{TraceID: "old", SpanID: "o-run", Kind: obsv.SpanKindRun, Name: "first run", Status: obsv.SpanStatusOK, Start: at(1)},
		obsv.Span{TraceID: "old", SpanID: "o-model", ParentSpanID: "o-run", Kind: obsv.SpanKindModel, Name: "model", Status: obsv.SpanStatusOK, Start: at(2)},
		obsv.Span{TraceID: "new", SpanID: "n-run", Kind: obsv.SpanKindRun, Name: "second run", Status: obsv.SpanStatusError, Start: at(5)},
	)

	studioServer := newStudio(t, studio.Config{Traces: repo})

	var body studio.TraceListResponse
	getJSON(t, studioServer, "/api/studio/traces", &body)

	if len(body.Traces) != 2 {
		t.Fatalf("want 2 traces, got %d", len(body.Traces))
	}
	// Newest first: the "new" trace, started at second 5, leads.
	if body.Traces[0].TraceID != "new" {
		t.Fatalf("want newest trace first, got %q", body.Traces[0].TraceID)
	}
	if body.Traces[0].Name != "second run" || body.Traces[0].Status != "error" {
		t.Fatalf("summary not taken from root span: %+v", body.Traces[0])
	}
	if body.Traces[1].TraceID != "old" || body.Traces[1].SpanCount != 2 {
		t.Fatalf("want old trace with 2 spans second, got %+v", body.Traces[1])
	}
}

func TestGetTraceReturnsSpansInTimelineOrder(t *testing.T) {
	repo := obsv.NewMemoryRepository()
	appendSpans(t, repo,
		obsv.Span{TraceID: "t1", SpanID: "run", Kind: obsv.SpanKindRun, Name: "run", Status: obsv.SpanStatusOK, Start: at(1)},
		obsv.Span{TraceID: "t1", SpanID: "tool", ParentSpanID: "run", Kind: obsv.SpanKindTool, Name: "search", Status: obsv.SpanStatusOK, Start: at(2)},
		obsv.Span{TraceID: "other", SpanID: "x", Kind: obsv.SpanKindRun, Name: "unrelated", Start: at(3)},
	)

	studioServer := newStudio(t, studio.Config{Traces: repo})

	var body studio.TraceResponse
	getJSON(t, studioServer, "/api/studio/traces/t1", &body)

	if body.TraceID != "t1" {
		t.Fatalf("want trace id t1, got %q", body.TraceID)
	}
	if len(body.Spans) != 2 {
		t.Fatalf("want 2 spans for t1, got %d", len(body.Spans))
	}
	// The order is the insertion order the ordered-event view renders top to
	// bottom: run first, then the tool call under it.
	if body.Spans[0].Kind != obsv.SpanKindRun || body.Spans[1].Kind != obsv.SpanKindTool {
		t.Fatalf("spans out of order: %v then %v", body.Spans[0].Kind, body.Spans[1].Kind)
	}
}

func TestGetUnknownTraceIsEmptyNotError(t *testing.T) {
	studioServer := newStudio(t, studio.Config{Traces: obsv.NewMemoryRepository()})

	recorder := httptest.NewRecorder()
	studioServer.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/studio/traces/missing", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200 for unknown trace, got %d", recorder.Code)
	}
	var body studio.TraceResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Spans) != 0 {
		t.Fatalf("want no spans for unknown trace, got %d", len(body.Spans))
	}
}

func TestTraceViewsPresentWithoutTraceLister(t *testing.T) {
	// A Studio built with no Traces still serves the trace routes, empty, rather
	// than 500ing on a nil lister.
	studioServer := newStudio(t, studio.Config{})

	var list studio.TraceListResponse
	getJSON(t, studioServer, "/api/studio/traces", &list)
	if len(list.Traces) != 0 {
		t.Fatalf("want empty trace list, got %d", len(list.Traces))
	}
}

// nilUnsafeLister is a TraceLister whose methods panic on a nil receiver, unlike
// obsv.MemoryRepository which guards against it. It proves the typed-nil check
// prevents a handler from ever calling through a nil-able interface value.
type nilUnsafeLister struct{ spans []obsv.Span }

func (l *nilUnsafeLister) Spans() []obsv.Span { return l.spans }
func (l *nilUnsafeLister) SpansByTrace(context.Context, obsv.TraceID) ([]obsv.Span, error) {
	return l.spans, nil
}

func TestTraceViewsTreatTypedNilListerAsEmpty(t *testing.T) {
	// A typed-nil TraceLister — a nil pointer boxed in the interface — is not
	// equal to a nil interface, so a naive nil check would pass it through and a
	// handler would dereference it. With a nil-unsafe implementation that is a
	// panic; the views must treat the value as absent and serve empty instead.
	var nilLister *nilUnsafeLister
	studioServer := newStudio(t, studio.Config{Traces: nilLister})

	var list studio.TraceListResponse
	getJSON(t, studioServer, "/api/studio/traces", &list)
	if len(list.Traces) != 0 {
		t.Fatalf("want empty trace list for typed-nil lister, got %d", len(list.Traces))
	}

	recorder := httptest.NewRecorder()
	studioServer.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/studio/traces/anything", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200 for typed-nil lister, got %d", recorder.Code)
	}
}

func TestGetTracePlacesParentBeforeChildOnStartTie(t *testing.T) {
	// When a child shares its parent's exact start instant, the timeline must
	// still put the parent first. SpanID has no ordering contract, so the tie
	// cannot be broken on it; it is broken on ancestry depth. Insertion order
	// here is child-before-root, so a correct fix must reorder them.
	tie := at(1)
	repo := obsv.NewMemoryRepository()
	// The child's SpanID sorts lexically before the root's, so a tie-break on
	// SpanID would wrongly place the child first; only an ancestry-aware order
	// puts the root first. Insertion order is also child-first.
	appendSpans(t, repo,
		obsv.Span{TraceID: "t", SpanID: "aaa-model", ParentSpanID: "zzz-run", Kind: obsv.SpanKindModel, Name: "model", Start: tie, Status: obsv.SpanStatusOK},
		obsv.Span{TraceID: "t", SpanID: "zzz-run", Kind: obsv.SpanKindRun, Name: "run", Start: tie, Status: obsv.SpanStatusOK},
	)

	studioServer := newStudio(t, studio.Config{Traces: repo})

	var trace studio.TraceResponse
	getJSON(t, studioServer, "/api/studio/traces/t", &trace)
	if len(trace.Spans) != 2 {
		t.Fatalf("want 2 spans, got %d", len(trace.Spans))
	}
	if trace.Spans[0].Kind != obsv.SpanKindRun {
		t.Fatalf("parent must precede child on a start tie, got %q first", trace.Spans[0].Kind)
	}
}

func TestListTracesKeepsFirstRootAuthoritative(t *testing.T) {
	// Two parentless spans in one trace: the first root names the trace and
	// carries its status; a later parentless span must not overwrite them.
	repo := obsv.NewMemoryRepository()
	appendSpans(t, repo,
		obsv.Span{TraceID: "t", SpanID: "run", Kind: obsv.SpanKindRun, Name: "authoritative run", Status: obsv.SpanStatusOK, Start: at(1)},
		obsv.Span{TraceID: "t", SpanID: "stray", Kind: obsv.SpanKindRun, Name: "stray root", Status: obsv.SpanStatusError, Start: at(2)},
	)

	studioServer := newStudio(t, studio.Config{Traces: repo})

	var list studio.TraceListResponse
	getJSON(t, studioServer, "/api/studio/traces", &list)
	if len(list.Traces) != 1 {
		t.Fatalf("want 1 trace, got %d", len(list.Traces))
	}
	if list.Traces[0].Name != "authoritative run" || list.Traces[0].Status != "ok" {
		t.Fatalf("first root should stay authoritative, got name=%q status=%q", list.Traces[0].Name, list.Traces[0].Status)
	}
}
