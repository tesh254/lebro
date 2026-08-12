package evals_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/evals"
)

// fakeMessageRunner is a message-centric runner that records what it was asked
// and returns a scripted result.
type fakeMessageRunner struct {
	definition lebro.WorkflowDefinition
	result     lebro.RunResult
	err        error
	seen       []lebro.Message
	seenMeta   map[string]string
}

func (r *fakeMessageRunner) Definition() lebro.WorkflowDefinition { return r.definition }

func (r *fakeMessageRunner) Run(_ context.Context, input lebro.RunInput) (lebro.RunResult, error) {
	r.seen = input.Messages
	r.seenMeta = input.Metadata
	return r.result, r.err
}

// fakeJSONRunner is a JSON-step runner with the same shape as
// *lebro.LinearWorkflow.
type fakeJSONRunner struct {
	definition lebro.WorkflowDefinition
	result     lebro.WorkflowRunResult
	err        error
	seen       json.RawMessage
}

func (r *fakeJSONRunner) Definition() lebro.WorkflowDefinition { return r.definition }

func (r *fakeJSONRunner) Run(_ context.Context, input lebro.WorkflowRunInput) (lebro.WorkflowRunResult, error) {
	r.seen = input.Input
	return r.result, r.err
}

func TestAgentTargetReducesRunResult(t *testing.T) {
	runner := &fakeMessageRunner{
		definition: lebro.WorkflowDefinition{ID: "agent-1", Name: "support"},
		result: lebro.RunResult{
			ID:     "run-1",
			Status: lebro.RunStatusSucceeded,
			Messages: []lebro.Message{
				{Role: lebro.RoleUser, Content: "question"},
				{Role: lebro.RoleAssistant, Content: "first"},
				{Role: lebro.RoleAssistant, Content: "final", StructuredOutput: lebro.NewModelStructuredOutput(json.RawMessage(`{"a":1}`))},
			},
		},
	}
	target := evals.NewAgentTarget(runner)
	if target.Name() != "support" {
		t.Fatalf("Name() = %q, want %q", target.Name(), "support")
	}

	output, err := target.Invoke(context.Background(), evals.Case{
		ID:       "c1",
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "question"}},
	})
	if err != nil {
		t.Fatalf("Invoke() = %v", err)
	}
	if output.Text != "final" {
		t.Fatalf("Text = %q, want the final assistant message %q", output.Text, "final")
	}
	if string(output.Structured) != `{"a":1}` {
		t.Fatalf("Structured = %s, want {\"a\":1}", output.Structured)
	}
	if output.RunID != "run-1" || output.Status != lebro.RunStatusSucceeded {
		t.Fatalf("identity = (%q, %q), want (run-1, succeeded)", output.RunID, output.Status)
	}
}

// TestAgentTargetNamesFromDefinitionID covers the fallback when a definition has
// no name.
func TestAgentTargetNamesFromDefinitionID(t *testing.T) {
	target := evals.NewAgentTarget(&fakeMessageRunner{definition: lebro.WorkflowDefinition{ID: "agent-7"}})
	if target.Name() != "agent-7" {
		t.Fatalf("Name() = %q, want %q", target.Name(), "agent-7")
	}
}

// TestAgentTargetUnquotesJSONStringInput pins that a JSON-string input reaches
// the agent as the user's text rather than as its JSON encoding, which is what
// lets one dataset serve both target kinds.
func TestAgentTargetUnquotesJSONStringInput(t *testing.T) {
	runner := &fakeMessageRunner{result: lebro.RunResult{Status: lebro.RunStatusSucceeded}}
	target := evals.NewAgentTarget(runner)

	if _, err := target.Invoke(context.Background(), evals.Case{ID: "c1", Input: json.RawMessage(`"what is 2+2?"`)}); err != nil {
		t.Fatalf("Invoke() = %v", err)
	}
	if len(runner.seen) != 1 {
		t.Fatalf("sent %d messages, want 1", len(runner.seen))
	}
	if runner.seen[0].Content != "what is 2+2?" {
		t.Fatalf("Content = %q, want the unquoted string", runner.seen[0].Content)
	}
	if runner.seen[0].Role != lebro.RoleUser {
		t.Fatalf("Role = %q, want user", runner.seen[0].Role)
	}
}

// TestAgentTargetPassesObjectInputVerbatim covers the other branch: a JSON object
// has no string value to unquote, so it is sent as JSON text.
func TestAgentTargetPassesObjectInputVerbatim(t *testing.T) {
	runner := &fakeMessageRunner{result: lebro.RunResult{Status: lebro.RunStatusSucceeded}}
	target := evals.NewAgentTarget(runner)

	if _, err := target.Invoke(context.Background(), evals.Case{ID: "c1", Input: json.RawMessage(`{"q":"hi"}`)}); err != nil {
		t.Fatalf("Invoke() = %v", err)
	}
	if runner.seen[0].Content != `{"q":"hi"}` {
		t.Fatalf("Content = %q, want the raw JSON object", runner.seen[0].Content)
	}
}

// TestAgentTargetRecordsFailedRunIdentity pins that a failed run still yields the
// run ID and status: an experiment records what happened rather than discarding
// the evidence with the error.
func TestAgentTargetRecordsFailedRunIdentity(t *testing.T) {
	wantErr := errors.New("provider unavailable")
	runner := &fakeMessageRunner{
		result: lebro.RunResult{ID: "run-9", Status: lebro.RunStatusFailed},
		err:    wantErr,
	}
	target := evals.NewAgentTarget(runner)

	output, err := target.Invoke(context.Background(), evals.Case{ID: "c1", Input: json.RawMessage(`"x"`)})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Invoke() = %v, want %v", err, wantErr)
	}
	if output.RunID != "run-9" || output.Status != lebro.RunStatusFailed {
		t.Fatalf("identity = (%q, %q), want (run-9, failed)", output.RunID, output.Status)
	}
}

func TestAgentTargetRejectsCaseWithNoInput(t *testing.T) {
	target := evals.NewAgentTarget(&fakeMessageRunner{})
	_, err := target.Invoke(context.Background(), evals.Case{ID: "c1"})
	if !errors.Is(err, evals.ErrTargetUnsupportedCase) {
		t.Fatalf("Invoke() = %v, want ErrTargetUnsupportedCase", err)
	}
}

func TestWorkflowTargetReducesRunResult(t *testing.T) {
	runner := &fakeJSONRunner{
		definition: lebro.WorkflowDefinition{ID: "wf-1", Name: "triage"},
		result: lebro.WorkflowRunResult{
			ID:     "run-2",
			Status: lebro.RunStatusSucceeded,
			Output: json.RawMessage("{\n  \"b\": 2,\n  \"a\": 1\n}"),
		},
	}
	target := evals.NewWorkflowTarget(runner)
	if target.Name() != "triage" {
		t.Fatalf("Name() = %q, want %q", target.Name(), "triage")
	}

	output, err := target.Invoke(context.Background(), evals.Case{ID: "c1", Input: json.RawMessage(`{"ticket":1}`)})
	if err != nil {
		t.Fatalf("Invoke() = %v", err)
	}
	// Text is canonical so a text scorer sees a stable rendering regardless of how
	// the workflow formatted its output.
	if output.Text != `{"a":1,"b":2}` {
		t.Fatalf("Text = %q, want canonical JSON", output.Text)
	}
	if output.RunID != "run-2" {
		t.Fatalf("RunID = %q, want run-2", output.RunID)
	}
	if string(runner.seen) != `{"ticket":1}` {
		t.Fatalf("workflow saw %s, want the case input", runner.seen)
	}
}

// TestWorkflowTargetRejectsMessageOnlyCase pins the deliberate refusal: there is
// no single correct way to turn a conversation into a step input, so the case is
// rejected rather than silently reshaped.
func TestWorkflowTargetRejectsMessageOnlyCase(t *testing.T) {
	target := evals.NewWorkflowTarget(&fakeJSONRunner{})
	_, err := target.Invoke(context.Background(), evals.Case{
		ID:       "c1",
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
	})
	if !errors.Is(err, evals.ErrTargetUnsupportedCase) {
		t.Fatalf("Invoke() = %v, want ErrTargetUnsupportedCase", err)
	}
}

// TestWorkflowTargetDoesNotAliasCaseInput pins that the workflow cannot mutate
// the dataset through the slice it was handed.
func TestWorkflowTargetDoesNotAliasCaseInput(t *testing.T) {
	runner := &fakeJSONRunner{result: lebro.WorkflowRunResult{Status: lebro.RunStatusSucceeded}}
	target := evals.NewWorkflowTarget(runner)
	input := json.RawMessage(`{"a":1}`)

	if _, err := target.Invoke(context.Background(), evals.Case{ID: "c1", Input: input}); err != nil {
		t.Fatalf("Invoke() = %v", err)
	}
	runner.seen[2] = 'X'
	if string(input) != `{"a":1}` {
		t.Fatalf("case input mutated to %s", input)
	}
}

// TestTargetsDoNotAliasCaseMetadata pins that a runner cannot mutate the dataset
// through the metadata map it was handed. The case outlives the run — every later
// experiment reuses it — so a target writing to that map would silently change
// what subsequent runs evaluate.
func TestTargetsDoNotAliasCaseMetadata(t *testing.T) {
	t.Run("agent target", func(t *testing.T) {
		runner := &fakeMessageRunner{result: lebro.RunResult{Status: lebro.RunStatusSucceeded}}
		target := evals.NewAgentTarget(runner)
		metadata := map[string]string{"tag": "smoke"}

		if _, err := target.Invoke(context.Background(), evals.Case{
			ID: "c1", Input: json.RawMessage(`"x"`), Metadata: metadata,
		}); err != nil {
			t.Fatalf("Invoke() = %v", err)
		}
		runner.seenMeta["tag"] = "mutated"
		if metadata["tag"] != "smoke" {
			t.Fatalf("case metadata mutated to %q", metadata["tag"])
		}
	})

	t.Run("workflow target", func(t *testing.T) {
		runner := &recordingJSONRunner{result: lebro.WorkflowRunResult{Status: lebro.RunStatusSucceeded}}
		target := evals.NewWorkflowTarget(runner)
		metadata := map[string]string{"tag": "smoke"}

		if _, err := target.Invoke(context.Background(), evals.Case{
			ID: "c1", Input: json.RawMessage(`{"a":1}`), Metadata: metadata,
		}); err != nil {
			t.Fatalf("Invoke() = %v", err)
		}
		runner.seenMeta["tag"] = "mutated"
		if metadata["tag"] != "smoke" {
			t.Fatalf("case metadata mutated to %q", metadata["tag"])
		}
	})
}

// recordingJSONRunner captures the metadata map it was handed so a test can try
// to mutate the caller's dataset through it.
type recordingJSONRunner struct {
	result   lebro.WorkflowRunResult
	seenMeta map[string]string
}

func (r *recordingJSONRunner) Definition() lebro.WorkflowDefinition {
	return lebro.WorkflowDefinition{ID: "wf"}
}

func (r *recordingJSONRunner) Run(_ context.Context, input lebro.WorkflowRunInput) (lebro.WorkflowRunResult, error) {
	r.seenMeta = input.Metadata
	return r.result, nil
}

func TestNewTargetsReturnNilForNilRunner(t *testing.T) {
	if target := evals.NewAgentTarget(nil); target != nil {
		t.Fatal("NewAgentTarget(nil) returned a target")
	}
	if target := evals.NewWorkflowTarget(nil); target != nil {
		t.Fatal("NewWorkflowTarget(nil) returned a target")
	}
}

// TestAgentSatisfiesMessageRunner is a compile-time guarantee that the real
// *lebro.Agent works as a target without an adapter written by the caller.
func TestAgentSatisfiesMessageRunner(t *testing.T) {
	var _ evals.MessageRunner = (*lebro.Agent)(nil)
	var _ evals.JSONRunner = (*lebro.LinearWorkflow)(nil)
}
