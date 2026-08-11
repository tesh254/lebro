package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// SubagentErrorKind identifies the normalized category of a delegation
// failure.
type SubagentErrorKind string

const (
	// SubagentErrorInvalidInput means the delegation arguments could not be
	// decoded into the delegation contract. Schema validation happens in the
	// registry boundary before Execute runs, so this covers only a payload that
	// satisfied the schema but not the Go contract.
	SubagentErrorInvalidInput SubagentErrorKind = "subagent_invalid_input"
	// SubagentErrorRunFailed means the delegated run reached a terminal
	// non-success state. The wrapped error preserves the child's *AgentError so
	// callers can use errors.As to inspect the underlying cause.
	SubagentErrorRunFailed SubagentErrorKind = "subagent_run_failed"
	// SubagentErrorCancelled means the delegated run was cancelled, either by
	// the parent context or by the subagent's own deadline elapsing.
	SubagentErrorCancelled SubagentErrorKind = "subagent_cancelled"
)

var (
	// ErrSubagentInvalidInput matches delegation payloads that cannot be
	// decoded into the delegation contract.
	ErrSubagentInvalidInput = errors.New("lebro: subagent invalid input")
	// ErrSubagentRunFailed matches delegated runs that finished in a terminal
	// non-success state.
	ErrSubagentRunFailed = errors.New("lebro: subagent run failed")
	// ErrSubagentCancelled matches delegated runs aborted by context
	// cancellation or by the subagent's own deadline.
	ErrSubagentCancelled = errors.New("lebro: subagent cancelled")
)

// SubagentError preserves the category, subagent identity, and cause of a
// delegation failure.
type SubagentError struct {
	Kind SubagentErrorKind
	ID   ToolID
	Err  error
}

func (e *SubagentError) Error() string {
	if e == nil {
		return "lebro: subagent failure"
	}
	kind := e.Kind
	if kind == "" {
		kind = SubagentErrorRunFailed
	}
	if e.Err == nil {
		return fmt.Sprintf("lebro: subagent %q %s", e.ID, kind)
	}
	return fmt.Sprintf("lebro: subagent %q %s: %s", e.ID, kind, e.Err.Error())
}

// Unwrap exposes the child run error or context error.
func (e *SubagentError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Is supports errors.Is checks against the normalized ErrSubagent sentinels
// while Unwrap continues to preserve the original cause.
func (e *SubagentError) Is(target error) bool {
	if e == nil {
		return false
	}
	return target == subagentErrorSentinel(e.Kind)
}

func subagentErrorSentinel(kind SubagentErrorKind) error {
	switch kind {
	case SubagentErrorInvalidInput:
		return ErrSubagentInvalidInput
	case SubagentErrorRunFailed:
		return ErrSubagentRunFailed
	case SubagentErrorCancelled:
		return ErrSubagentCancelled
	default:
		return ErrSubagentRunFailed
	}
}

// subagentInputSchema is the default delegation contract. A supervisor states
// the delegated task and may add focusing context. Message roles stay
// application-controlled so a supervisor cannot inject a system prompt or a
// synthetic tool result into the child transcript.
const subagentInputSchema = `{
	"type":"object",
	"required":["task"],
	"properties":{
		"task":{"type":"string","description":"The focused task to delegate."},
		"context":{"type":"string","description":"Optional supporting context for the task."}
	},
	"additionalProperties":false
}`

// subagentOutputSchema is the default delegation result. It reports the child
// identity and terminal status alongside the output so a supervisor can reason
// about a delegation without a second lookup.
const subagentOutputSchema = `{
	"type":"object",
	"required":["agent_id","run_id","status","output"],
	"properties":{
		"agent_id":{"type":"string"},
		"run_id":{"type":"string"},
		"status":{"type":"string"},
		"output":{"type":"string"}
	},
	"additionalProperties":false
}`

// subagentInput is the decoded delegation contract.
type subagentInput struct {
	Task    string `json:"task"`
	Context string `json:"context,omitempty"`
}

// subagentOutput is the delegation result returned to the supervisor.
type subagentOutput struct {
	AgentID string `json:"agent_id"`
	RunID   string `json:"run_id"`
	Status  string `json:"status"`
	Output  string `json:"output"`
}

// SubagentConfig describes one named delegation target. ID and Agent are
// required; every other field has a safe default.
//
// The zero value of ShareThread and ShareMetadata is the isolating choice: a
// delegated run sees only the task it was given. Sharing is opt-in per
// subagent so a supervisor cannot widen a child's view of the parent thread by
// changing what it sends at call time.
type SubagentConfig struct {
	// ID is the stable tool ID the supervisor uses to select this subagent. It
	// must be unique within the supervisor's registry.
	ID ToolID
	// Agent is the delegation target. *Agent satisfies Workflow, so any agent
	// can be delegated to; so can any other Workflow implementation.
	Agent Workflow
	// Description tells the supervising model when to choose this subagent. It
	// defaults to the target's workflow description.
	Description string
	// MaxSteps bounds the delegated run independently of the parent. A zero
	// value leaves the target's own configured bound in place.
	//
	// This bound is advisory for targets that are not *Agent: only *Agent can
	// have its step budget narrowed per delegation. A non-Agent Workflow keeps
	// whatever bound it was built with.
	MaxSteps int
	// Deadline caps the delegated run independently of the parent. A zero value
	// means the child is bounded only by the parent context. The deadline is
	// layered on the parent context, so a parent that expires first still stops
	// the child.
	Deadline time.Duration
	// ShareThread passes the parent run's ThreadID to the delegated run, giving
	// the child the parent's persisted message history and appending the
	// child's transcript to the same thread. When false (the default), the
	// child runs against a fresh transcript containing only the delegated task.
	ShareThread bool
	// ShareMetadata passes the parent run's metadata to the delegated run. When
	// false (the default), the child receives only correlation metadata.
	ShareMetadata bool
	// InputSchema overrides the default delegation contract. When set, the
	// payload must still decode into a task and optional context.
	InputSchema json.RawMessage
	// OutputSchema overrides the default delegation result schema. When set, it
	// must accept the agent_id, run_id, status, and output fields this adapter
	// produces.
	OutputSchema json.RawMessage
}

// Subagent exposes a Workflow as a schema-backed, callable capability so a
// supervising agent can delegate focused work to it by stable ID.
//
// It implements Tool, so registering one in a ToolRegistry gives delegation the
// same execution boundary as any other tool: arguments are schema-validated
// before the child starts, results are validated on the way back, handler
// panics are contained, and the supervisor's tool allow-list governs which
// subagents it may reach. A supervisor selects a subagent through ordinary
// model tool-calling; no separate selection mechanism is involved.
//
// Delegated runs are bounded independently of the parent by MaxSteps and
// Deadline, and are correlated to the parent through the run event stream:
// every child event carries the parent's RunID, StepID, and step position.
//
// Nesting is permitted — a delegated agent may itself hold subagent tools —
// and each level is bounded by its own MaxSteps and Deadline. There is no
// global depth cap, so a recursive topology must be bounded by the deadlines
// its levels declare.
//
// The zero value is not usable; construct one with NewSubagent.
type Subagent struct {
	id            ToolID
	agent         Workflow
	definition    ToolDefinition
	maxSteps      int
	deadline      time.Duration
	shareThread   bool
	shareMetadata bool
}

var _ Tool = (*Subagent)(nil)

// NewSubagent validates the configuration and returns a delegation capability
// safe for concurrent use.
func NewSubagent(config SubagentConfig) (*Subagent, error) {
	if config.ID == "" {
		return nil, errors.New("lebro: subagent ID is required")
	}
	if config.Agent == nil || isNilInterface(config.Agent) {
		return nil, errors.New("lebro: subagent agent is required")
	}
	if config.MaxSteps < 0 {
		return nil, errors.New("lebro: subagent max steps must not be negative")
	}
	if config.Deadline < 0 {
		return nil, errors.New("lebro: subagent deadline must not be negative")
	}

	inputSchema := config.InputSchema
	if len(inputSchema) == 0 {
		inputSchema = json.RawMessage(subagentInputSchema)
	} else if !json.Valid(inputSchema) {
		return nil, errors.New("lebro: subagent input schema must be valid JSON")
	}
	outputSchema := config.OutputSchema
	if len(outputSchema) == 0 {
		outputSchema = json.RawMessage(subagentOutputSchema)
	} else if !json.Valid(outputSchema) {
		return nil, errors.New("lebro: subagent output schema must be valid JSON")
	}

	description := config.Description
	if description == "" {
		description = config.Agent.Definition().Description
	}

	return &Subagent{
		id:    config.ID,
		agent: config.Agent,
		definition: ToolDefinition{
			ID:           config.ID,
			Description:  description,
			InputSchema:  cloneRawMessage(inputSchema),
			OutputSchema: cloneRawMessage(outputSchema),
		},
		maxSteps:      config.MaxSteps,
		deadline:      config.Deadline,
		shareThread:   config.ShareThread,
		shareMetadata: config.ShareMetadata,
	}, nil
}

// Definition returns a caller-owned copy of the delegation capability's tool
// definition.
func (s *Subagent) Definition() ToolDefinition {
	if s == nil {
		return ToolDefinition{}
	}
	return cloneToolDefinition(s.definition)
}

// Execute runs the delegation target as a bounded child run and returns its
// terminal output.
//
// Cancellation is returned as the bare context error so the registry boundary
// classifies it as ToolExecutionCancelled rather than a handler failure. A
// child that exhausted its own deadline reports cancellation too, but the
// parent context stays live, so the supervisor observes a failed delegation
// rather than a cancelled run of its own.
func (s *Subagent) Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	if s == nil || s.agent == nil || isNilInterface(s.agent) {
		return nil, errors.New("lebro: subagent is nil")
	}
	if ctx == nil {
		return nil, errors.New("lebro: subagent context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var decoded subagentInput
	if err := json.Unmarshal(input, &decoded); err != nil {
		return nil, &SubagentError{Kind: SubagentErrorInvalidInput, ID: s.id, Err: fmt.Errorf("lebro: decode subagent input: %w", err)}
	}
	if decoded.Task == "" {
		return nil, &SubagentError{Kind: SubagentErrorInvalidInput, ID: s.id, Err: errors.New("lebro: subagent task must not be empty")}
	}

	parent := workflowInvocationFromContext(ctx)

	runCtx, cancel := s.applyDeadline(ctx)
	defer cancel()

	// Re-publish the parent invocation on the child context so the child's run
	// emitter stamps every event with the parent run, step ID, and step
	// position. The thread ID carried here is what the child would inherit, so
	// it is cleared unless sharing was configured.
	childThread := ThreadID("")
	if s.shareThread {
		childThread = parent.threadID
	}
	childMetadata := map[string]string(nil)
	if s.shareMetadata {
		childMetadata = parent.metadata
	}
	runCtx = withWorkflowInvocation(runCtx, parent.runID, parent.step, parent.stepID, childThread, childMetadata)

	result, err := s.runTarget(runCtx, RunInput{
		Messages: []Message{{Role: RoleUser, Content: delegationPrompt(decoded)}},
		ThreadID: childThread,
		Metadata: childMetadata,
	}, parent)
	if err != nil {
		// A parent that is itself cancelled reports cancellation; a child that
		// merely ran out of its own budget is a failed delegation the
		// supervisor can react to.
		if parentErr := ctx.Err(); parentErr != nil {
			return nil, parentErr
		}
		if errors.Is(err, ErrAgentCancelled) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, &SubagentError{Kind: SubagentErrorCancelled, ID: s.id, Err: err}
		}
		return nil, &SubagentError{Kind: SubagentErrorRunFailed, ID: s.id, Err: err}
	}
	if result.Status != RunStatusSucceeded {
		return nil, &SubagentError{Kind: SubagentErrorRunFailed, ID: s.id, Err: fmt.Errorf("lebro: subagent run finished with status %q: %w", result.Status, ErrSubagentRunFailed)}
	}

	return s.encodeResult(result)
}

// runTarget invokes the delegation target, narrowing the step budget for the
// duration of this delegation when the target is an *Agent and MaxSteps was
// configured. Other Workflow implementations keep their own bound.
//
// The child's identifiers are namespaced under the parent run so a delegated
// run is distinguishable from its parent. Agents each carry their own ID
// source, and the default source counts from one per agent, so a parent and its
// child would otherwise both report the first run as "agent-run-0001" and the
// correlation on their events would be ambiguous.
func (s *Subagent) runTarget(ctx context.Context, input RunInput, parent workflowInvocation) (RunResult, error) {
	agent, ok := s.agent.(*Agent)
	if !ok {
		return s.agent.Run(ctx, input)
	}
	maxSteps := s.maxSteps
	if maxSteps <= 0 {
		maxSteps = agent.maxSteps
	}
	return agent.runDelegated(ctx, input, maxSteps, delegatedIDPrefix(s.id, parent.runID))
}

// delegatedIDPrefix namespaces a delegated run's identifiers. It combines the
// parent run with the subagent ID so two subagents delegated from the same
// parent step still produce distinct child run IDs.
func delegatedIDPrefix(id ToolID, parentRun RunID) string {
	if parentRun == "" {
		return string(id)
	}
	return string(parentRun) + "/" + string(id)
}

// prefixedIDSource namespaces the identifiers of a delegated run under its
// parent. It delegates generation to the underlying source so the agent's
// configured source — including a fixed source in tests — still decides the
// sequence.
type prefixedIDSource struct {
	prefix string
	inner  IDSource
}

func (s prefixedIDSource) NewRunID() RunID {
	return RunID(s.prefix + "/" + string(s.inner.NewRunID()))
}

func (s prefixedIDSource) NewStepID() StepID {
	return s.inner.NewStepID()
}

// encodeResult renders the child's terminal output as the delegation result.
// Structured output is passed through as-is; otherwise the final assistant
// message content is used, so a supervisor always receives a string it can
// reason about.
func (s *Subagent) encodeResult(result RunResult) (json.RawMessage, error) {
	output := ""
	if structured := result.StructuredOutput(); structured != "" {
		output = string(structured.Raw())
	} else {
		for i := len(result.Messages) - 1; i >= 0; i-- {
			if result.Messages[i].Role == RoleAssistant {
				output = result.Messages[i].Content
				break
			}
		}
	}

	encoded, err := json.Marshal(subagentOutput{
		AgentID: string(s.agent.Definition().ID),
		RunID:   string(result.ID),
		Status:  string(result.Status),
		Output:  output,
	})
	if err != nil {
		return nil, &SubagentError{Kind: SubagentErrorRunFailed, ID: s.id, Err: fmt.Errorf("lebro: encode subagent result: %w", err)}
	}
	return encoded, nil
}

func (s *Subagent) applyDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.deadline <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, s.deadline)
}

// delegationPrompt renders the delegation contract as the single user message
// that seeds the child transcript.
func delegationPrompt(input subagentInput) string {
	if input.Context == "" {
		return input.Task
	}
	return input.Task + "\n\n" + input.Context
}
