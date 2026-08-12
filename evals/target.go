package evals

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tesh254/lebro"
)

// Output is the provider-neutral result of invoking a target on one case. It is
// what every scorer reads, so a scorer works against an agent and a workflow
// without knowing which produced the output.
//
// Text is the answer as text: the final assistant message's content for a
// message-centric target, or the JSON output rendered as a string for a
// JSON-step workflow. Structured carries the structured JSON payload when the
// target produced one. Status is the target's own run status, and RunID
// correlates the case result back to the run's traces and stored records.
type Output struct {
	Text       string            `json:"text,omitempty"`
	Structured json.RawMessage   `json:"structured,omitempty"`
	Status     lebro.RunStatus   `json:"status,omitempty"`
	RunID      lebro.RunID       `json:"run_id,omitempty"`
	Usage      lebro.ModelUsage  `json:"usage,omitzero"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// Clone returns a deep copy of the output.
func (o Output) Clone() Output {
	o.Structured = cloneRawJSON(o.Structured)
	o.Metadata = cloneMetadata(o.Metadata)
	return o
}

// Target is the thing under evaluation. Implementations must honor context
// cancellation and must not retain the Case they are given past the call.
//
// A returned error means the target failed to produce an answer for this case;
// the experiment records it as a target failure and does not run scorers. An
// error is not a test failure — a dataset may deliberately include cases a
// target is expected to reject.
type Target interface {
	// Name identifies the target in stored records.
	Name() string
	Invoke(context.Context, Case) (Output, error)
}

// MessageRunner is the subset of lebro.Workflow that AgentTarget needs.
// *lebro.Agent satisfies it, as does anything else implementing lebro.Workflow.
type MessageRunner interface {
	Definition() lebro.WorkflowDefinition
	Run(context.Context, lebro.RunInput) (lebro.RunResult, error)
}

// JSONRunner is the run contract of a JSON-step workflow.
// *lebro.LinearWorkflow satisfies it.
type JSONRunner interface {
	Definition() lebro.WorkflowDefinition
	Run(context.Context, lebro.WorkflowRunInput) (lebro.WorkflowRunResult, error)
}

// AgentTarget evaluates a message-centric primitive — an agent, or any other
// lebro.Workflow implementation.
//
// A case is invoked with its Messages when it has them. When it carries only a
// JSON input, the input is passed as a single user message so one dataset can
// serve both target kinds; a JSON input that is a bare JSON string is unquoted
// first, because "what did the user type" is the string's value and not its JSON
// encoding.
type AgentTarget struct {
	runner MessageRunner
	name   string
}

// NewAgentTarget adapts a message-centric runner into a Target. The name
// defaults to the runner's definition name, then its ID.
func NewAgentTarget(runner MessageRunner) *AgentTarget {
	if runner == nil {
		return nil
	}
	definition := runner.Definition()
	name := definition.Name
	if name == "" {
		name = string(definition.ID)
	}
	return &AgentTarget{runner: runner, name: name}
}

var _ Target = (*AgentTarget)(nil)

// Name returns the target's name.
func (t *AgentTarget) Name() string {
	if t == nil {
		return ""
	}
	return t.name
}

// Invoke runs the case against the agent and reduces the result to an Output.
func (t *AgentTarget) Invoke(ctx context.Context, testCase Case) (Output, error) {
	if t == nil || t.runner == nil {
		return Output{}, ErrNoTarget
	}
	messages, err := agentMessages(testCase)
	if err != nil {
		return Output{}, err
	}
	// The metadata map is copied because the runner may retain or mutate it, and
	// the case it came from is reused by every later experiment run.
	result, runErr := t.runner.Run(ctx, lebro.RunInput{
		Messages: messages,
		Metadata: cloneMetadata(testCase.Metadata),
	})
	// A failed run still carries an ID and a status worth recording, so the
	// output is built before the error is returned.
	output := Output{
		Status:     result.Status,
		RunID:      result.ID,
		Structured: result.StructuredOutput().Raw(),
		Metadata:   cloneMetadata(result.Metadata),
	}
	output.Text = finalAssistantContent(result.Messages)
	if runErr != nil {
		return output, runErr
	}
	return output, nil
}

// agentMessages derives the conversation to send for a case.
func agentMessages(testCase Case) ([]lebro.Message, error) {
	if len(testCase.Messages) > 0 {
		return append([]lebro.Message(nil), testCase.Messages...), nil
	}
	if len(testCase.Input) == 0 {
		return nil, fmt.Errorf("%w: case %q carries no messages or input", ErrTargetUnsupportedCase, testCase.ID)
	}
	content := string(testCase.Input)
	// A bare JSON string input is the user's text, not its encoding.
	var unquoted string
	if err := json.Unmarshal(testCase.Input, &unquoted); err == nil {
		content = unquoted
	}
	return []lebro.Message{{Role: lebro.RoleUser, Content: content}}, nil
}

// finalAssistantContent returns the content of the last assistant message, which
// is the answer a scorer grades.
func finalAssistantContent(messages []lebro.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == lebro.RoleAssistant {
			return messages[i].Content
		}
	}
	return ""
}

// WorkflowTarget evaluates a JSON-step workflow such as *lebro.LinearWorkflow.
//
// A case must carry a JSON Input; a case with only Messages is rejected with
// ErrTargetUnsupportedCase rather than being silently reshaped, because there is
// no single correct way to turn a conversation into a step input. The workflow's
// final step output becomes both Structured and, rendered as compact JSON, Text —
// so text-oriented scorers work against a JSON workflow.
type WorkflowTarget struct {
	runner JSONRunner
	name   string
}

// NewWorkflowTarget adapts a JSON-step workflow into a Target. The name defaults
// to the workflow's definition name, then its ID.
func NewWorkflowTarget(runner JSONRunner) *WorkflowTarget {
	if runner == nil {
		return nil
	}
	definition := runner.Definition()
	name := definition.Name
	if name == "" {
		name = string(definition.ID)
	}
	return &WorkflowTarget{runner: runner, name: name}
}

var _ Target = (*WorkflowTarget)(nil)

// Name returns the target's name.
func (t *WorkflowTarget) Name() string {
	if t == nil {
		return ""
	}
	return t.name
}

// Invoke runs the case against the workflow and reduces the result to an Output.
func (t *WorkflowTarget) Invoke(ctx context.Context, testCase Case) (Output, error) {
	if t == nil || t.runner == nil {
		return Output{}, ErrNoTarget
	}
	if len(testCase.Input) == 0 {
		return Output{}, fmt.Errorf("%w: case %q carries no JSON input", ErrTargetUnsupportedCase, testCase.ID)
	}
	result, runErr := t.runner.Run(ctx, lebro.WorkflowRunInput{
		Input: cloneRawJSON(testCase.Input),
		// Copied for the same reason as the input: the workflow may retain or
		// mutate it, and the case outlives the run.
		Metadata: cloneMetadata(testCase.Metadata),
	})
	output := Output{
		Status:   result.Status,
		RunID:    result.ID,
		Metadata: cloneMetadata(result.Metadata),
	}
	if len(result.Output) > 0 {
		output.Structured = cloneRawJSON(result.Output)
		if canonical, err := canonicalJSON(result.Output); err == nil {
			output.Text = string(canonical)
		} else {
			output.Text = string(result.Output)
		}
	}
	if runErr != nil {
		return output, runErr
	}
	return output, nil
}

// TargetFunc adapts an ordinary function into a Target, which keeps a test or a
// one-off evaluation from needing a type.
type TargetFunc struct {
	TargetName string
	Fn         func(context.Context, Case) (Output, error)
}

var _ Target = TargetFunc{}

// Name returns the configured name.
func (f TargetFunc) Name() string { return f.TargetName }

// Invoke calls Fn. A nil Fn reports ErrNoTarget.
func (f TargetFunc) Invoke(ctx context.Context, testCase Case) (Output, error) {
	if f.Fn == nil {
		return Output{}, ErrNoTarget
	}
	return f.Fn(ctx, testCase)
}
