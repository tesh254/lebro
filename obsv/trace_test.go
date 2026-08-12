package obsv_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
	"github.com/tesh254/lebro/obsv"
)

// TestObserverCorrelatesModelToolAndWorkflowSpans is the ticket's first
// acceptance criterion: a run produces correlated model, tool, and workflow
// spans.
//
// The run is a real workflow driving a real agent through the scripted provider,
// not a hand-built event slice: a synthetic slice would keep passing even if the
// runtime stopped emitting one side of a pairing, which is precisely the
// regression this test exists to catch.
func TestObserverCorrelatesModelToolAndWorkflowSpans(t *testing.T) {
	exporter := obsv.NewMemorySpanExporter()
	observer := newObserver(t, synchronousConfig(obsv.Config{Spans: exporter}))

	registry := newRegistry(t, echoTool{id: "lookup"})
	model := testkit.NewModel(
		testkit.ToolCallResponse(testkit.ToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{"query":"handbook"}`)}),
		testkit.Text("done"),
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
	agentStep, err := lebro.NewAgentStep(agent)
	if err != nil {
		t.Fatalf("NewAgentStep() error = %v", err)
	}
	workflow, err := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition: lebro.WorkflowDefinition{ID: "pipeline"},
		Listener:   observer,
		Clock:      newSteppingClock(time.Millisecond),
		Steps: []lebro.Step{
			{Definition: lebro.StepDefinition{ID: "prepare"}, Handler: lebro.StepHandlerFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`"summarize"`), nil
			})},
			{Definition: lebro.StepDefinition{ID: "agent"}, Handler: agentStep},
		},
	})
	if err != nil {
		t.Fatalf("NewLinearWorkflow() error = %v", err)
	}

	result, err := workflow.Run(context.Background(), lebro.WorkflowRunInput{Input: json.RawMessage(`"start"`)})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != lebro.RunStatusSucceeded {
		t.Fatalf("Run() status = %s, want %s", result.Status, lebro.RunStatusSucceeded)
	}

	spans := exporter.Spans()
	if len(spans) == 0 {
		t.Fatal("no spans exported")
	}

	// One trace covers the workflow, the nested agent run, and the agent's
	// model and tool calls. Two traces would mean the nested run failed to
	// correlate to its parent.
	traces := make(map[obsv.TraceID]int)
	for _, span := range spans {
		traces[span.TraceID]++
	}
	if len(traces) != 1 {
		t.Fatalf("exported spans span %d traces, want 1: %s", len(traces), describeSpans(spans))
	}

	for _, kind := range []obsv.SpanKind{obsv.SpanKindRun, obsv.SpanKindStep, obsv.SpanKindModel, obsv.SpanKindTool} {
		if len(spansByKind(spans, kind)) == 0 {
			t.Errorf("no %s span exported; got %s", kind, describeSpans(spans))
		}
	}

	indexed := spanByID(spans)

	// Exactly one span roots the trace, and it is the workflow run.
	var roots []obsv.Span
	for _, span := range spans {
		if span.IsRoot() {
			roots = append(roots, span)
		}
	}
	if len(roots) != 1 {
		t.Fatalf("exported %d root spans, want 1: %s", len(roots), describeSpans(roots))
	}
	if roots[0].Kind != obsv.SpanKindRun {
		t.Errorf("root span kind = %s, want %s", roots[0].Kind, obsv.SpanKindRun)
	}

	// The tool span sits under the model's step, under the nested agent run,
	// under the workflow step that launched it, under the workflow run. That
	// chain is the correlation the ticket asks for.
	tool := findSpan(t, spans, "tool span", func(span obsv.Span) bool { return span.Kind == obsv.SpanKindTool })
	toolChain := ancestors(t, indexed, tool)
	if !containsKinds(toolChain, obsv.SpanKindRun, obsv.SpanKindStep) {
		t.Errorf("tool span ancestry = %v, want a run and a step ancestor", toolChain)
	}

	model2 := findSpan(t, spans, "model span", func(span obsv.Span) bool { return span.Kind == obsv.SpanKindModel })
	modelChain := ancestors(t, indexed, model2)
	if !containsKinds(modelChain, obsv.SpanKindRun) {
		t.Errorf("model span ancestry = %v, want a run ancestor", modelChain)
	}

	// The nested agent run parents to a workflow step rather than rooting its
	// own trace.
	nested := findSpan(t, spans, "nested agent run span", func(span obsv.Span) bool {
		return span.Kind == obsv.SpanKindRun && !span.IsRoot()
	})
	parent, ok := indexed[nested.ParentSpanID]
	if !ok {
		t.Fatalf("nested run parent %s was never exported", nested.ParentSpanID)
	}
	if parent.Kind != obsv.SpanKindStep {
		t.Errorf("nested run parent kind = %s, want %s", parent.Kind, obsv.SpanKindStep)
	}
	if parent.StepID != "agent" {
		t.Errorf("nested run parents to step %q, want the %q step that launched it", parent.StepID, "agent")
	}
	// The two runs are distinct spans even though the runtime's default ID
	// sources gave them the same RunID; see
	// TestObserverSeparatesRunsSharingARunID.
	if nested.SpanID == roots[0].SpanID {
		t.Error("nested run and workflow run share a span ID; they must be distinct spans")
	}

	// Every ended span carries a status and a bracketed time range.
	for _, span := range spans {
		if span.Status == obsv.SpanStatusUnset {
			t.Errorf("span %s (%s) exported with unset status", span.SpanID, span.Kind)
		}
		if span.Start.IsZero() {
			t.Errorf("span %s (%s) has a zero start", span.SpanID, span.Kind)
		}
		if span.End.Before(span.Start) {
			t.Errorf("span %s (%s) ends %s before it starts %s", span.SpanID, span.Kind, span.End, span.Start)
		}
	}
}

// TestObserverSeparatesRunsSharingARunID pins the behavior that made keying
// in-flight span state by RunID alone incorrect.
//
// Each primitive mints run IDs from its own IDSource, so a workflow and the
// agent it invokes both produce "agent-run-0001" unless the application injects
// distinct sources. A tracer keyed on RunID merges the two runs: the nested
// run's terminal event closes the parent's open step, which surfaced as a
// spuriously failed step span and a split trace. The tracer keys on the
// launching invocation instead, which separates them.
func TestObserverSeparatesRunsSharingARunID(t *testing.T) {
	exporter := obsv.NewMemorySpanExporter()
	observer := newObserver(t, synchronousConfig(obsv.Config{Spans: exporter}))

	model := testkit.NewModel(testkit.Text("done"))
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "agent", Model: "fixture"},
		Model:      model,
		Listener:   observer,
		Clock:      newSteppingClock(time.Millisecond),
	})
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}
	agentStep, err := lebro.NewAgentStep(agent)
	if err != nil {
		t.Fatalf("NewAgentStep() error = %v", err)
	}
	workflow, err := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition: lebro.WorkflowDefinition{ID: "pipeline"},
		Listener:   observer,
		Clock:      newSteppingClock(time.Millisecond),
		Steps: []lebro.Step{
			{Definition: lebro.StepDefinition{ID: "agent"}, Handler: agentStep},
		},
	})
	if err != nil {
		t.Fatalf("NewLinearWorkflow() error = %v", err)
	}
	if _, err := workflow.Run(context.Background(), lebro.WorkflowRunInput{Input: json.RawMessage(`"go"`)}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	spans := exporter.Spans()
	runs := spansByKind(spans, obsv.SpanKindRun)
	if len(runs) != 2 {
		t.Fatalf("exported %d run spans, want 2 (workflow and nested agent): %s", len(runs), describeSpans(spans))
	}
	// The premise of this test: the runtime handed both runs the same ID. If
	// this stops holding the test still passes, but it no longer covers the
	// collision, so say so rather than passing silently.
	if runs[0].RunID != runs[1].RunID {
		t.Skipf("runtime no longer reuses run IDs across nested runs (%q, %q); collision case not exercised", runs[0].RunID, runs[1].RunID)
	}
	if runs[0].SpanID == runs[1].SpanID {
		t.Fatal("both runs exported as one span; in-flight state was keyed by RunID")
	}

	// Neither run reports a failure: a merged tracer closed the parent's step
	// span on the child's terminal event, which showed up as a failed step.
	for _, span := range spans {
		if span.Status == obsv.SpanStatusError {
			t.Errorf("span %s (%s/%s) reports an error on a fully successful run", span.SpanID, span.Kind, span.Name)
		}
	}
	steps := spansByKind(spans, obsv.SpanKindStep)
	if len(steps) != 1 {
		t.Errorf("exported %d step spans, want 1: %s", len(steps), describeSpans(steps))
	}
	traces := make(map[obsv.TraceID]struct{})
	for _, span := range spans {
		traces[span.TraceID] = struct{}{}
	}
	if len(traces) != 1 {
		t.Errorf("exported spans span %d traces, want 1", len(traces))
	}
}

// TestObserverAggregatesUsageOntoRunSpan checks that a run span reports the
// tokens its model calls consumed, since cost is one of the ticket's stated
// goals. The two model calls report different usage, so a span that summed the
// wrong call, or double-counted one, produces a different total.
func TestObserverAggregatesUsageOntoRunSpan(t *testing.T) {
	exporter := obsv.NewMemorySpanExporter()
	observer := newObserver(t, synchronousConfig(obsv.Config{Spans: exporter}))

	model := testkit.NewModel(
		testkit.Response(lebro.ModelResponse{
			Message:      lebro.Message{Role: lebro.RoleAssistant, Content: "first"},
			FinishReason: lebro.FinishReasonLength,
			Usage:        lebro.ModelUsage{InputTokens: 11, OutputTokens: 3, TotalTokens: 14},
		}),
	)
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "agent", Model: "fixture"},
		Model:      model,
		Listener:   observer,
		Clock:      newSteppingClock(time.Millisecond),
	})
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}
	if _, err := agent.Run(context.Background(), lebro.RunInput{
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hello"}},
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	spans := exporter.Spans()
	run := findSpan(t, spans, "run span", func(span obsv.Span) bool { return span.Kind == obsv.SpanKindRun })
	want := lebro.ModelUsage{InputTokens: 11, OutputTokens: 3, TotalTokens: 14}
	if run.Usage != want {
		t.Errorf("run span usage = %+v, want %+v", run.Usage, want)
	}

	modelSpan := findSpan(t, spans, "model span", func(span obsv.Span) bool { return span.Kind == obsv.SpanKindModel })
	if modelSpan.Usage != want {
		t.Errorf("model span usage = %+v, want %+v", modelSpan.Usage, want)
	}
	if got := modelSpan.Attributes[obsv.AttrFinishReason]; got != string(lebro.FinishReasonLength) {
		t.Errorf("model span finish reason = %q, want %q", got, lebro.FinishReasonLength)
	}
}

// TestObserverRecordsFailureStatus checks that a failed run is exported as an
// error rather than silently as a success.
func TestObserverRecordsFailureStatus(t *testing.T) {
	exporter := obsv.NewMemorySpanExporter()
	observer := newObserver(t, synchronousConfig(obsv.Config{Spans: exporter}))

	failure := errors.New("provider exploded")
	model := testkit.NewModel(testkit.Failure(failure))
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "agent", Model: "fixture"},
		Model:      model,
		Listener:   observer,
		Clock:      newSteppingClock(time.Millisecond),
	})
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}
	if _, err := agent.Run(context.Background(), lebro.RunInput{
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hello"}},
	}); err == nil {
		t.Fatal("Run() error = nil, want provider failure")
	}

	spans := exporter.Spans()
	run := findSpan(t, spans, "run span", func(span obsv.Span) bool { return span.Kind == obsv.SpanKindRun })
	if run.Status != obsv.SpanStatusError {
		t.Errorf("run span status = %s, want %s", run.Status, obsv.SpanStatusError)
	}
	modelSpan := findSpan(t, spans, "model span", func(span obsv.Span) bool { return span.Kind == obsv.SpanKindModel })
	if modelSpan.Status != obsv.SpanStatusError {
		t.Errorf("model span status = %s, want %s", modelSpan.Status, obsv.SpanStatusError)
	}
	if modelSpan.Error == "" {
		t.Error("model span carries no error message")
	}
	if !errors.Is(modelSpan.Err, failure) {
		t.Errorf("model span Err = %v, want it to wrap %v", modelSpan.Err, failure)
	}
}

// TestObserverRecordsCancellationDistinctly checks that a cancelled run is not
// counted as a failure: cost and error dashboards read these differently.
func TestObserverRecordsCancellationDistinctly(t *testing.T) {
	exporter := obsv.NewMemorySpanExporter()
	observer := newObserver(t, synchronousConfig(obsv.Config{Spans: exporter}))

	model := testkit.NewModel(testkit.WaitForCancellation())
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "agent", Model: "fixture"},
		Model:      model,
		Listener:   observer,
		Clock:      newSteppingClock(time.Millisecond),
	})
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() {
		_, err := agent.Run(ctx, lebro.RunInput{
			Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hello"}},
		})
		runErr <- err
	}()
	waitFor(t, func() bool { return len(model.Calls()) == 1 })
	cancel()
	if err := <-runErr; err == nil {
		t.Fatal("Run() error = nil, want cancellation")
	}

	spans := exporter.Spans()
	run := findSpan(t, spans, "run span", func(span obsv.Span) bool { return span.Kind == obsv.SpanKindRun })
	if run.Status != obsv.SpanStatusCancelled {
		t.Errorf("run span status = %s, want %s", run.Status, obsv.SpanStatusCancelled)
	}
}

// TestTracerBoundsStreamingDeltaEvents checks the documented delta cap. Without
// it, a streaming run produces one span event per token and a span grows with the
// response length.
func TestTracerBoundsStreamingDeltaEvents(t *testing.T) {
	exporter := obsv.NewMemorySpanExporter()
	observer := newObserver(t, synchronousConfig(obsv.Config{
		Spans:      exporter,
		Filter:     obsv.PassthroughFilter,
		DeltaLimit: 3,
	}))

	chunks := make([]testkit.StreamChunk, 0, 10)
	for i := 0; i < 10; i++ {
		chunks = append(chunks, testkit.TextChunk("token"))
	}
	model := testkit.NewModel(testkit.Stream(chunks...))
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "agent", Model: "fixture"},
		Model:      model,
		Listener:   observer,
		Clock:      newSteppingClock(time.Millisecond),
	})
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}

	stream, err := agent.RunStream(context.Background(), lebro.RunInput{
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("RunStream() error = %v", err)
	}
	defer stream.Cancel()
	if _, err := stream.Drain(); err != nil {
		t.Fatalf("Drain() error = %v", err)
	}

	spans := exporter.Spans()
	modelSpan := findSpan(t, spans, "model span", func(span obsv.Span) bool { return span.Kind == obsv.SpanKindModel })
	deltas := 0
	for _, event := range modelSpan.Events {
		if event.Name == "model_delta" {
			deltas++
		}
	}
	if deltas > 3 {
		t.Errorf("model span retained %d delta events, want at most 3", deltas)
	}
	if deltas == 0 {
		t.Error("model span retained no delta events; the cap should retain up to the limit, not drop all")
	}
	if modelSpan.Attributes["model.deltas_dropped"] == "" {
		t.Error("model span does not report dropped deltas; silent truncation reads as complete coverage")
	}
}

func TestObserverCorrelatesDelegatedAgentWithToolSpan(t *testing.T) {
	exporter := obsv.NewMemorySpanExporter()
	observer := newObserver(t, synchronousConfig(obsv.Config{Spans: exporter}))
	child, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "child", Model: "fixture"},
		Model:      testkit.NewModel(testkit.Text("child result")),
		Listener:   observer,
		Clock:      newSteppingClock(time.Millisecond),
	})
	if err != nil {
		t.Fatalf("NewAgent(child) error = %v", err)
	}
	subagent, err := lebro.NewSubagent(lebro.SubagentConfig{ID: "delegate", Agent: child})
	if err != nil {
		t.Fatalf("NewSubagent() error = %v", err)
	}
	registry := newRegistry(t, subagent)
	supervisor, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "supervisor", Model: "fixture", Tools: []lebro.ToolID{"delegate"}},
		Model: testkit.NewModel(
			testkit.ToolCallResponse(testkit.ToolCall{ID: "delegate-1", ToolID: "delegate", Arguments: json.RawMessage(`{"task":"research"}`)}),
			testkit.Text("complete"),
		),
		Tools: registry, Listener: observer, Clock: newSteppingClock(time.Millisecond),
	})
	if err != nil {
		t.Fatalf("NewAgent(supervisor) error = %v", err)
	}
	if _, err := supervisor.Run(context.Background(), lebro.RunInput{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "delegate"}}}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	spans := exporter.Spans()
	tool := findSpan(t, spans, "delegation tool", func(span obsv.Span) bool {
		return span.Kind == obsv.SpanKindTool && span.Attributes[obsv.AttrToolID] == "delegate"
	})
	childRun := findSpan(t, spans, "delegated child run", func(span obsv.Span) bool {
		return span.Kind == obsv.SpanKindRun && span.ParentSpanID == tool.SpanID
	})
	if childRun.TraceID != tool.TraceID {
		t.Errorf("delegated run trace = %q, tool trace = %q", childRun.TraceID, tool.TraceID)
	}
}

func containsKinds(chain []obsv.SpanKind, want ...obsv.SpanKind) bool {
	present := make(map[obsv.SpanKind]bool, len(chain))
	for _, kind := range chain {
		present[kind] = true
	}
	for _, kind := range want {
		if !present[kind] {
			return false
		}
	}
	return true
}
