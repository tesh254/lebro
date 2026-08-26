package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// runJournal captures one agent run's observability records — model attempts,
// tool executions, and ordered run events — so they can be persisted durably
// alongside the transcript. It implements RunListener and is wired in
// addition to any caller-supplied listener; when no Store is configured the
// journal is never created and behavior is unchanged.
//
// Redaction defaults: stream deltas are never recorded (their text,
// reasoning, and structured-output payloads duplicate transcript content and
// may carry provider replay data), tool arguments and results are never
// recorded, and errors are stored as a normalized kind plus their safe
// message. Stored event sequences keep their original numbering, so omitted
// delta events leave gaps but order never regresses.
type runJournal struct {
	mu       sync.Mutex
	clock    Clock
	runID    RunID
	threadID ThreadID
	scope    ObservabilityScope
	base     Metadata

	events   []RunEventRecord
	attempts []ModelAttemptRecord
	tools    []ToolExecutionRecord

	// open holds in-flight provider attempts in start order. Attempts within
	// one model call are sequential (routing walks its fallback chain one
	// provider at a time), so completion pops from the front.
	open    []openModelAttempt
	toolSeq int
	// persisted counts how many entries the success-path transaction already
	// committed, so the deferred diagnostic flush writes only the delta —
	// typically the terminal event emitted after persist.
	persistedEvents   int
	persistedAttempts int
	persistedTools    int
}

type openModelAttempt struct {
	provider ProviderID
	model    string
	start    time.Time
}

// newRunJournal returns nil unless durable persistence is configured; every
// call site treats a nil journal as "capture disabled".
func newRunJournal(clock Clock, store Store, runID RunID, threadID ThreadID, scope ObservabilityScope, annotations Metadata) *runJournal {
	if store == nil || isNilInterface(store) {
		return nil
	}
	return &runJournal{clock: clock, runID: runID, threadID: threadID, scope: scope, base: annotations.Clone()}
}

// OnRunEvent converts an emitted run event into a durable record. Delta
// events are skipped entirely.
func (j *runJournal) OnRunEvent(event RunEvent) {
	if j == nil || event.Type == RunEventDelta {
		return
	}
	record := RunEventRecord{
		ID:              fmt.Sprintf("%s-event-%d", event.RunID, event.Sequence),
		RunID:           event.RunID,
		ThreadID:        j.threadID,
		Namespace:       j.scope.Namespace,
		OwnerID:         j.scope.OwnerID,
		Sequence:        int64(event.Sequence),
		Type:            event.Type,
		Timestamp:       event.Timestamp,
		StepID:          event.StepID,
		Step:            event.Step,
		ParentRunID:     event.ParentRunID,
		ParentStepID:    event.ParentStepID,
		Branch:          event.Branch,
		ToolCallID:      event.ToolCallID,
		ToolID:          event.ToolID,
		Provider:        event.Provider,
		ProviderModel:   event.ProviderModel,
		AttemptStatus:   event.AttemptStatus,
		ProcessorPhase:  event.ProcessorPhase,
		ProcessorAction: event.ProcessorAction,
		Status:          event.Status,
		FinishReason:    event.FinishReason,
		Usage:           event.Usage,
		DurationNanos:   int64(event.Duration),
	}
	record.ErrorKind, record.ErrorMessage = classifyRunError(event.Error)
	if payload := runEventPayload(event); len(payload) > 0 {
		record.Payload = payload
	}
	j.mu.Lock()
	j.events = append(j.events, record)
	j.mu.Unlock()
}

// runEventPayload builds the allowlisted per-type payload. Content-bearing
// fields (delta text, reasoning, structured output) are deliberately absent.
func runEventPayload(event RunEvent) json.RawMessage {
	switch event.Type {
	case RunEventStepAttemptStarted, RunEventStepAttemptFinished:
		raw, err := json.Marshal(map[string]any{"attempt": event.Attempt, "delay_ns": int64(event.Delay)})
		if err != nil {
			return nil
		}
		return raw
	case RunEventLoopIterationStarted, RunEventLoopIterationFinished:
		raw, err := json.Marshal(map[string]any{"iteration": event.Iteration})
		if err != nil {
			return nil
		}
		return raw
	case RunEventRouteSelected:
		// The emitter encodes the candidate list as JSON in DeltaText.
		if event.DeltaText == "" || !json.Valid([]byte(event.DeltaText)) {
			return nil
		}
		return json.RawMessage(event.DeltaText)
	default:
		return nil
	}
}

// classifyRunError maps an event error to a normalized kind and a deliberately
// content-free diagnostic. Provider and tool errors often echo prompts,
// arguments, or credentials; callers that need more detail can correlate the
// typed kind with their own protected logs.
func classifyRunError(err error) (kind, message string) {
	if err == nil {
		return "", ""
	}
	var agentErr *AgentError
	if errors.As(err, &agentErr) && agentErr != nil {
		kind := agentErr.Kind
		if kind == "" {
			kind = AgentErrorProviderFailure
		}
		return string(kind), "redacted"
	}
	var modelErr *ModelError
	if errors.As(err, &modelErr) && modelErr != nil {
		return string(modelErr.Kind), "redacted"
	}
	var toolErr *ToolExecutionError
	if errors.As(err, &toolErr) && toolErr != nil {
		return string(toolErr.State), "redacted"
	}
	if errors.Is(err, context.Canceled) {
		return string(AgentErrorCancelled), context.Canceled.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded", context.DeadlineExceeded.Error()
	}
	return "error", "redacted"
}

// beginModelAttempt records the start of one provider attempt. Provider is
// empty for direct model calls that bypass the router.
func (j *runJournal) beginModelAttempt(provider ProviderID, model string) {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.open = append(j.open, openModelAttempt{provider: provider, model: model, start: j.clock.Now()})
}

// completeModelAttempt closes the oldest open attempt with its observed
// outcome. It is called by the router attempt observer as each provider
// finishes.
func (j *runJournal) completeModelAttempt(attempt ModelAttempt) {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	var slot openModelAttempt
	if len(j.open) > 0 {
		slot, j.open = j.open[0], j.open[1:]
	} else {
		slot.start = j.clock.Now()
	}
	record := ModelAttemptRecord{
		ID:         fmt.Sprintf("%s-attempt-%d", j.runID, len(j.attempts)+1),
		RunID:      j.runID,
		ThreadID:   j.threadID,
		Namespace:  j.scope.Namespace,
		OwnerID:    j.scope.OwnerID,
		Index:      len(j.attempts) + 1,
		Provider:   attempt.Provider,
		Model:      attempt.Model,
		Status:     attempt.Status,
		StartedAt:  slot.start,
		FinishedAt: j.clock.Now(),
	}
	if attempt.Error != nil {
		record.ErrorKind, record.ErrorMessage = classifyRunError(attempt.Error)
	}
	j.attempts = append(j.attempts, record)
}

// finishModelCall closes out one model call. Any attempt still open failed or
// was cancelled without its observer completing (direct model calls land
// here); on success the final attempt is the routed winner and receives the
// response usage and finish reason.
func (j *runJournal) finishModelCall(usage ModelUsage, finishReason FinishReason, err error) {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	status := ModelAttemptFailed
	switch {
	case err == nil:
		// A successful call closes its own dangling slot (the direct-model
		// path has no observer).
		status = ModelAttemptSuccess
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		status = ModelAttemptCancelled
	}
	end := j.clock.Now()
	for _, slot := range j.open {
		record := ModelAttemptRecord{
			ID:         fmt.Sprintf("%s-attempt-%d", j.runID, len(j.attempts)+1),
			RunID:      j.runID,
			ThreadID:   j.threadID,
			Namespace:  j.scope.Namespace,
			OwnerID:    j.scope.OwnerID,
			Index:      len(j.attempts) + 1,
			Provider:   slot.provider,
			Model:      slot.model,
			Status:     status,
			StartedAt:  slot.start,
			FinishedAt: end,
		}
		record.ErrorKind, record.ErrorMessage = classifyRunError(err)
		j.attempts = append(j.attempts, record)
	}
	j.open = nil
	if err == nil && len(j.attempts) > 0 {
		winner := &j.attempts[len(j.attempts)-1]
		winner.Usage = usage
		winner.FinishReason = finishReason
	}
}

// linkProducedMessages attaches produced transcript message IDs to the
// winning attempt of a successful run.
func (j *runJournal) linkProducedMessages(messageIDs []string) {
	if j == nil || len(messageIDs) == 0 {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for i := len(j.attempts) - 1; i >= 0; i-- {
		switch j.attempts[i].Status {
		case ModelAttemptSuccess, ModelAttemptFallback:
			j.attempts[i].ProducedMessageIDs = append([]string(nil), messageIDs...)
			return
		default:
		}
	}
}

// toolStarted records the beginning of one tool invocation.
func (j *runJournal) toolStarted(step int, stepID StepID, call ModelToolCall) {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.toolSeq++
	j.tools = append(j.tools, ToolExecutionRecord{
		ID:         fmt.Sprintf("%s-tool-%d", j.runID, j.toolSeq),
		RunID:      j.runID,
		ThreadID:   j.threadID,
		Namespace:  j.scope.Namespace,
		OwnerID:    j.scope.OwnerID,
		StepID:     stepID,
		Step:       step,
		ToolCallID: call.ID,
		ToolID:     call.ToolID,
		State:      ToolExecutionSucceeded,
		StartedAt:  j.clock.Now(),
	})
}

// toolFinished stamps the outcome onto the most recent unfinished tool
// record. Tool invocations execute sequentially within a step, so the newest
// open record is always the one being completed.
func (j *runJournal) toolFinished(result ToolExecutionResult) {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for i := len(j.tools) - 1; i >= 0; i-- {
		if !j.tools[i].FinishedAt.IsZero() {
			continue
		}
		j.tools[i].State = result.State
		j.tools[i].FinishedAt = j.clock.Now()
		j.tools[i].ErrorKind, j.tools[i].ErrorMessage = classifyRunError(result.Err)
		return
	}
}

// snapshot merges the run-level annotations into every record and returns
// caller-owned copies.
func (j *runJournal) snapshot() ([]RunEventRecord, []ModelAttemptRecord, []ToolExecutionRecord) {
	if j == nil {
		return nil, nil, nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.snapshotLocked()
}

// snapshotLocked builds merged copies of every captured record. Callers must
// hold j.mu.
func (j *runJournal) snapshotLocked() ([]RunEventRecord, []ModelAttemptRecord, []ToolExecutionRecord) {
	events := make([]RunEventRecord, len(j.events))
	for i, event := range j.events {
		event.Metadata = mergeMetadata(j.base, event.Metadata)
		events[i] = event
	}
	attempts := make([]ModelAttemptRecord, len(j.attempts))
	for i, attempt := range j.attempts {
		attempt.Metadata = mergeMetadata(j.base, attempt.Metadata)
		attempts[i] = attempt
	}
	tools := make([]ToolExecutionRecord, len(j.tools))
	for i, execution := range j.tools {
		execution.Metadata = mergeMetadata(j.base, execution.Metadata)
		tools[i] = execution
	}
	return events, attempts, tools
}

// markPersisted records how many records the success-path transaction
// committed so the deferred diagnostic flush writes only the remainder.
func (j *runJournal) markPersisted(events, attempts, tools int) {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.persistedEvents, j.persistedAttempts, j.persistedTools = events, attempts, tools
}

// flushDiagnostics persists records for runs that did not complete the
// success path — failures, cancellations, and panics — plus anything captured
// after the success-path persist (the terminal event). Best-effort by design:
// a diagnostics write failure must never mask the run's own error, so errors
// are swallowed. The caller passes a detached context so cancelled runs still
// retain records.
func (j *runJournal) flushDiagnostics(ctx context.Context, store Store) {
	if j == nil || store == nil || isNilInterface(store) {
		return
	}
	events, attempts, tools := j.snapshot()
	j.mu.Lock()
	events = events[j.persistedEvents:]
	attempts = attempts[j.persistedAttempts:]
	tools = tools[j.persistedTools:]
	j.mu.Unlock()
	if len(events) == 0 && len(attempts) == 0 && len(tools) == 0 {
		return
	}
	writeObservabilityRecords(ctx, store, events, attempts, tools)
}

// writeObservabilityRecords writes the record set through the store's
// transaction boundary. Stores that do not opt into observability repositories
// are skipped silently; opting in is exactly what makes these records durable.
func writeObservabilityRecords(ctx context.Context, store Store, events []RunEventRecord, attempts []ModelAttemptRecord, tools []ToolExecutionRecord) {
	if _, ok := store.(ObservabilityRepositories); !ok {
		return
	}
	_ = store.Transaction(ctx, func(ctx context.Context, repos Repositories) error {
		return writeObservability(ctx, repos, events, attempts, tools)
	})
}

// writeObservability appends the record set to transaction-scoped
// repositories. It is called inside the caller's transaction so transcript
// messages and observability records commit atomically when the store opts in.
func writeObservability(ctx context.Context, repos Repositories, events []RunEventRecord, attempts []ModelAttemptRecord, tools []ToolExecutionRecord) error {
	obs, ok := repos.(ObservabilityRepositories)
	if !ok {
		return nil
	}
	if len(events) > 0 {
		if err := obs.RunEvents().AppendRunEvents(ctx, events); err != nil {
			return fmt.Errorf("lebro: append run events: %w", err)
		}
	}
	if len(attempts) > 0 {
		if err := obs.ModelAttempts().SaveModelAttempts(ctx, attempts); err != nil {
			return fmt.Errorf("lebro: save model attempts: %w", err)
		}
	}
	if len(tools) > 0 {
		if err := obs.ToolExecutions().SaveToolExecutions(ctx, tools); err != nil {
			return fmt.Errorf("lebro: save tool executions: %w", err)
		}
	}
	return nil
}

// mergeMetadata overlays overlay on top of base; overlay keys win.
func mergeMetadata(base, overlay Metadata) Metadata {
	if len(base) == 0 {
		return overlay.Clone()
	}
	if len(overlay) == 0 {
		return base.Clone()
	}
	merged := make(Metadata, len(base)+len(overlay))
	for key, value := range base {
		merged[key] = append(json.RawMessage(nil), value...)
	}
	for key, value := range overlay {
		merged[key] = append(json.RawMessage(nil), value...)
	}
	return merged
}
