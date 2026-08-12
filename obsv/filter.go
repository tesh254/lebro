package obsv

import "strings"

// Filter rewrites a span before it reaches any exporter. It runs inside the
// Observer, ahead of span, log, and metric export alike, so a filtered field
// cannot reappear through a different signal.
//
// A filter owns the span it is given: the Observer passes a copy and does not
// read it again. Mutating the argument's maps and returning it is safe.
//
// Returning the zero Span suppresses the span entirely — it is not exported and
// produces no log record or metric.
type Filter func(Span) Span

// FeedbackFilter rewrites a feedback record before export, with the same
// ownership rules as Filter. Returning the zero FeedbackRecord suppresses the
// record.
type FeedbackFilter func(FeedbackRecord) FeedbackRecord

// DefaultFilter removes model- and tool-supplied payloads while keeping the
// structure that makes a trace useful: identifiers, parentage, timings, token
// usage, status, and errors.
//
// It drops every attribute under the SensitiveAttr prefix, on the span and on
// its events, which is where streamed text and structured output are recorded.
// It keeps span events themselves, so the shape of a stream stays visible
// without its content.
//
// This is the default precisely because it is not the empty policy: a nil Filter
// selects it, so a zero-valued Config exports less rather than more. Pass
// PassthroughFilter to opt out deliberately.
//
// It does not attempt to detect secrets in values it keeps. Tool IDs, step IDs,
// and finish reasons are developer-supplied identifiers, not payloads; an
// application that puts sensitive data in an identifier should compose its own
// filter.
func DefaultFilter(span Span) Span {
	span.Attributes = dropSensitive(span.Attributes)
	for i := range span.Events {
		span.Events[i].Attributes = dropSensitive(span.Events[i].Attributes)
	}
	return span
}

// PassthroughFilter exports every span unchanged, including streamed text and
// structured output. Use it only when the observability backend is as trusted as
// the process being observed.
func PassthroughFilter(span Span) Span { return span }

// DropEventsFilter removes every span event, keeping the span itself. It is a
// useful second filter for a high-throughput streaming workload where the
// per-delta events are volume without value.
func DropEventsFilter(span Span) Span {
	span.Events = nil
	return span
}

// DefaultFeedbackFilter removes reviewer-supplied free text while keeping the
// score and correlation fields. A nil FeedbackFilter selects it.
func DefaultFeedbackFilter(record FeedbackRecord) FeedbackRecord {
	record.Comment = ""
	return record
}

// PassthroughFeedbackFilter exports feedback unchanged, including reviewer
// comments.
func PassthroughFeedbackFilter(record FeedbackRecord) FeedbackRecord { return record }

// ChainFilters composes filters left to right. A filter that returns the zero
// Span short-circuits the chain, so a suppressing filter is not followed by one
// that might repopulate the span.
func ChainFilters(filters ...Filter) Filter {
	active := make([]Filter, 0, len(filters))
	for _, filter := range filters {
		if filter != nil {
			active = append(active, filter)
		}
	}
	if len(active) == 0 {
		return PassthroughFilter
	}
	if len(active) == 1 {
		return active[0]
	}
	return func(span Span) Span {
		for _, filter := range active {
			span = filter(span)
			if span.SpanID == "" {
				return Span{}
			}
		}
		return span
	}
}

// RedactAttributes returns a Filter that removes the named attribute keys from
// a span and its events, on top of DefaultFilter's sensitive-prefix handling.
func RedactAttributes(keys ...string) Filter {
	if len(keys) == 0 {
		return DefaultFilter
	}
	targets := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		targets[key] = struct{}{}
	}
	return func(span Span) Span {
		span = DefaultFilter(span)
		span.Attributes = dropKeys(span.Attributes, targets)
		for i := range span.Events {
			span.Events[i].Attributes = dropKeys(span.Events[i].Attributes, targets)
		}
		return span
	}
}

func dropSensitive(attributes map[string]string) map[string]string {
	if len(attributes) == 0 {
		return attributes
	}
	for key := range attributes {
		if strings.HasPrefix(key, SensitiveAttr) {
			delete(attributes, key)
		}
	}
	if len(attributes) == 0 {
		return nil
	}
	return attributes
}

func dropKeys(attributes map[string]string, targets map[string]struct{}) map[string]string {
	if len(attributes) == 0 {
		return attributes
	}
	for key := range targets {
		delete(attributes, key)
	}
	if len(attributes) == 0 {
		return nil
	}
	return attributes
}
