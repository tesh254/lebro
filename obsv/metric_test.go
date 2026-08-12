package obsv_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
	"github.com/tesh254/lebro/obsv"
)

// TestMetricsReportUsageAndLatencyPerSpan checks that each span's own numbers
// reach the metric exporter.
//
// The two model calls report deliberately different token counts and the clock
// steps by a different amount per span, so a metric attributed to the wrong span,
// or a total that summed the wrong calls, produces a different value. Identical
// fixtures would score the same either way and prove nothing.
func TestMetricsReportUsageAndLatencyPerSpan(t *testing.T) {
	metrics := obsv.NewMemoryMetricExporter()
	observer := newObserver(t, synchronousConfig(obsv.Config{Metrics: metrics}))

	registry := newRegistry(t, echoTool{id: "lookup"})
	model := testkit.NewModel(
		testkit.Response(lebro.ModelResponse{
			Message: lebro.Message{Role: lebro.RoleAssistant, ToolCalls: mustToolCalls(t, lebro.ModelToolCall{
				ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{"query":"a"}`),
			})},
			FinishReason: lebro.FinishReasonToolCalls,
			Usage:        lebro.ModelUsage{InputTokens: 13, OutputTokens: 5, TotalTokens: 18},
		}),
		testkit.Response(lebro.ModelResponse{
			Message:      lebro.Message{Role: lebro.RoleAssistant, Content: "done"},
			FinishReason: lebro.FinishReasonStop,
			Usage:        lebro.ModelUsage{InputTokens: 29, OutputTokens: 7, TotalTokens: 36},
		}),
	)
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "agent", Model: "fixture", Tools: []lebro.ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
		Listener:   observer,
		Clock:      newSteppingClock(3 * time.Millisecond),
	})
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}
	if _, err := agent.Run(t.Context(), lebro.RunInput{
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "look it up"}},
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	exported := metrics.Metrics()
	if len(exported) == 0 {
		t.Fatal("no metrics exported")
	}

	// Token counters sum to exactly the two calls' reported usage. A
	// double-counted model span, or a run span that also emitted token
	// counters, would overshoot.
	wantTokens := map[string]int64{
		obsv.MetricInputTokens:  13 + 29,
		obsv.MetricOutputTokens: 5 + 7,
		obsv.MetricTotalTokens:  18 + 36,
	}
	for name, want := range wantTokens {
		if got := sumMetric(exported, name); got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}

	// Only model spans carry token counters, so a run span cannot double the
	// totals.
	for _, metric := range exported {
		switch metric.Name {
		case obsv.MetricInputTokens, obsv.MetricOutputTokens, obsv.MetricTotalTokens:
			if got := metric.Labels[obsv.LabelKind]; got != string(obsv.SpanKindModel) {
				t.Errorf("%s attributed to a %s span, want %s only", metric.Name, got, obsv.SpanKindModel)
			}
		}
	}

	// Every latency metric is present and positive: a zero duration would mean
	// the paired start timestamp was lost.
	for _, name := range []string{
		obsv.MetricRunDuration,
		obsv.MetricModelDuration,
		obsv.MetricToolDuration,
	} {
		found := metricsNamed(exported, name)
		if len(found) == 0 {
			t.Errorf("no %s metric exported", name)
			continue
		}
		for _, metric := range found {
			if metric.Kind != obsv.MetricKindDuration {
				t.Errorf("%s kind = %s, want %s", name, metric.Kind, obsv.MetricKindDuration)
			}
			if metric.Duration <= 0 {
				t.Errorf("%s duration = %s, want a positive elapsed time", name, metric.Duration)
			}
		}
	}

	// Outcome counters tally each kind exactly once per span.
	if got := sumMetric(exported, obsv.MetricRunOutcome); got != 1 {
		t.Errorf("%s = %d, want 1", obsv.MetricRunOutcome, got)
	}
	if got := sumMetric(exported, obsv.MetricModelOutcome); got != 2 {
		t.Errorf("%s = %d, want 2 (one per model call)", obsv.MetricModelOutcome, got)
	}
	if got := sumMetric(exported, obsv.MetricToolOutcome); got != 1 {
		t.Errorf("%s = %d, want 1", obsv.MetricToolOutcome, got)
	}

	// Labels carry the outcome and the tool identity, which is what a
	// per-tool error-rate panel needs.
	toolOutcome := metricsNamed(exported, obsv.MetricToolOutcome)
	if len(toolOutcome) > 0 {
		if got := toolOutcome[0].Labels[obsv.LabelToolID]; got != "lookup" {
			t.Errorf("tool outcome tool_id label = %q, want %q", got, "lookup")
		}
		if got := toolOutcome[0].Labels[obsv.LabelStatus]; got != string(obsv.SpanStatusOK) {
			t.Errorf("tool outcome status label = %q, want %q", got, obsv.SpanStatusOK)
		}
	}
	// Every metric is correlated to its run, so cost can be attributed.
	for _, metric := range exported {
		if metric.RunID == "" {
			t.Errorf("metric %s carries no RunID; it cannot be attributed to a run", metric.Name)
		}
		if metric.TraceID == "" {
			t.Errorf("metric %s carries no TraceID", metric.Name)
		}
	}
}

// TestMetricsOmitZeroTokenCounters checks that a provider reporting no usage does
// not produce a stream of zero-valued counters.
func TestMetricsOmitZeroTokenCounters(t *testing.T) {
	metrics := obsv.NewMemoryMetricExporter()
	observer := newObserver(t, synchronousConfig(obsv.Config{Metrics: metrics}))

	model := testkit.NewModel(testkit.Text("no usage reported"))
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "agent", Model: "fixture"},
		Model:      model,
		Listener:   observer,
		Clock:      newSteppingClock(time.Millisecond),
	})
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}
	if _, err := agent.Run(t.Context(), lebro.RunInput{
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	for _, metric := range metrics.Metrics() {
		switch metric.Name {
		case obsv.MetricInputTokens, obsv.MetricOutputTokens, obsv.MetricTotalTokens:
			t.Errorf("exported %s = %d for a provider that reported no usage", metric.Name, metric.Value)
		}
	}
	// Latency and outcome metrics still export, so omitting token counters did
	// not disable metrics wholesale.
	if got := sumMetric(metrics.Metrics(), obsv.MetricRunOutcome); got != 1 {
		t.Errorf("%s = %d, want 1", obsv.MetricRunOutcome, got)
	}
}

// TestMetricsRecordFailureStatus checks that a failed call is labelled as such;
// an error rate computed from these labels would otherwise read zero.
func TestMetricsRecordFailureStatus(t *testing.T) {
	metrics := obsv.NewMemoryMetricExporter()
	observer := newObserver(t, synchronousConfig(obsv.Config{Metrics: metrics}))

	model := testkit.NewModel(testkit.Failure(errors.New("provider unavailable")))
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "agent", Model: "fixture"},
		Model:      model,
		Listener:   observer,
		Clock:      newSteppingClock(time.Millisecond),
	})
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}
	if _, err := agent.Run(t.Context(), lebro.RunInput{
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
	}); err == nil {
		t.Fatal("Run() error = nil, want provider failure")
	}

	outcomes := metricsNamed(metrics.Metrics(), obsv.MetricRunOutcome)
	if len(outcomes) != 1 {
		t.Fatalf("exported %d run outcome metrics, want 1", len(outcomes))
	}
	if got := outcomes[0].Labels[obsv.LabelStatus]; got != string(obsv.SpanStatusError) {
		t.Errorf("run outcome status label = %q, want %q", got, obsv.SpanStatusError)
	}
}

func TestMetricsIncludeRetryAndFallbackSpans(t *testing.T) {
	metrics := obsv.NewMemoryMetricExporter()
	observer := newObserver(t, synchronousConfig(obsv.Config{Metrics: metrics}))
	start := fixtureStart
	for _, event := range []lebro.RunEvent{
		{Type: lebro.RunEventStarted, RunID: "run-1", Timestamp: start},
		{Type: lebro.RunEventStepStarted, RunID: "run-1", StepID: "step-1", Step: 1, Timestamp: start.Add(time.Millisecond)},
		{Type: lebro.RunEventStepAttemptStarted, RunID: "run-1", StepID: "step-1", Step: 1, Attempt: 2, Timestamp: start.Add(2 * time.Millisecond)},
		{Type: lebro.RunEventStepAttemptFinished, RunID: "run-1", StepID: "step-1", Step: 1, Attempt: 2, Timestamp: start.Add(3 * time.Millisecond), Duration: time.Millisecond},
		{Type: lebro.RunEventModelStarted, RunID: "run-1", StepID: "step-1", Step: 1, Timestamp: start.Add(4 * time.Millisecond)},
		{Type: lebro.RunEventModelAttemptStarted, RunID: "run-1", StepID: "step-1", Step: 1, Timestamp: start.Add(5 * time.Millisecond)},
		{Type: lebro.RunEventModelAttemptFinished, RunID: "run-1", StepID: "step-1", Step: 1, Timestamp: start.Add(6 * time.Millisecond), Duration: time.Millisecond},
		{Type: lebro.RunEventModelFinished, RunID: "run-1", StepID: "step-1", Step: 1, Timestamp: start.Add(7 * time.Millisecond), Duration: 3 * time.Millisecond},
		{Type: lebro.RunEventStepFinished, RunID: "run-1", StepID: "step-1", Step: 1, Timestamp: start.Add(8 * time.Millisecond), Duration: 7 * time.Millisecond},
		{Type: lebro.RunEventSucceeded, RunID: "run-1", Timestamp: start.Add(9 * time.Millisecond)},
	} {
		observer.OnRunEvent(event)
	}
	seen := make(map[string]bool)
	for _, metric := range metrics.Metrics() {
		seen[metric.Name] = true
	}
	for _, name := range []string{obsv.MetricStepAttemptDuration, obsv.MetricStepAttemptOutcome, obsv.MetricModelAttemptDuration, obsv.MetricModelAttemptOutcome} {
		if !seen[name] {
			t.Errorf("missing %s for ended retry or fallback span", name)
		}
	}
}

func sumMetric(metrics []obsv.Metric, name string) int64 {
	var total int64
	for _, metric := range metrics {
		if metric.Name == name {
			total += metric.Value
		}
	}
	return total
}

func metricsNamed(metrics []obsv.Metric, name string) []obsv.Metric {
	var matched []obsv.Metric
	for _, metric := range metrics {
		if metric.Name == name {
			matched = append(matched, metric)
		}
	}
	return matched
}

func mustToolCalls(t *testing.T, calls ...lebro.ModelToolCall) lebro.ModelToolCalls {
	t.Helper()
	encoded, err := lebro.NewModelToolCalls(calls...)
	if err != nil {
		t.Fatalf("NewModelToolCalls() error = %v", err)
	}
	return encoded
}
