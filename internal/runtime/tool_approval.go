package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ToolApprovalOutcome is the policy result for one model-requested tool call.
// Require suspends before the handler starts; Deny rejects the call without
// invoking the handler; Allow preserves the ordinary agent execution path.
type ToolApprovalOutcome string

const (
	ToolApprovalAllow   ToolApprovalOutcome = "allow"
	ToolApprovalRequire ToolApprovalOutcome = "require_approval"
	ToolApprovalDeny    ToolApprovalOutcome = "deny"
)

// ToolApprovalInvocation is the content-bearing input supplied only to an
// opted-in approval policy. Arguments are a caller-owned canonical copy of the
// model request; lifecycle events and diagnostic records remain content-free.
type ToolApprovalInvocation struct {
	RunID       RunID
	ExecutionID string
	Step        int
	StepID      StepID
	ThreadID    ThreadID
	ToolCallID  string
	ToolID      ToolID
	Arguments   json.RawMessage
	Metadata    map[string]string
}

// ToolApprovalResolution describes how to handle one invocation. Action and
// Resource are optional reviewer metadata; their zero values identify the
// model-requested tool call. TTL zero means the request does not expire.
type ToolApprovalResolution struct {
	Outcome   ToolApprovalOutcome
	Action    Action
	Resource  Resource
	Reason    string
	Requester string
	Metadata  map[string]string
	TTL       time.Duration
}

// ToolApprovalPolicy classifies a model-requested tool call before its handler
// begins. Implementations must be deterministic for a persisted request: once
// Require is returned, the persisted request is authoritative and Resume never
// calls the policy again for that invocation.
type ToolApprovalPolicy interface {
	EvaluateToolApproval(context.Context, ToolApprovalInvocation) (ToolApprovalResolution, error)
}

// ToolApprovalPolicyFunc adapts a function to ToolApprovalPolicy.
type ToolApprovalPolicyFunc func(context.Context, ToolApprovalInvocation) (ToolApprovalResolution, error)

func (f ToolApprovalPolicyFunc) EvaluateToolApproval(ctx context.Context, invocation ToolApprovalInvocation) (ToolApprovalResolution, error) {
	if f == nil {
		return ToolApprovalResolution{Outcome: ToolApprovalAllow}, nil
	}
	return f(ctx, invocation)
}

// ToolApprovalRequest is the immutable, durable review record for a protected
// tool call. Resume uses RequestID and RunID to locate this exact request; it
// never accepts replacement tool identity or arguments from the decision.
type ToolApprovalRequest struct {
	ID          string            `json:"id"`
	RunID       RunID             `json:"run_id"`
	ExecutionID string            `json:"execution_id,omitempty"`
	Step        int               `json:"step"`
	StepID      StepID            `json:"step_id"`
	ThreadID    ThreadID          `json:"thread_id,omitempty"`
	ToolCallID  string            `json:"tool_call_id"`
	ToolID      ToolID            `json:"tool_id"`
	Arguments   json.RawMessage   `json:"arguments"`
	Action      Action            `json:"action"`
	Resource    Resource          `json:"resource"`
	Reason      string            `json:"reason,omitempty"`
	Requester   string            `json:"requester,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	RequestedAt time.Time         `json:"requested_at"`
	ExpiresAt   time.Time         `json:"expires_at,omitempty"`
}

// ToolApprovalDecision is the only human input accepted at the resume
// boundary. It intentionally contains no tool name or arguments, so a caller
// cannot replace what was reviewed. DecidedAt is audit data and is checked
// against the persisted expiry before any handler starts.
type ToolApprovalDecision struct {
	RunID     RunID     `json:"run_id"`
	RequestID string    `json:"request_id"`
	Approved  bool      `json:"approved"`
	Decider   string    `json:"decider,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	DecidedAt time.Time `json:"decided_at"`
}

// ToolApprovalErrorKind identifies a terminal or coordination outcome at the
// approval boundary.
type ToolApprovalErrorKind string

const (
	ToolApprovalErrorDenied    ToolApprovalErrorKind = "denied"
	ToolApprovalErrorExpired   ToolApprovalErrorKind = "expired"
	ToolApprovalErrorStale     ToolApprovalErrorKind = "stale"
	ToolApprovalErrorUncertain ToolApprovalErrorKind = "execution_uncertain"
)

var (
	ErrToolApprovalDenied    = errors.New("lebro: tool approval denied")
	ErrToolApprovalExpired   = errors.New("lebro: tool approval expired")
	ErrToolApprovalStale     = errors.New("lebro: tool approval stale")
	ErrToolApprovalUncertain = errors.New("lebro: tool approval execution uncertain")
)

// ToolApprovalError preserves the request identity and typed cause of a
// decision outcome. An uncertain request is intentionally not replayable:
// the durable action-start marker may have been written before a process died.
type ToolApprovalError struct {
	Kind      ToolApprovalErrorKind
	RequestID string
	Err       error
}

func (e *ToolApprovalError) Error() string {
	if e == nil {
		return "lebro: tool approval failure"
	}
	if e.Err != nil {
		return fmt.Sprintf("lebro: tool approval %s for %q: %v", e.Kind, e.RequestID, e.Err)
	}
	return fmt.Sprintf("lebro: tool approval %s for %q", e.Kind, e.RequestID)
}

func (e *ToolApprovalError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *ToolApprovalError) Is(target error) bool {
	if e == nil {
		return false
	}
	switch e.Kind {
	case ToolApprovalErrorDenied:
		return target == ErrToolApprovalDenied
	case ToolApprovalErrorExpired:
		return target == ErrToolApprovalExpired
	case ToolApprovalErrorStale:
		return target == ErrToolApprovalStale
	case ToolApprovalErrorUncertain:
		return target == ErrToolApprovalUncertain
	default:
		return false
	}
}

const toolApprovalSnapshotVersion = 1

// toolApprovalSnapshot is stored in the existing workflow snapshot repository.
// The repository is intentionally generic: an agent is already a Workflow, and
// this envelope makes the agent continuation portable across Memory, SQLite,
// Postgres, and compatible application-owned RuntimeStores without new tables.
type toolApprovalSnapshot struct {
	Version       int                   `json:"version"`
	AgentID       AgentID               `json:"agent_id"`
	Request       ToolApprovalRequest   `json:"request"`
	Decision      *ToolApprovalDecision `json:"decision,omitempty"`
	State         string                `json:"state"`
	Transcript    []Message             `json:"transcript"`
	Calls         []ModelToolCall       `json:"calls"`
	CallIndex     int                   `json:"call_index"`
	Revision      int                   `json:"revision,omitempty"`
	Metadata      map[string]string     `json:"metadata,omitempty"`
	ThreadID      ThreadID              `json:"thread_id,omitempty"`
	Scope         RuntimeScope          `json:"scope,omitempty"`
	Annotations   Metadata              `json:"annotations,omitempty"`
	Reasoning     ReasoningConfig       `json:"reasoning,omitempty"`
	Instructions  string                `json:"instructions,omitempty"`
	OutputSchema  *ModelOutputSchema    `json:"output_schema,omitempty"`
	LoadedCount   int                   `json:"loaded_count,omitempty"`
	ModelAttempts []ModelAttempt        `json:"model_attempts,omitempty"`
	EventSequence int                   `json:"event_sequence,omitempty"`
}

const (
	toolApprovalPending   = "pending"
	toolApprovalExecuting = "executing"
)

func (a *Agent) evaluateToolApproval(ctx context.Context, runID RunID, executionID string, step int, stepID StepID, threadID ThreadID, call ModelToolCall, metadata map[string]string) (ToolApprovalResolution, error) {
	if a.toolApprovalPolicy == nil || isNilInterface(a.toolApprovalPolicy) {
		return ToolApprovalResolution{Outcome: ToolApprovalAllow}, nil
	}
	resolution, err := a.toolApprovalPolicy.EvaluateToolApproval(ctx, ToolApprovalInvocation{
		RunID:       runID,
		ExecutionID: executionID,
		Step:        step,
		StepID:      stepID,
		ThreadID:    threadID,
		ToolCallID:  call.ID,
		ToolID:      call.ToolID,
		Arguments:   cloneRawMessage(call.Arguments),
		Metadata:    cloneMetadata(metadata),
	})
	if err != nil {
		return ToolApprovalResolution{}, fmt.Errorf("lebro: evaluate tool approval for %q: %w", call.ToolID, err)
	}
	if resolution.Outcome == "" {
		resolution.Outcome = ToolApprovalAllow
	}
	switch resolution.Outcome {
	case ToolApprovalAllow, ToolApprovalRequire, ToolApprovalDeny:
	default:
		return ToolApprovalResolution{}, fmt.Errorf("lebro: invalid tool approval outcome %q", resolution.Outcome)
	}
	if resolution.TTL < 0 {
		return ToolApprovalResolution{}, errors.New("lebro: tool approval TTL must not be negative")
	}
	return resolution, nil
}

func (a *Agent) newToolApprovalRequest(runID RunID, executionID string, step int, stepID StepID, threadID ThreadID, call ModelToolCall, resolution ToolApprovalResolution) ToolApprovalRequest {
	now := a.clock.Now().UTC()
	action := resolution.Action
	if action == "" {
		action = ActionToolCall
	}
	resource := resolution.Resource
	if resource.Kind == "" {
		resource = Resource{Kind: ResourceKindTool, ID: string(call.ToolID)}
	}
	arguments := canonicalToolApprovalArguments(call.Arguments)
	request := ToolApprovalRequest{
		ID:          fmt.Sprintf("%s:%d:%s:%s", runID, step, stepID, call.ID),
		RunID:       runID,
		ExecutionID: executionID,
		Step:        step,
		StepID:      stepID,
		ThreadID:    threadID,
		ToolCallID:  call.ID,
		ToolID:      call.ToolID,
		Arguments:   arguments,
		Action:      action,
		Resource:    resource,
		Reason:      resolution.Reason,
		Requester:   resolution.Requester,
		Metadata:    cloneMetadata(resolution.Metadata),
		RequestedAt: now,
	}
	if resolution.TTL > 0 {
		request.ExpiresAt = now.Add(resolution.TTL)
	}
	return request
}

func toolApprovalSnapshotID(runID RunID, step int, index int, revision int) string {
	return fmt.Sprintf("%s-tool-approval-%d-%d-%d", runID, step, index, revision)
}

func cloneModelToolCalls(calls []ModelToolCall) []ModelToolCall {
	if len(calls) == 0 {
		return nil
	}
	cloned := make([]ModelToolCall, len(calls))
	for i, call := range calls {
		cloned[i] = cloneToolCallValue(call)
	}
	return cloned
}

func cloneModelAttempts(attempts []ModelAttempt) []ModelAttempt {
	if len(attempts) == 0 {
		return nil
	}
	cloned := make([]ModelAttempt, len(attempts))
	copy(cloned, attempts)
	return cloned
}

func cloneTranscript(messages []Message) []Message {
	return cloneMessages(messages)
}

func (a *Agent) persistToolApproval(ctx context.Context, state toolApprovalSnapshot) error {
	if a.store == nil || isNilInterface(a.store) {
		return errors.New("lebro: durable tool approval requires a store")
	}
	if err := requireCapability(a.storeCaps, StoreCapabilityWorkflowState, "durable tool approval"); err != nil {
		return err
	}
	state.Version = toolApprovalSnapshotVersion
	state.State = toolApprovalPending
	state.Transcript = cloneTranscript(state.Transcript)
	state.Calls = cloneModelToolCalls(state.Calls)
	state.Metadata = cloneMetadata(state.Metadata)
	state.Request = cloneToolApprovalRequest(state.Request)
	state.Decision = cloneToolApprovalDecision(state.Decision)
	state.Annotations = state.Annotations.Clone()
	state.ModelAttempts = cloneModelAttempts(state.ModelAttempts)
	encoded, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("lebro: encode tool approval snapshot: %w", err)
	}
	now := a.clock.Now().UTC()
	record := WorkflowRunRecord{
		ID:            state.Request.RunID,
		WorkflowID:    WorkflowID(a.definition.ID),
		ThreadID:      state.ThreadID,
		Namespace:     state.Scope.Namespace,
		OwnerID:       state.Scope.OwnerID,
		Status:        RunStatusSuspended,
		CurrentStep:   state.Request.Step,
		CurrentStepID: state.Request.StepID,
		Metadata:      marshalMetadata(state.Metadata),
		StartedAt:     state.Request.RequestedAt,
		UpdatedAt:     now,
	}
	return a.store.Transaction(ctx, func(ctx context.Context, repos Repositories) error {
		if existing, err := repos.WorkflowRuns().GetWorkflowRun(ctx, state.Request.RunID); err == nil {
			if existing.WorkflowID != WorkflowID(a.definition.ID) || existing.Status != RunStatusRunning {
				return fmt.Errorf("lebro: tool approval run %q already exists", state.Request.RunID)
			}
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		if err := repos.WorkflowRuns().SaveWorkflowRun(ctx, record); err != nil {
			return err
		}
		sequence, err := nextToolApprovalSnapshotSequence(ctx, repos.WorkflowSnapshots(), state.Request.RunID)
		if err != nil {
			return err
		}
		return repos.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, WorkflowSnapshotRecord{
			ID:            toolApprovalSnapshotID(state.Request.RunID, state.Request.Step, state.CallIndex, state.Revision),
			RunID:         state.Request.RunID,
			Sequence:      sequence,
			SchemaVersion: toolApprovalSnapshotVersion,
			State:         encoded,
			CreatedAt:     now,
		})
	})
}

// nextToolApprovalSnapshotSequence allocates one monotonic sequence for the
// run inside the enclosing repository transaction. It avoids imposing an
// artificial bit-width on agent steps, tool-call batches, or revisions.
func nextToolApprovalSnapshotSequence(ctx context.Context, snapshots WorkflowSnapshotRepository, runID RunID) (int64, error) {
	var (
		cursor string
		latest int64
	)
	for {
		page, err := snapshots.ListWorkflowSnapshots(ctx, runID, PageRequest{Cursor: cursor, Limit: 1000})
		if err != nil {
			return 0, err
		}
		for _, record := range page.Records {
			if record.Sequence > latest {
				latest = record.Sequence
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if latest == int64(^uint64(0)>>1) {
		return 0, errors.New("lebro: tool approval snapshot sequence exhausted")
	}
	return latest + 1, nil
}

func (a *Agent) suspendAgentToolApproval(ctx context.Context, input RunInput, runID RunID, executionID string, step int, stepID StepID, metadata map[string]string, transcript []Message, calls []ModelToolCall, callIndex int, attempts []ModelAttempt, loadedCount int, emitter *runEmitter, resolution ToolApprovalResolution) (ToolApprovalRequest, error) {
	if callIndex < 0 || callIndex >= len(calls) {
		return ToolApprovalRequest{}, errors.New("lebro: tool approval call index is out of range")
	}
	request := a.newToolApprovalRequest(runID, executionID, step, stepID, input.ThreadID, calls[callIndex], resolution)
	state := toolApprovalSnapshot{
		AgentID:       a.definition.ID,
		Request:       request,
		Transcript:    transcript,
		Calls:         calls,
		CallIndex:     callIndex,
		Metadata:      metadata,
		ThreadID:      input.ThreadID,
		Scope:         input.ObservabilityScope,
		Annotations:   input.Annotations,
		Reasoning:     input.Reasoning,
		Instructions:  snapshotInstructions(transcript),
		OutputSchema:  cloneModelOutputSchema(input.OutputSchema),
		LoadedCount:   loadedCount,
		ModelAttempts: attempts,
		EventSequence: pendingToolApprovalSuspendSequence(emitter),
	}
	if err := a.persistToolApproval(ctx, state); err != nil {
		return ToolApprovalRequest{}, err
	}
	emitter.emitSuspended(runID, step, stepID)
	return request, nil
}

func pendingToolApprovalSuspendSequence(emitter *runEmitter) int {
	if emitter != nil && emitter.enabled() {
		return emitter.seq + 1
	}
	if emitter == nil {
		return 0
	}
	return emitter.seq
}

func marshalMetadata(metadata map[string]string) json.RawMessage {
	if len(metadata) == 0 {
		return nil
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return nil
	}
	return encoded
}

func (a *Agent) loadToolApproval(ctx context.Context, runID RunID) (WorkflowRunRecord, toolApprovalSnapshot, error) {
	if a.store == nil || isNilInterface(a.store) {
		return WorkflowRunRecord{}, toolApprovalSnapshot{}, errors.New("lebro: durable tool approval requires a store")
	}
	run, err := a.store.WorkflowRuns().GetWorkflowRun(ctx, runID)
	if err != nil {
		return WorkflowRunRecord{}, toolApprovalSnapshot{}, fmt.Errorf("lebro: load tool approval run %q: %w", runID, err)
	}
	if run.WorkflowID != WorkflowID(a.definition.ID) {
		return WorkflowRunRecord{}, toolApprovalSnapshot{}, fmt.Errorf("lebro: run %q belongs to agent %q, not %q", runID, run.WorkflowID, a.definition.ID)
	}
	var (
		best   WorkflowSnapshotRecord
		state  toolApprovalSnapshot
		cursor string
	)
	for {
		page, err := a.store.WorkflowSnapshots().ListWorkflowSnapshots(ctx, runID, PageRequest{Cursor: cursor, Limit: 1000})
		if err != nil {
			return WorkflowRunRecord{}, toolApprovalSnapshot{}, fmt.Errorf("lebro: list tool approval snapshots for %q: %w", runID, err)
		}
		for _, candidate := range page.Records {
			var decoded toolApprovalSnapshot
			if err := json.Unmarshal(candidate.State, &decoded); err != nil || decoded.Version != toolApprovalSnapshotVersion || decoded.AgentID != a.definition.ID || decoded.Request.ID == "" {
				continue
			}
			if best.ID == "" || candidate.Sequence > best.Sequence {
				best, state = candidate, decoded
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if best.ID == "" {
		return WorkflowRunRecord{}, toolApprovalSnapshot{}, fmt.Errorf("lebro: no tool approval snapshot for run %q", runID)
	}
	return run, state, nil
}

// PendingToolApproval loads the currently suspended request after a process
// restart. It is safe to call repeatedly; it never invokes policy or a tool.
func (a *Agent) PendingToolApproval(ctx context.Context, runID RunID) (ToolApprovalRequest, error) {
	run, state, err := a.loadToolApproval(ctx, runID)
	if err != nil {
		return ToolApprovalRequest{}, err
	}
	if state.State == toolApprovalExecuting {
		return ToolApprovalRequest{}, &ToolApprovalError{Kind: ToolApprovalErrorUncertain, RequestID: state.Request.ID, Err: ErrToolApprovalUncertain}
	}
	if run.Status != RunStatusSuspended {
		return ToolApprovalRequest{}, &ToolApprovalError{Kind: ToolApprovalErrorStale, RequestID: state.Request.ID, Err: ErrToolApprovalStale}
	}
	if state.State != toolApprovalPending {
		return ToolApprovalRequest{}, &ToolApprovalError{Kind: ToolApprovalErrorUncertain, RequestID: state.Request.ID, Err: ErrToolApprovalUncertain}
	}
	return cloneToolApprovalRequest(state.Request), nil
}

func cloneToolApprovalRequest(request ToolApprovalRequest) ToolApprovalRequest {
	request.Arguments = cloneRawMessage(request.Arguments)
	request.Metadata = cloneMetadata(request.Metadata)
	return request
}

func cloneToolApprovalDecision(decision *ToolApprovalDecision) *ToolApprovalDecision {
	if decision == nil {
		return nil
	}
	clone := *decision
	return &clone
}

func toolApprovalExpired(request ToolApprovalRequest, now time.Time, decidedAt time.Time) bool {
	if request.ExpiresAt.IsZero() {
		return false
	}
	return decidedAt.After(request.ExpiresAt) || now.After(request.ExpiresAt)
}

func (a *Agent) transitionToolApproval(ctx context.Context, snapshot toolApprovalSnapshot, decision *ToolApprovalDecision, status RunStatus, nextState string, failure *WorkflowFailureData) error {
	runID, requestID := snapshot.Request.RunID, snapshot.Request.ID
	for attempt := 0; attempt < 2; attempt++ {
		current := snapshot
		err := a.store.Transaction(ctx, func(ctx context.Context, repos Repositories) error {
			run, err := repos.WorkflowRuns().GetWorkflowRun(ctx, runID)
			if err != nil {
				return err
			}
			if run.Status != RunStatusSuspended || current.Request.ID != requestID || current.State != toolApprovalPending {
				return &ToolApprovalError{Kind: ToolApprovalErrorStale, RequestID: requestID, Err: ErrToolApprovalStale}
			}
			current.State = nextState
			current.Revision++
			current.Decision = cloneToolApprovalDecision(decision)
			encoded, err := json.Marshal(current)
			if err != nil {
				return err
			}
			now := a.clock.Now().UTC()
			run.Status = status
			run.UpdatedAt = now
			run.Failure = failure
			if status == RunStatusFailed {
				run.FinishedAt = &now
			}
			sequence, err := nextToolApprovalSnapshotSequence(ctx, repos.WorkflowSnapshots(), runID)
			if err != nil {
				return err
			}
			latest := WorkflowSnapshotRecord{ID: toolApprovalSnapshotID(runID, current.Request.Step, current.CallIndex, current.Revision), RunID: runID, Sequence: sequence, SchemaVersion: toolApprovalSnapshotVersion, State: encoded, CreatedAt: now}
			if err := repos.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, latest); err != nil {
				return err
			}
			return repos.WorkflowRuns().SaveWorkflowRun(ctx, run)
		})
		if !errors.Is(err, ErrConflict) {
			return err
		}
		// A conflict can be an unrelated optimistic write. Re-read before retrying:
		// if another decider advanced this request, report the promised typed stale
		// outcome rather than leaking storage contention to the approval boundary.
		run, latest, loadErr := a.loadToolApproval(ctx, runID)
		if loadErr != nil {
			return err
		}
		if run.Status != RunStatusSuspended || latest.State != toolApprovalPending || latest.Request.ID != requestID {
			return &ToolApprovalError{Kind: ToolApprovalErrorStale, RequestID: requestID, Err: ErrToolApprovalStale}
		}
		snapshot = latest
	}
	return ErrConflict
}

// ResumeToolApproval applies one decision to its exact persisted request. It
// writes an executing marker before the handler starts; if a process dies once
// that marker is durable, later callers receive ErrToolApprovalUncertain rather
// than replaying a potentially non-idempotent side effect.
func (a *Agent) ResumeToolApproval(ctx context.Context, decision ToolApprovalDecision) (RunResult, error) {
	if a == nil {
		return RunResult{}, &AgentError{Kind: AgentErrorProviderFailure, Err: errors.New("lebro: agent is nil")}
	}
	if decision.RunID == "" || decision.RequestID == "" || decision.DecidedAt.IsZero() {
		return RunResult{}, &ToolApprovalError{Kind: ToolApprovalErrorStale, RequestID: decision.RequestID, Err: ErrToolApprovalStale}
	}
	run, state, err := a.loadToolApproval(ctx, decision.RunID)
	if err != nil {
		return RunResult{}, err
	}
	if state.Request.ID != decision.RequestID {
		return RunResult{}, &ToolApprovalError{Kind: ToolApprovalErrorStale, RequestID: decision.RequestID, Err: ErrToolApprovalStale}
	}
	if state.State == toolApprovalExecuting {
		return RunResult{}, &ToolApprovalError{Kind: ToolApprovalErrorUncertain, RequestID: decision.RequestID, Err: ErrToolApprovalUncertain}
	}
	if run.Status != RunStatusSuspended || state.State != toolApprovalPending {
		return RunResult{}, &ToolApprovalError{Kind: ToolApprovalErrorStale, RequestID: decision.RequestID, Err: ErrToolApprovalStale}
	}
	if !toolApprovalMatchesCall(state) {
		return RunResult{}, &ToolApprovalError{Kind: ToolApprovalErrorUncertain, RequestID: decision.RequestID, Err: ErrToolApprovalUncertain}
	}
	if expired := toolApprovalExpired(state.Request, a.clock.Now(), decision.DecidedAt); expired {
		failure := &WorkflowFailureData{Kind: WorkflowErrorStepFailed, Step: state.Request.Step, StepID: state.Request.StepID, Message: ErrToolApprovalExpired.Error()}
		if err := a.transitionToolApproval(ctx, state, &decision, RunStatusFailed, "expired", failure); err != nil {
			return RunResult{}, err
		}
		approvalErr := &ToolApprovalError{Kind: ToolApprovalErrorExpired, RequestID: decision.RequestID, Err: ErrToolApprovalExpired}
		a.emitApprovalTerminal(ctx, state, approvalErr)
		return RunResult{ID: decision.RunID, Status: RunStatusFailed, Messages: cloneTranscript(state.Transcript), Metadata: cloneMetadata(state.Metadata), ModelAttempts: cloneModelAttempts(state.ModelAttempts)}, approvalErr
	}
	if !decision.Approved {
		failure := &WorkflowFailureData{Kind: WorkflowErrorStepFailed, Step: state.Request.Step, StepID: state.Request.StepID, Message: ErrToolApprovalDenied.Error()}
		if err := a.transitionToolApproval(ctx, state, &decision, RunStatusFailed, "denied", failure); err != nil {
			return RunResult{}, err
		}
		approvalErr := &ToolApprovalError{Kind: ToolApprovalErrorDenied, RequestID: decision.RequestID, Err: ErrToolApprovalDenied}
		a.emitApprovalTerminal(ctx, state, approvalErr)
		return RunResult{ID: decision.RunID, Status: RunStatusFailed, Messages: cloneTranscript(state.Transcript), Metadata: cloneMetadata(state.Metadata), ModelAttempts: cloneModelAttempts(state.ModelAttempts)}, approvalErr
	}
	if err := a.transitionToolApproval(ctx, state, &decision, RunStatusRunning, toolApprovalExecuting, nil); err != nil {
		return RunResult{}, err
	}

	return a.resumeApprovedTool(ctx, state)
}

func toolApprovalMatchesCall(state toolApprovalSnapshot) bool {
	if state.CallIndex < 0 || state.CallIndex >= len(state.Calls) {
		return false
	}
	call := state.Calls[state.CallIndex]
	return call.ID == state.Request.ToolCallID && call.ToolID == state.Request.ToolID && rawJSONEqual(call.Arguments, state.Request.Arguments)
}

func (a *Agent) emitApprovalTerminal(ctx context.Context, state toolApprovalSnapshot, approvalErr error) {
	emitter := newRunEmitter(ctx, a.listener, a.clock, a.idSource)
	emitter.seq = state.EventSequence
	journal := newRunJournal(a.clock, a.store, state.Request.RunID, state.ThreadID, "approval-"+state.Request.ID+"-decision", state.Scope, state.Annotations)
	if journal != nil {
		emitter.setListener(fanoutListener{listeners: []RunListener{a.listener, journal}})
	}
	emitter.terminal(state.Request.RunID, state.Request.Step, state.Request.StepID, RunEventFailed, RunStatusFailed, approvalErr)
	journal.flushDiagnostics(context.WithoutCancel(ctx), a.store)
}

func (a *Agent) resumeApprovedTool(ctx context.Context, state toolApprovalSnapshot) (RunResult, error) {
	if state.CallIndex < 0 || state.CallIndex >= len(state.Calls) {
		return RunResult{}, &ToolApprovalError{Kind: ToolApprovalErrorUncertain, RequestID: state.Request.ID, Err: ErrToolApprovalUncertain}
	}
	executionID := "approval-" + state.Request.ID + "-tool"
	emitter := newRunEmitter(ctx, a.listener, a.clock, a.idSource)
	emitter.seq = state.EventSequence
	journal := newRunJournal(a.clock, a.store, state.Request.RunID, state.ThreadID, executionID, state.Scope, state.Annotations)
	if journal != nil {
		emitter.setListener(fanoutListener{listeners: []RunListener{a.listener, journal}})
	}
	emitter.emitResumed(state.Request.RunID, state.Request.Step, state.Request.StepID)
	transcript := cloneTranscript(state.Transcript)
	for i := state.CallIndex; i < len(state.Calls); i++ {
		call := cloneToolCallValue(state.Calls[i])
		if i > state.CallIndex {
			if err := ctx.Err(); err != nil {
				return a.cancelApprovalResume(ctx, emitter, journal, state, transcript, err)
			}
			emitter.emitToolRequested(state.Request.RunID, state.Request.Step, state.Request.StepID, call.ID, call.ToolID)
			resolution, err := a.evaluateToolApproval(ctx, state.Request.RunID, executionID, state.Request.Step, state.Request.StepID, state.ThreadID, call, state.Metadata)
			if err != nil {
				return a.finishApprovalResume(ctx, emitter, journal, state, transcript, &AgentError{Kind: AgentErrorToolFailure, Step: state.Request.Step, Err: err})
			}
			if resolution.Outcome == ToolApprovalDeny {
				return a.finishApprovalResume(ctx, emitter, journal, state, transcript, &ToolApprovalError{Kind: ToolApprovalErrorDenied, RequestID: state.Request.ID, Err: ErrToolApprovalDenied})
			}
			if resolution.Outcome == ToolApprovalRequire {
				request := a.newToolApprovalRequest(state.Request.RunID, executionID, state.Request.Step, state.Request.StepID, state.ThreadID, call, resolution)
				next := state
				next.Request, next.Decision, next.Calls, next.CallIndex, next.Transcript, next.EventSequence = request, nil, cloneModelToolCalls(state.Calls), i, transcript, pendingToolApprovalSuspendSequence(emitter)
				if err := a.persistToolApproval(ctx, next); err != nil {
					return RunResult{}, err
				}
				emitter.emitSuspended(state.Request.RunID, state.Request.Step, state.Request.StepID)
				journal.flushDiagnostics(context.WithoutCancel(ctx), a.store)
				return RunResult{ID: state.Request.RunID, Status: RunStatusSuspended, Messages: cloneTranscript(transcript), Metadata: cloneMetadata(state.Metadata), ModelAttempts: cloneModelAttempts(state.ModelAttempts), ToolApproval: &request}, nil
			}
		}
		toolStart := emitter.emitToolStarted(state.Request.RunID, state.Request.Step, state.Request.StepID, call.ID, call.ToolID)
		journal.toolStarted(state.Request.Step, state.Request.StepID, call)
		observationStart := a.clock.Now()
		result := a.executeToolCall(ctx, state.Request.RunID, state.Request.Step, state.Request.StepID, state.ThreadID, call, state.Metadata)
		observationEnd := a.clock.Now()
		emitter.emitToolFinished(state.Request.RunID, state.Request.Step, state.Request.StepID, toolStart, call.ID, call.ToolID, result.State, result.Err)
		journal.toolFinished(result)
		a.deliverObservation(state.Request.RunID, executionID, state.Request.Step, state.Request.StepID, state.ThreadID, call, result, observationStart, observationEnd)
		transcript = append(transcript, toolResultMessage(call.ID, result))
		if result.State == ToolExecutionCancelled {
			return a.cancelApprovalResume(ctx, emitter, journal, state, transcript, result.Err)
		}
		if result.State != ToolExecutionSucceeded {
			return a.finishApprovalResume(ctx, emitter, journal, state, transcript, toolExecutionAgentError(state.Request.Step, result))
		}
	}
	journal.flushDiagnostics(context.WithoutCancel(ctx), a.store)
	continuationAgent := *a
	continuationAgent.instructionsResolver = nil
	continuationAgent.definition.Instructions = state.Instructions
	continuation := continuationAgent.resumeInput(state, transcript, emitter.seq)
	result, err := continuationAgent.Run(ctx, continuation)
	if result.Status != RunStatusSuspended {
		a.persistApprovalRunTerminal(state, result, err)
	}
	return result, err
}

func (a *Agent) finishApprovalResume(ctx context.Context, emitter *runEmitter, journal *runJournal, state toolApprovalSnapshot, transcript []Message, err error) (RunResult, error) {
	emitter.terminal(state.Request.RunID, state.Request.Step, state.Request.StepID, RunEventFailed, RunStatusFailed, err)
	journal.flushDiagnostics(context.WithoutCancel(ctx), a.store)
	result := RunResult{ID: state.Request.RunID, Status: RunStatusFailed, Messages: cloneTranscript(transcript), Metadata: cloneMetadata(state.Metadata), ModelAttempts: cloneModelAttempts(state.ModelAttempts)}
	a.persistApprovalRunTerminal(state, result, err)
	return result, err
}

func (a *Agent) cancelApprovalResume(ctx context.Context, emitter *runEmitter, journal *runJournal, state toolApprovalSnapshot, transcript []Message, cause error) (RunResult, error) {
	emitter.terminal(state.Request.RunID, state.Request.Step, state.Request.StepID, RunEventCancelled, RunStatusCancelled, cause)
	journal.flushDiagnostics(context.WithoutCancel(ctx), a.store)
	result, err := a.cancelledWithAttempts(state.Request.RunID, transcript, cloneMetadata(state.Metadata), state.Request.Step, cause, cloneModelAttempts(state.ModelAttempts))
	a.persistApprovalRunTerminal(state, result, err)
	return result, err
}

func (a *Agent) resumeInput(state toolApprovalSnapshot, transcript []Message, eventSequence int) RunInput {
	start := 0
	if len(transcript) > 0 && transcript[0].Role == RoleSystem {
		start = 1
	}
	start += state.LoadedCount
	if start > len(transcript) {
		start = len(transcript)
	}
	return RunInput{
		Messages:             cloneTranscript(transcript[start:]),
		ThreadID:             state.ThreadID,
		RunID:                state.Request.RunID,
		ExecutionID:          "approval-" + state.Request.ID + "-loop",
		Metadata:             cloneMetadata(state.Metadata),
		Annotations:          state.Annotations.Clone(),
		ObservabilityScope:   state.Scope,
		Reasoning:            state.Reasoning,
		OutputSchema:         cloneModelOutputSchema(state.OutputSchema),
		resumeStartStep:      state.Request.Step + 1,
		resumeEventSequence:  eventSequence,
		resumePriorAttempts:  cloneModelAttempts(state.ModelAttempts),
		resumeSkipInputPhase: true,
	}
}

func snapshotInstructions(transcript []Message) string {
	if len(transcript) > 0 && transcript[0].Role == RoleSystem {
		return transcript[0].Content
	}
	return ""
}

func (a *Agent) persistApprovalRunTerminal(state toolApprovalSnapshot, result RunResult, runErr error) {
	if a.store == nil || isNilInterface(a.store) {
		return
	}
	now := a.clock.Now().UTC()
	failure := (*WorkflowFailureData)(nil)
	switch {
	case result.Status == RunStatusCancelled:
		failure = &WorkflowFailureData{Kind: WorkflowErrorCancelled, Step: state.Request.Step, StepID: state.Request.StepID, Message: "agent tool approval continuation cancelled"}
	case runErr != nil:
		failure = &WorkflowFailureData{Kind: WorkflowErrorStepFailed, Step: state.Request.Step, StepID: state.Request.StepID, Message: "agent tool approval continuation failed"}
	}
	_ = a.store.WorkflowRuns().SaveWorkflowRun(context.Background(), WorkflowRunRecord{
		ID:            state.Request.RunID,
		WorkflowID:    WorkflowID(a.definition.ID),
		ThreadID:      state.ThreadID,
		Namespace:     state.Scope.Namespace,
		OwnerID:       state.Scope.OwnerID,
		Status:        result.Status,
		CurrentStep:   state.Request.Step,
		CurrentStepID: state.Request.StepID,
		Metadata:      marshalMetadata(state.Metadata),
		StartedAt:     state.Request.RequestedAt,
		FinishedAt:    &now,
		UpdatedAt:     now,
		Failure:       failure,
	})
}

// canonicalToolApprovalArguments compares the immutable persisted request
// bytes to the original model call. The helper is used in tests and protects
// future call sites from accidentally re-marshalling unreviewed input.
func canonicalToolApprovalArguments(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil
	}
	return json.RawMessage(compact.Bytes())
}
