package studio

import (
	"context"
	"net/http"
	"sort"

	"github.com/tesh254/lebro/obsv"
)

// TraceLister reads recorded observability spans for the Studio trace views. It
// is the minimal read surface Studio needs, deliberately narrower than
// obsv.Repository: Studio never writes spans, and it needs to enumerate traces,
// which the Repository write contract does not expose. obsv.MemoryRepository
// satisfies it through its Spans and SpansByTrace methods, so the common local
// setup wires with no adapter.
//
// Spans returns every recorded span in insertion order; the returned slice is
// owned by the caller. SpansByTrace returns the spans of one trace in insertion
// order. A nil TraceLister is treated as an empty one, so the trace views are
// present but empty when a program runs Studio without observability wired.
type TraceLister interface {
	Spans() []obsv.Span
	SpansByTrace(ctx context.Context, traceID obsv.TraceID) ([]obsv.Span, error)
}

// TraceSummary describes one recorded trace in the trace listing. It is derived
// from the trace's spans rather than stored, so a backend that only records
// spans needs no extra bookkeeping to appear here.
type TraceSummary struct {
	TraceID string `json:"trace_id"`
	// Name is the root run span's name, the human-facing label for the trace.
	Name string `json:"name"`
	// Status is the root span's status, so a caller can spot failed runs
	// without opening the trace.
	Status string `json:"status"`
	// SpanCount is how many spans the trace contains.
	SpanCount int `json:"span_count"`
	// Start is the earliest span start in the trace, the point the ordered
	// listing sorts on so the newest run is first.
	Start string `json:"start,omitempty"`
}

// TraceListResponse is the body of GET /api/studio/traces.
type TraceListResponse struct {
	Traces []TraceSummary `json:"traces"`
}

// TraceResponse is the body of GET /api/studio/traces/{id}. Spans are ordered so
// a client can render the run's events top to bottom: the ordered step, model,
// and tool spans that record what happened and in what order.
type TraceResponse struct {
	TraceID string      `json:"trace_id"`
	Spans   []obsv.Span `json:"spans"`
}

// handleListTraces enumerates recorded traces, newest first. It groups the flat
// span stream by TraceID and summarizes each group from its root span, so a
// backend that only records spans needs no separate trace index.
func (s *Studio) handleListTraces(w http.ResponseWriter, _ *http.Request) {
	summaries := summarizeTraces(s.tracesOrEmpty().Spans())
	writeJSON(w, http.StatusOK, TraceListResponse{Traces: summaries})
}

// handleGetTrace returns one trace's spans as an ordered event timeline. An
// unknown trace is not an error: a trace with no spans is an empty list,
// matching the listing, which avoids a race where a trace listed a moment ago
// 404s because its spans were pruned.
func (s *Studio) handleGetTrace(w http.ResponseWriter, r *http.Request) {
	traceID := obsv.TraceID(r.PathValue("id"))
	spans, err := s.tracesOrEmpty().SpansByTrace(r.Context(), traceID)
	if err != nil {
		writeStudioError(w, http.StatusInternalServerError, "trace_read_failed")
		return
	}
	writeJSON(w, http.StatusOK, TraceResponse{TraceID: string(traceID), Spans: orderSpans(spans)})
}

// orderSpans arranges a trace's spans as the timeline a developer reads: by
// start time, so the run root comes first and each model and tool call follows
// in the order it happened. The Repository returns spans in export order, which
// ends a child before its parent, so the raw slice is not the order to render.
//
// On an exact start-time tie the comparison cannot rely on SpanID, which has no
// ordering contract, and must not place a child before its parent. It breaks the
// tie by ancestry alone: an ancestor sorts before its descendant. Spans that are
// not on the same ancestry line are left in their incoming order, which a stable
// sort preserves — the comparison does not reorder unrelated branches by depth.
// The result never renders a span above one of its own ancestors, whatever ID
// scheme the backend uses.
func orderSpans(spans []obsv.Span) []obsv.Span {
	ordered := make([]obsv.Span, len(spans))
	copy(ordered, spans)

	parent := make(map[obsv.SpanID]obsv.SpanID, len(ordered))
	for _, span := range ordered {
		parent[span.SpanID] = span.ParentSpanID
	}
	// isAncestor reports whether a is an ancestor of b, walking b's parent chain.
	// The walk is bounded by the span count so a malformed parent cycle cannot
	// loop forever.
	isAncestor := func(a, b obsv.SpanID) bool {
		steps := 0
		for current := parent[b]; current != ""; current = parent[current] {
			if current == a {
				return true
			}
			steps++
			if steps > len(ordered) {
				break
			}
		}
		return false
	}

	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].Start.Equal(ordered[j].Start) {
			return ordered[i].Start.Before(ordered[j].Start)
		}
		// Only an ancestor/descendant pair is reordered; unrelated spans compare
		// equal and keep their insertion order under the stable sort.
		if isAncestor(ordered[i].SpanID, ordered[j].SpanID) {
			return true
		}
		return false
	})
	return ordered
}

// summarizeTraces reduces a flat span stream to one summary per trace, sorted
// newest first by start time. Ties break on TraceID so the order is stable for
// a client that polls the listing.
func summarizeTraces(spans []obsv.Span) []TraceSummary {
	type accumulator struct {
		summary TraceSummary
		hasRoot bool
	}
	byTrace := map[obsv.TraceID]*accumulator{}
	order := []obsv.TraceID{}
	for _, span := range spans {
		acc, ok := byTrace[span.TraceID]
		if !ok {
			acc = &accumulator{summary: TraceSummary{TraceID: string(span.TraceID)}}
			byTrace[span.TraceID] = acc
			order = append(order, span.TraceID)
		}
		acc.summary.SpanCount++
		// The root run span names the trace and carries the outcome the listing
		// shows. Prefer it, but keep the earliest start across all spans so a
		// trace still sorts sensibly if its root has not been recorded yet. The
		// first root seen stays authoritative: a trace has one run root, and a
		// later parentless span (a sibling recorded without its parent) must not
		// overwrite the trace's name and status.
		if span.IsRoot() && !acc.hasRoot {
			acc.summary.Name = span.Name
			acc.summary.Status = string(span.Status)
			acc.hasRoot = true
		}
		start := span.Start.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
		if acc.summary.Start == "" || start < acc.summary.Start {
			acc.summary.Start = start
		}
	}

	summaries := make([]TraceSummary, 0, len(order))
	for _, id := range order {
		summaries = append(summaries, byTrace[id].summary)
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Start != summaries[j].Start {
			// Newest first: a later start sorts earlier.
			return summaries[i].Start > summaries[j].Start
		}
		return summaries[i].TraceID < summaries[j].TraceID
	})
	return summaries
}
