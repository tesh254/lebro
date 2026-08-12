package obsv_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
	"github.com/tesh254/lebro/obsv"
)

// secretPayload appears in the model's streamed text and in the tool arguments
// the echo tool returns, so it is present on both sides of every call the run
// makes. A leak into any exported signal is therefore detectable by substring.
const secretPayload = "SSN-078-05-1120"

// TestFiltersRunBeforeExport is the ticket's third acceptance criterion.
//
// Every destination is checked, not just the span exporter: logs and metrics are
// derived from the same filtered spans, so a filter that ran only ahead of span
// export would leak the payload through a log line. Serializing each destination
// and searching for the payload catches a leak through any field, including ones
// added later.
func TestFiltersRunBeforeExport(t *testing.T) {
	spans := obsv.NewMemorySpanExporter()
	logs := obsv.NewMemoryLogExporter()
	metrics := obsv.NewMemoryMetricExporter()
	repository := obsv.NewMemoryRepository()

	observer := newObserver(t, synchronousConfig(obsv.Config{
		Spans:      spans,
		Logs:       logs,
		Metrics:    metrics,
		Repository: repository,
	}))

	runSecretScenario(t, observer)

	// The payload really did flow through the run: a scenario that never
	// carried it would make every assertion below vacuous.
	assertPayloadPresent(t, observer)

	for _, destination := range []struct {
		name string
		data any
	}{
		{"exported spans", spans.Spans()},
		{"exported logs", logs.Records()},
		{"exported metrics", metrics.Metrics()},
		{"stored spans", repository.Spans()},
		{"stored logs", repository.Logs()},
	} {
		assertNoPayload(t, destination.name, destination.data)
	}
}

// TestPassthroughFilterExportsPayload is the counterpart: with filtering opted
// out, the payload does reach the exporter. Without this, TestFiltersRunBeforeExport
// would pass even if the tracer never recorded the payload in the first place.
func TestPassthroughFilterExportsPayload(t *testing.T) {
	spans := obsv.NewMemorySpanExporter()
	observer := newObserver(t, synchronousConfig(obsv.Config{
		Spans:  spans,
		Filter: obsv.PassthroughFilter,
	}))

	runSecretScenario(t, observer)

	if !containsPayload(t, spans.Spans()) {
		t.Fatal("PassthroughFilter dropped the payload; the default filter's test cannot distinguish filtering from never recording it")
	}
}

// TestFilterSuppressesSpanEntirely checks that a filter returning the zero Span
// removes it from every signal rather than exporting an empty husk.
func TestFilterSuppressesSpanEntirely(t *testing.T) {
	spans := obsv.NewMemorySpanExporter()
	logs := obsv.NewMemoryLogExporter()
	metrics := obsv.NewMemoryMetricExporter()

	observer := newObserver(t, synchronousConfig(obsv.Config{
		Spans:   spans,
		Logs:    logs,
		Metrics: metrics,
		Filter: func(span obsv.Span) obsv.Span {
			if span.Kind == obsv.SpanKindTool {
				return obsv.Span{}
			}
			return obsv.DefaultFilter(span)
		},
	}))

	runSecretScenario(t, observer)

	if got := spansByKind(spans.Spans(), obsv.SpanKindTool); len(got) != 0 {
		t.Errorf("exported %d suppressed tool spans", len(got))
	}
	for _, record := range logs.Records() {
		if record.Kind == obsv.SpanKindTool {
			t.Errorf("suppressed tool span produced log record %+v", record)
		}
	}
	for _, metric := range metrics.Metrics() {
		if metric.Labels[obsv.LabelKind] == string(obsv.SpanKindTool) {
			t.Errorf("suppressed tool span produced metric %+v", metric)
		}
	}
	if observer.Stats().SpansFiltered == 0 {
		t.Error("suppressed spans were not counted; a silent suppression is indistinguishable from a dropped export")
	}
	// Other kinds still export, so the filter suppressed selectively rather
	// than disabling observability wholesale.
	if len(spansByKind(spans.Spans(), obsv.SpanKindModel)) == 0 {
		t.Error("model spans were suppressed too; the filter is not selective")
	}
}

// TestDefaultFilterKeepsStructureAndDropsPayload pins what the default policy
// keeps. A filter that dropped identifiers or timings alongside payloads would
// satisfy the leak test while making traces useless.
func TestDefaultFilterKeepsStructureAndDropsPayload(t *testing.T) {
	span := obsv.Span{
		TraceID:  "trace-1",
		SpanID:   "span-1",
		Kind:     obsv.SpanKindModel,
		Name:     "model",
		RunID:    "run-1",
		StepID:   "step-1",
		Start:    fixtureStart,
		End:      fixtureStart.Add(time.Second),
		Duration: time.Second,
		Status:   obsv.SpanStatusOK,
		Usage:    lebro.ModelUsage{InputTokens: 7, OutputTokens: 2, TotalTokens: 9},
		Attributes: map[string]string{
			obsv.AttrToolID:              "lookup",
			obsv.AttrFinishReason:        "stop",
			obsv.AttrSensitiveDeltaText:  secretPayload,
			obsv.AttrSensitiveStructured: `{"ssn":"` + secretPayload + `"}`,
		},
		Events: []obsv.SpanEvent{{
			Name:      "model_delta",
			Timestamp: fixtureStart,
			Attributes: map[string]string{
				obsv.AttrSensitiveDeltaText: secretPayload,
				obsv.AttrToolCallID:         "call-1",
			},
		}},
	}

	filtered := obsv.DefaultFilter(span.Clone())

	for _, key := range []string{obsv.AttrSensitiveDeltaText, obsv.AttrSensitiveStructured} {
		if value, ok := filtered.Attributes[key]; ok {
			t.Errorf("DefaultFilter kept sensitive attribute %q = %q", key, value)
		}
	}
	if len(filtered.Events) != 1 {
		t.Fatalf("DefaultFilter kept %d events, want 1; the shape of a stream should stay visible", len(filtered.Events))
	}
	if value, ok := filtered.Events[0].Attributes[obsv.AttrSensitiveDeltaText]; ok {
		t.Errorf("DefaultFilter kept sensitive event attribute = %q", value)
	}
	if got := filtered.Events[0].Attributes[obsv.AttrToolCallID]; got != "call-1" {
		t.Errorf("DefaultFilter dropped the non-sensitive event attribute; tool call ID = %q, want %q", got, "call-1")
	}

	// Structure survives.
	if filtered.TraceID != span.TraceID || filtered.SpanID != span.SpanID {
		t.Errorf("DefaultFilter changed identifiers: %+v", filtered)
	}
	if filtered.Duration != span.Duration || !filtered.Start.Equal(span.Start) {
		t.Errorf("DefaultFilter changed timings: start=%s duration=%s", filtered.Start, filtered.Duration)
	}
	if filtered.Usage != span.Usage {
		t.Errorf("DefaultFilter changed usage = %+v, want %+v", filtered.Usage, span.Usage)
	}
	if filtered.Status != span.Status {
		t.Errorf("DefaultFilter changed status = %s", filtered.Status)
	}
	if got := filtered.Attributes[obsv.AttrToolID]; got != "lookup" {
		t.Errorf("DefaultFilter dropped tool ID = %q, want %q", got, "lookup")
	}
}

// TestChainFiltersShortCircuitsOnSuppression checks that a suppressing filter
// stops the chain. A later filter that repopulated the span would resurrect data
// an earlier one deliberately removed.
func TestChainFiltersShortCircuitsOnSuppression(t *testing.T) {
	var reached bool
	chain := obsv.ChainFilters(
		func(obsv.Span) obsv.Span { return obsv.Span{} },
		func(span obsv.Span) obsv.Span {
			reached = true
			span.SpanID = "resurrected"
			return span
		},
	)
	if got := chain(obsv.Span{SpanID: "span-1"}); got.SpanID != "" {
		t.Errorf("chain returned span %q after suppression", got.SpanID)
	}
	if reached {
		t.Error("chain ran a filter after suppression; a suppressed span must not be repopulated")
	}

	// A nil filter in the chain is skipped rather than panicking, and an
	// all-nil chain is a passthrough rather than a suppressor: silently
	// dropping every span would be the worst possible default.
	passthrough := obsv.ChainFilters(nil, nil)
	if got := passthrough(obsv.Span{SpanID: "span-1"}); got.SpanID != "span-1" {
		t.Errorf("all-nil chain returned %q, want the span unchanged", got.SpanID)
	}
}

// TestRedactAttributesRemovesNamedKeys checks the composable redactor.
func TestRedactAttributesRemovesNamedKeys(t *testing.T) {
	filter := obsv.ChainFilters(obsv.DefaultFilter, obsv.RedactAttributes(obsv.AttrToolID))
	span := obsv.Span{
		SpanID:     "span-1",
		Kind:       obsv.SpanKindTool,
		Attributes: map[string]string{obsv.AttrToolID: "lookup", obsv.AttrToolState: "succeeded"},
	}
	filtered := filter(span.Clone())
	if _, ok := filtered.Attributes[obsv.AttrToolID]; ok {
		t.Error("RedactAttributes kept the named key")
	}
	if got := filtered.Attributes[obsv.AttrToolState]; got != "succeeded" {
		t.Errorf("RedactAttributes dropped an unnamed key; tool state = %q", got)
	}
	if got := obsv.RedactAttributes()(span.Clone()); got.Attributes[obsv.AttrToolID] != "lookup" {
		t.Error("RedactAttributes() with no keys should be a passthrough")
	}
}

// TestDropEventsFilterKeepsSpan checks that dropping events leaves the span
// itself intact.
func TestDropEventsFilterKeepsSpan(t *testing.T) {
	span := obsv.Span{
		SpanID: "span-1",
		Kind:   obsv.SpanKindModel,
		Events: []obsv.SpanEvent{{Name: "model_delta"}, {Name: "tool_requested"}},
	}
	filtered := obsv.DropEventsFilter(span.Clone())
	if len(filtered.Events) != 0 {
		t.Errorf("DropEventsFilter kept %d events", len(filtered.Events))
	}
	if filtered.SpanID != "span-1" || filtered.Kind != obsv.SpanKindModel {
		t.Errorf("DropEventsFilter altered the span: %+v", filtered)
	}
}

// runSecretScenario drives a streaming run whose model text and tool payload both
// carry secretPayload.
func runSecretScenario(t *testing.T, observer *obsv.Observer) {
	t.Helper()
	registry := newRegistry(t, echoTool{id: "lookup"})
	model := testkit.NewModel(
		testkit.ToolCallResponse(testkit.ToolCall{
			ID:        "call-1",
			ToolID:    "lookup",
			Arguments: json.RawMessage(`{"ssn":"` + secretPayload + `"}`),
		}),
		testkit.Stream(testkit.TextChunk("your record is "), testkit.TextChunk(secretPayload)),
	)
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "agent", Model: "fixture", Tools: []lebro.ToolID{"lookup"}},
		Model:      model,
		Tools:      registry,
		Listener:   observer,
		Clock:      newSteppingClock(time.Millisecond),
	})
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}
	stream, err := agent.RunStream(context.Background(), lebro.RunInput{
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "look up my record"}},
	})
	if err != nil {
		t.Fatalf("RunStream() error = %v", err)
	}
	defer stream.Cancel()
	if _, err := stream.Drain(); err != nil {
		t.Fatalf("Drain() error = %v", err)
	}
}

// assertPayloadPresent confirms the scenario produced observability data at all,
// so a leak test cannot pass because nothing was recorded.
func assertPayloadPresent(t *testing.T, observer *obsv.Observer) {
	t.Helper()
	if stats := observer.Stats(); stats.SpansExported == 0 {
		t.Fatalf("scenario exported no spans; stats = %+v", stats)
	}
}

func assertNoPayload(t *testing.T, name string, data any) {
	t.Helper()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	if strings.Contains(string(encoded), secretPayload) {
		t.Errorf("%s leaked the payload %q; filters must run before export", name, secretPayload)
	}
}

func containsPayload(t *testing.T, data any) bool {
	t.Helper()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return strings.Contains(string(encoded), secretPayload)
}
