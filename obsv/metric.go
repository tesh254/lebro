package obsv

import (
	"time"

	"github.com/tesh254/lebro"
)

// MetricKind distinguishes how a metric value should be interpreted by a
// backend.
type MetricKind string

const (
	// MetricKindCounter is a value to be summed across records, such as a token
	// count or an outcome tally.
	MetricKindCounter MetricKind = "counter"
	// MetricKindDuration is an elapsed time to be aggregated as a
	// distribution, such as a latency.
	MetricKindDuration MetricKind = "duration"
)

// Metric names emitted for every ended span. Names are stable so a dashboard
// built against them keeps working.
const (
	MetricRunDuration    = "lebro.run.duration"
	MetricStepDuration   = "lebro.step.duration"
	MetricModelDuration  = "lebro.model.duration"
	MetricToolDuration   = "lebro.tool.duration"
	MetricInputTokens    = "lebro.model.input_tokens"
	MetricOutputTokens   = "lebro.model.output_tokens"
	MetricTotalTokens    = "lebro.model.total_tokens"
	MetricRunOutcome     = "lebro.run.outcome"
	MetricStepOutcome    = "lebro.step.outcome"
	MetricToolOutcome    = "lebro.tool.outcome"
	MetricModelOutcome   = "lebro.model.outcome"
	MetricSpansDropped   = "lebro.export.spans_dropped"
	MetricExportFailures = "lebro.export.failures"
)

// Metric labels. Labels come from filtered span attributes, so no payload data
// can reach a metric backend through them.
const (
	LabelStatus        = "status"
	LabelKind          = "kind"
	LabelToolID        = "tool_id"
	LabelToolState     = "tool_state"
	LabelProvider      = "provider"
	LabelProviderModel = "provider_model"
	LabelStepID        = "step_id"
	LabelFinishReason  = "finish_reason"
)

// Metric is one measurement derived from a span. Value carries counters and
// Duration carries elapsed times; exactly one is meaningful for a given Kind.
type Metric struct {
	Name      string            `json:"name"`
	Kind      MetricKind        `json:"kind"`
	Value     int64             `json:"value,omitempty"`
	Duration  time.Duration     `json:"duration,omitempty"`
	Timestamp time.Time         `json:"timestamp"`
	TraceID   TraceID           `json:"trace_id,omitempty"`
	RunID     lebro.RunID       `json:"run_id,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// Clone returns a deep copy of the metric.
func (m Metric) Clone() Metric {
	m.Labels = cloneAttributes(m.Labels)
	return m
}

// metricsForSpan derives the metrics for an ended span. It is called after
// filtering, so span carries only what the filter allowed.
//
// Token counters are emitted only for model spans: a run span aggregates its
// models' usage, so counting both would double every token.
func metricsForSpan(span Span) []Metric {
	timestamp := span.End
	if timestamp.IsZero() {
		timestamp = span.Start
	}
	labels := labelsForSpan(span)
	metrics := make([]Metric, 0, 5)

	if name, ok := durationMetricName(span.Kind); ok {
		metrics = append(metrics, Metric{
			Name:      name,
			Kind:      MetricKindDuration,
			Duration:  span.Duration,
			Timestamp: timestamp,
			TraceID:   span.TraceID,
			RunID:     span.RunID,
			Labels:    cloneAttributes(labels),
		})
	}
	if name, ok := outcomeMetricName(span.Kind); ok {
		metrics = append(metrics, Metric{
			Name:      name,
			Kind:      MetricKindCounter,
			Value:     1,
			Timestamp: timestamp,
			TraceID:   span.TraceID,
			RunID:     span.RunID,
			Labels:    cloneAttributes(labels),
		})
	}
	if span.Kind == SpanKindModel {
		for _, usage := range []struct {
			name  string
			value int64
		}{
			{MetricInputTokens, span.Usage.InputTokens},
			{MetricOutputTokens, span.Usage.OutputTokens},
			{MetricTotalTokens, span.Usage.TotalTokens},
		} {
			if usage.value == 0 {
				continue
			}
			metrics = append(metrics, Metric{
				Name:      usage.name,
				Kind:      MetricKindCounter,
				Value:     usage.value,
				Timestamp: timestamp,
				TraceID:   span.TraceID,
				RunID:     span.RunID,
				Labels:    cloneAttributes(labels),
			})
		}
	}
	if len(metrics) == 0 {
		return nil
	}
	return metrics
}

func durationMetricName(kind SpanKind) (string, bool) {
	switch kind {
	case SpanKindRun:
		return MetricRunDuration, true
	case SpanKindStep:
		return MetricStepDuration, true
	case SpanKindModel:
		return MetricModelDuration, true
	case SpanKindTool:
		return MetricToolDuration, true
	default:
		return "", false
	}
}

func outcomeMetricName(kind SpanKind) (string, bool) {
	switch kind {
	case SpanKindRun:
		return MetricRunOutcome, true
	case SpanKindStep:
		return MetricStepOutcome, true
	case SpanKindModel:
		return MetricModelOutcome, true
	case SpanKindTool:
		return MetricToolOutcome, true
	default:
		return "", false
	}
}

// labelsForSpan builds the label set for a span. Only attributes with bounded
// cardinality become labels: a metrics backend allocates a series per label
// combination, so a per-run or per-call value would create unbounded series.
func labelsForSpan(span Span) map[string]string {
	labels := map[string]string{
		LabelStatus: string(span.Status),
		LabelKind:   string(span.Kind),
	}
	if span.StepID != "" {
		labels[LabelStepID] = string(span.StepID)
	}
	for attr, label := range map[string]string{
		AttrToolID:        LabelToolID,
		AttrToolState:     LabelToolState,
		AttrProvider:      LabelProvider,
		AttrProviderModel: LabelProviderModel,
		AttrFinishReason:  LabelFinishReason,
	} {
		if value, ok := span.Attributes[attr]; ok && value != "" {
			labels[label] = value
		}
	}
	return labels
}

func cloneMetrics(metrics []Metric) []Metric {
	if len(metrics) == 0 {
		return nil
	}
	cloned := make([]Metric, len(metrics))
	for i, metric := range metrics {
		cloned[i] = metric.Clone()
	}
	return cloned
}
