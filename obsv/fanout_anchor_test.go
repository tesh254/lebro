package obsv_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
	"github.com/tesh254/lebro/obsv"
)

// TestTracerFanOutPreservesStepAnchor verifies that when concurrent fan-out
// branches share the same step key (same RunID, StepID, and step position),
// a later branch's tool anchor does not overwrite the earlier branch's anchor.
//
// Without the fix, the second branch's ToolStarted overwrites the first
// branch's tool anchor in stepTraces, so the first branch's subagent is
// attached to the wrong tool span. With the fix, the first branch's step
// anchor is preserved and both subagents parent to it.
//
// The test feeds events directly to the observer to control the exact
// interleaving that triggers the bug: branch B's tool event arrives between
// branch A's tool event and branch A's subagent start.
func TestTracerFanOutPreservesStepAnchor(t *testing.T) {
	exporter := obsv.NewMemorySpanExporter()
	observer := newObserver(t, synchronousConfig(obsv.Config{Spans: exporter}))

	const wfRun = lebro.RunID("wf-run-1")
	ts := fixtureStart

	emit := func(typ lebro.RunEventType, runID lebro.RunID, opts ...func(*lebro.RunEvent)) {
		ev := lebro.RunEvent{Type: typ, RunID: runID, Timestamp: ts}
		for _, opt := range opts {
			opt(&ev)
		}
		observer.OnRunEvent(ev)
		ts = ts.Add(time.Millisecond)
	}

	withStep := func(stepID lebro.StepID, step int) func(*lebro.RunEvent) {
		return func(e *lebro.RunEvent) { e.StepID = stepID; e.Step = step }
	}
	withBranch := func(branch string) func(*lebro.RunEvent) {
		return func(e *lebro.RunEvent) { e.Branch = branch }
	}
	withTool := func(toolID lebro.ToolID, callID string) func(*lebro.RunEvent) {
		return func(e *lebro.RunEvent) { e.ToolID = toolID; e.ToolCallID = callID }
	}
	withParent := func(parentRun lebro.RunID, parentStepID lebro.StepID, parentStep int) func(*lebro.RunEvent) {
		return func(e *lebro.RunEvent) {
			e.ParentRunID = parentRun
			e.ParentStepID = parentStepID
			e.ParentStep = parentStep
		}
	}

	// Workflow run starts.
	emit(lebro.RunEventStarted, wfRun)

	// Fan-out step starts at position 1.
	emit(lebro.RunEventStepStarted, wfRun, withStep("fanout", 1))

	// Branch A's child step starts (same Step=1, StepID="process", Branch="A").
	emit(lebro.RunEventStepStarted, wfRun, withStep("process", 1), withBranch("A"))

	// Branch A's tool starts.
	emit(lebro.RunEventToolStarted, wfRun, withStep("process", 1), withBranch("A"), withTool("delegate", "tc-A"))

	// Branch B's child step starts (same key, Branch="B").
	emit(lebro.RunEventStepStarted, wfRun, withStep("process", 1), withBranch("B"))

	// Branch B's tool starts — this would overwrite branch A's tool anchor
	// without the fix.
	emit(lebro.RunEventToolStarted, wfRun, withStep("process", 1), withBranch("B"), withTool("delegate", "tc-B"))

	// Subagent A starts. Its parent is the workflow run, step "process",
	// position 1. Without the fix, locateParent finds branch B's tool span.
	// Every event for a nested run carries the parent invocation, not just
	// the start event, because the runtime's emitter stamps them all.
	childA := lebro.RunID("child-A")
	childAParent := withParent(wfRun, "process", 1)
	emit(lebro.RunEventStarted, childA, childAParent)
	emit(lebro.RunEventSucceeded, childA, childAParent, func(e *lebro.RunEvent) { e.Status = lebro.RunStatusSucceeded })

	// Subagent B starts with the same parent key.
	childB := lebro.RunID("child-B")
	childBParent := withParent(wfRun, "process", 1)
	emit(lebro.RunEventStarted, childB, childBParent)
	emit(lebro.RunEventSucceeded, childB, childBParent, func(e *lebro.RunEvent) { e.Status = lebro.RunStatusSucceeded })

	// Close remaining spans.
	emit(lebro.RunEventToolFinished, wfRun, withStep("process", 1), withBranch("A"), withTool("delegate", "tc-A"))
	emit(lebro.RunEventStepFinished, wfRun, withStep("process", 1), withBranch("A"))
	emit(lebro.RunEventToolFinished, wfRun, withStep("process", 1), withBranch("B"), withTool("delegate", "tc-B"))
	emit(lebro.RunEventStepFinished, wfRun, withStep("process", 1), withBranch("B"))
	emit(lebro.RunEventStepFinished, wfRun, withStep("fanout", 1))
	emit(lebro.RunEventSucceeded, wfRun, func(e *lebro.RunEvent) { e.Status = lebro.RunStatusSucceeded })

	spans := exporter.Spans()
	indexed := spanByID(spans)

	// Both child runs must be in the same trace as the workflow.
	wfRunSpan := findSpan(t, spans, "workflow run span", func(s obsv.Span) bool {
		return s.Kind == obsv.SpanKindRun && s.RunID == wfRun
	})
	childASpan := findSpan(t, spans, "child A run span", func(s obsv.Span) bool {
		return s.Kind == obsv.SpanKindRun && s.RunID == childA
	})
	childBSpan := findSpan(t, spans, "child B run span", func(s obsv.Span) bool {
		return s.Kind == obsv.SpanKindRun && s.RunID == childB
	})

	if childASpan.TraceID != wfRunSpan.TraceID {
		t.Errorf("child A trace %q != workflow trace %q; fan-out anchor was lost",
			childASpan.TraceID, wfRunSpan.TraceID)
	}
	if childBSpan.TraceID != wfRunSpan.TraceID {
		t.Errorf("child B trace %q != workflow trace %q", childBSpan.TraceID, wfRunSpan.TraceID)
	}

	// Without the fix, child A parents to branch B's tool span. With the fix,
	// child A parents to the first branch's step span (an unambiguous parent
	// for all branches). Verify neither child parents to a tool span — the
	// fix preserves the step anchor rather than letting tools overwrite it.
	for _, child := range []struct {
		name string
		span obsv.Span
	}{
		{"child A", childASpan},
		{"child B", childBSpan},
	} {
		if child.span.ParentSpanID == "" {
			t.Errorf("%s has no parent span", child.name)
			continue
		}
		parent, ok := indexed[child.span.ParentSpanID]
		if !ok {
			t.Errorf("%s parent span %s was never exported", child.name, child.span.ParentSpanID)
			continue
		}
		if parent.Kind == obsv.SpanKindTool {
			t.Errorf("%s parents to a tool span (%s) instead of a step span; "+
				"fan-out tool anchor overwrote the step anchor",
				child.name, parent.SpanID)
		}
	}
}

// TestSpansByRunDisambiguatesByRunSpanID verifies that SpansByRun with a
// non-empty runSpanID returns only the spans for that specific run occurrence,
// even when a parent and nested run share the same RunID within one trace.
func TestSpansByRunDisambiguatesByRunSpanID(t *testing.T) {
	repo := obsv.NewMemoryRepository()
	observer := newObserver(t, synchronousConfig(obsv.Config{Repository: repo}))

	// A workflow with one agent step. The default ID sources give both the
	// workflow and the agent the same RunID ("agent-run-0001"), which is the
	// collision this test exercises.
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

	allSpans := repo.Spans()
	runSpans := spansByKind(allSpans, obsv.SpanKindRun)
	if len(runSpans) != 2 {
		t.Fatalf("stored %d run spans, want 2 (workflow and nested agent)", len(runSpans))
	}
	// Verify the premise: both runs share the same RunID.
	if runSpans[0].RunID != runSpans[1].RunID {
		t.Skipf("runtime no longer reuses run IDs across nested runs (%q, %q); collision case not exercised",
			runSpans[0].RunID, runSpans[1].RunID)
	}
	sharedRunID := runSpans[0].RunID
	traceID := runSpans[0].TraceID
	if runSpans[1].TraceID != traceID {
		t.Fatalf("both runs should share a trace; got %q and %q", traceID, runSpans[1].TraceID)
	}

	// Identify each run occurrence by its root span ID.
	rootA := runSpans[0]
	rootB := runSpans[1]

	// SpansByRun with rootA's RunSpanID returns only rootA's spans.
	spansA, err := repo.SpansByRun(context.Background(), traceID, sharedRunID, rootA.SpanID)
	if err != nil {
		t.Fatalf("SpansByRun(rootA) error = %v", err)
	}
	for _, s := range spansA {
		if s.RunSpanID != rootA.SpanID {
			t.Errorf("SpansByRun(rootA) returned span %s with RunSpanID %q, want %q",
				s.SpanID, s.RunSpanID, rootA.SpanID)
		}
	}
	if len(spansA) == 0 {
		t.Error("SpansByRun(rootA) returned no spans")
	}

	// SpansByRun with rootB's RunSpanID returns only rootB's spans.
	spansB, err := repo.SpansByRun(context.Background(), traceID, sharedRunID, rootB.SpanID)
	if err != nil {
		t.Fatalf("SpansByRun(rootB) error = %v", err)
	}
	for _, s := range spansB {
		if s.RunSpanID != rootB.SpanID {
			t.Errorf("SpansByRun(rootB) returned span %s with RunSpanID %q, want %q",
				s.SpanID, s.RunSpanID, rootB.SpanID)
		}
	}
	if len(spansB) == 0 {
		t.Error("SpansByRun(rootB) returned no spans")
	}

	// SpansByRun with empty RunSpanID returns both runs' spans (backward
	// compatible behavior for callers without the run span ID).
	spansAll, err := repo.SpansByRun(context.Background(), traceID, sharedRunID, "")
	if err != nil {
		t.Fatalf("SpansByRun(empty) error = %v", err)
	}
	if len(spansAll) <= len(spansA) || len(spansAll) <= len(spansB) {
		t.Errorf("SpansByRun(empty) returned %d spans, should be more than rootA (%d) or rootB (%d) alone",
			len(spansAll), len(spansA), len(spansB))
	}

	// Every span should carry a non-empty RunSpanID matching its run root.
	for _, s := range allSpans {
		if s.RunSpanID == "" {
			t.Errorf("span %s (%s) has empty RunSpanID; every span should carry its run root span ID",
				s.SpanID, s.Kind)
		}
	}
}
