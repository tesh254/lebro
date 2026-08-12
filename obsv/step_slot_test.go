package obsv_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/obsv"
)

// TestBranchSelectedAttachesToItsStep pins the fix for a slot-keying defect.
//
// A branch_selected event reports the branch a conditional step *selected*, while
// the step span was opened from an event carrying no branch. Keying the in-flight
// step slot on the branch therefore made the lookup miss, and the event landed on
// the run root with the step span left unlabelled — so a trace could not show
// which branch a step took.
func TestBranchSelectedAttachesToItsStep(t *testing.T) {
	spans := obsv.NewMemorySpanExporter()
	observer := newObserver(t, synchronousConfig(obsv.Config{Spans: spans}))

	workflow, err := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition: lebro.WorkflowDefinition{ID: "router"},
		Listener:   observer,
		Clock:      newSteppingClock(time.Millisecond),
		Steps: []lebro.Step{{
			Definition: lebro.StepDefinition{
				ID: "route",
				Branches: []lebro.Branch{
					{
						Name:      "chosen",
						Condition: func(context.Context, json.RawMessage) (bool, error) { return true, nil },
						Steps: []lebro.Step{{
							Definition: lebro.StepDefinition{ID: "chosen-step"},
							Handler: lebro.StepHandlerFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
								return json.RawMessage(`"done"`), nil
							}),
						}},
					},
					{
						Name:      "skipped",
						Condition: func(context.Context, json.RawMessage) (bool, error) { return false, nil },
						Steps: []lebro.Step{{
							Definition: lebro.StepDefinition{ID: "skipped-step"},
							Handler: lebro.StepHandlerFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
								return json.RawMessage(`"unreachable"`), nil
							}),
						}},
					},
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("NewLinearWorkflow() error = %v", err)
	}
	if _, err := workflow.Run(context.Background(), lebro.WorkflowRunInput{Input: json.RawMessage(`"go"`)}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	exported := spans.Spans()
	routeStep := findSpan(t, exported, "the route step span", func(span obsv.Span) bool {
		return span.Kind == obsv.SpanKindStep && span.StepID == "route"
	})

	// The event belongs to the branching step, not the run root.
	if !hasEvent(routeStep, "branch_selected") {
		t.Errorf("route step span has no branch_selected event; events = %v", eventNames(routeStep))
	}
	runSpan := findSpan(t, exported, "the run span", func(span obsv.Span) bool {
		return span.Kind == obsv.SpanKindRun
	})
	if hasEvent(runSpan, "branch_selected") {
		t.Error("branch_selected recorded on the run span; it belongs to the branching step")
	}

	// The step is labelled with the branch it took, which is the point of the
	// event.
	if got := routeStep.Attributes[obsv.AttrBranch]; got != "chosen" {
		t.Errorf("route step branch attribute = %q, want %q", got, "chosen")
	}
}

// TestFanOutChildAndRetryAttemptParentCorrectly pins the fix for the same keying
// defect as seen from a fan-out.
//
// The runtime reports a fan-out child at the same step position as the fan-out
// step that launched it, and carries the branch name on the child's step
// lifecycle events but *not* on its retry-attempt events. Keying the slot on
// position and branch therefore misplaced both spans: the child parented to the
// run root instead of the fan-out step, and the retry attempt parented to the
// fan-out step instead of the child, so the fan-out subtree read as empty.
func TestFanOutChildAndRetryAttemptParentCorrectly(t *testing.T) {
	spans := obsv.NewMemorySpanExporter()
	observer := newObserver(t, synchronousConfig(obsv.Config{Spans: spans}))

	flaky := errors.New("transient")
	var attempts int
	workflow, err := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition: lebro.WorkflowDefinition{ID: "fanout-retry"},
		Listener:   observer,
		Clock:      newSteppingClock(time.Millisecond),
		Steps: []lebro.Step{{
			Definition: lebro.StepDefinition{
				ID: "fanout",
				FanOut: &lebro.FanOut{
					MaxParallel: 1,
					Branches: []lebro.FanOutBranch{{
						Name: "only",
						Steps: []lebro.Step{{
							Definition: lebro.StepDefinition{
								ID:    "child",
								Retry: &lebro.RetryPolicy{Attempts: 3},
							},
							Handler: lebro.StepHandlerFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
								attempts++
								if attempts == 1 {
									return nil, flaky
								}
								return json.RawMessage(`{"ok":true}`), nil
							}),
						}},
					}},
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("NewLinearWorkflow() error = %v", err)
	}
	if _, err := workflow.Run(context.Background(), lebro.WorkflowRunInput{Input: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if attempts < 2 {
		t.Fatalf("handler ran %d times, want a retry so the attempt span exists", attempts)
	}

	exported := spans.Spans()
	indexed := spanByID(exported)

	fanOut := findSpan(t, exported, "the fan-out step span", func(span obsv.Span) bool {
		return span.Kind == obsv.SpanKindStep && span.StepID == "fanout"
	})
	child := findSpan(t, exported, "the fan-out child step span", func(span obsv.Span) bool {
		return span.Kind == obsv.SpanKindStep && span.StepID == "child"
	})
	attempt := findSpan(t, exported, "the retry attempt span", func(span obsv.Span) bool {
		return span.Kind == obsv.SpanKindStepAttempt
	})

	// The child hangs off the fan-out step that launched it.
	if child.ParentSpanID != fanOut.SpanID {
		parent := indexed[child.ParentSpanID]
		t.Errorf("fan-out child parents to %s (%s/%s), want the fanout step %s", child.ParentSpanID, parent.Kind, parent.StepID, fanOut.SpanID)
	}
	// The retry attempt hangs off the child whose attempt it is, not off the
	// enclosing fan-out step it shares a position with.
	if attempt.ParentSpanID != child.SpanID {
		parent := indexed[attempt.ParentSpanID]
		t.Errorf("retry attempt parents to %s (%s/%s), want the child step %s", attempt.ParentSpanID, parent.Kind, parent.StepID, child.SpanID)
	}
	// The branch identity survives on the child, so concurrent branches stay
	// distinguishable.
	if got := child.Attributes[obsv.AttrBranch]; got != "only" {
		t.Errorf("fan-out child branch attribute = %q, want %q", got, "only")
	}
}

// TestConcurrentFanOutBranchesKeepSeparateSpans checks that the slot change did
// not reintroduce the collision it replaced: two branches running at the same
// step position must not overwrite one another.
//
// The runtime rejects a workflow that declares one step ID twice, so concurrent
// branches always differ by step ID and the position alone is the collision
// risk.
func TestConcurrentFanOutBranchesKeepSeparateSpans(t *testing.T) {
	spans := obsv.NewMemorySpanExporter()
	observer := newObserver(t, synchronousConfig(obsv.Config{Spans: spans}))

	workflow, err := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition: lebro.WorkflowDefinition{ID: "fanout-parallel"},
		Listener:   observer,
		Clock:      newSteppingClock(time.Millisecond),
		Steps: []lebro.Step{{
			Definition: lebro.StepDefinition{
				ID: "fanout",
				FanOut: &lebro.FanOut{
					MaxParallel: 2,
					Branches: []lebro.FanOutBranch{
						{Name: "a", Steps: []lebro.Step{{
							Definition: lebro.StepDefinition{ID: "leaf-a"},
							Handler: lebro.StepHandlerFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
								return json.RawMessage(`{"branch":"a"}`), nil
							}),
						}}},
						{Name: "b", Steps: []lebro.Step{{
							Definition: lebro.StepDefinition{ID: "leaf-b"},
							Handler: lebro.StepHandlerFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
								return json.RawMessage(`{"branch":"b"}`), nil
							}),
						}}},
					},
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("NewLinearWorkflow() error = %v", err)
	}
	if _, err := workflow.Run(context.Background(), lebro.WorkflowRunInput{Input: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// Both branches run at the same step position concurrently. Two spans must
	// be exported, one per branch, rather than one overwriting the other.
	var leaves []obsv.Span
	for _, span := range spans.Spans() {
		if span.Kind == obsv.SpanKindStep && (span.StepID == "leaf-a" || span.StepID == "leaf-b") {
			leaves = append(leaves, span)
		}
	}
	if len(leaves) != 2 {
		t.Fatalf("exported %d leaf step spans, want 2 (one per fan-out branch)", len(leaves))
	}
	if leaves[0].SpanID == leaves[1].SpanID {
		t.Fatal("both fan-out branches exported one span; the slot collides")
	}
	branches := map[string]bool{}
	for _, span := range leaves {
		branches[span.Attributes[obsv.AttrBranch]] = true
	}
	if !branches["a"] || !branches["b"] {
		t.Errorf("leaf spans carry branches %v, want both %q and %q", branches, "a", "b")
	}
}

func hasEvent(span obsv.Span, name string) bool {
	for _, event := range span.Events {
		if event.Name == name {
			return true
		}
	}
	return false
}

func eventNames(span obsv.Span) []string {
	names := make([]string, 0, len(span.Events))
	for _, event := range span.Events {
		names = append(names, event.Name)
	}
	return names
}
