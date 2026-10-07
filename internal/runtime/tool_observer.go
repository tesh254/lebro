package runtime

import (
	"encoding/json"
	"time"
)

// ToolExecutionObservation reports one tool invocation's actual input and
// outcome at the execution boundary, for callers that opt in through
// AgentConfig.ToolObserver. Lifecycle events and durable diagnostics never
// carry these payloads; the observer is the only content-bearing channel, and
// its delivery is entirely the caller's responsibility.
//
// Arguments and Result are immutable snapshots cloned before delivery:
// mutating them does not affect the run, transcript, or any other consumer.
// Result is populated only when State is ToolExecutionSucceeded; otherwise Err
// carries the normalized tool failure.
type ToolExecutionObservation struct {
	RunID       RunID
	ExecutionID string
	StepID      StepID
	Step        int
	ThreadID    ThreadID
	ToolCallID  string
	ToolID      ToolID
	Arguments   json.RawMessage
	Result      json.RawMessage
	State       ToolExecutionState
	Err         error
	StartedAt   time.Time
	FinishedAt  time.Time
}

// ToolResultObserver receives ToolExecutionObservation values for a run's
// tool invocations. Delivery is synchronous, after the tool finishes and
// before the run continues, so observations arrive in execution order exactly
// once per invocation — including when a later model step fails or the run is
// cancelled by someone else.
//
// Observers cannot affect the run: a returned error or panic is contained,
// the tool is never executed a second time because of a failed delivery, and
// the transcript is untouched. A blocking observer applies backpressure to
// the run. Observers must not reenter the observer's own agent from the
// callback; no reentrancy guarantee is made.
type ToolResultObserver interface {
	ObserveToolExecution(ToolExecutionObservation)
}

// ToolResultObserverFunc adapts a function to ToolResultObserver.
type ToolResultObserverFunc func(ToolExecutionObservation)

// ObserveToolExecution implements ToolResultObserver.
func (fn ToolResultObserverFunc) ObserveToolExecution(observation ToolExecutionObservation) {
	fn(observation)
}

// deliverObservation invokes the configured observer once with a defensive
// snapshot. Delivery is best-effort: observer errors are ignored (the
// interface cannot accept one, so there is nothing to propagate), panics are
// recovered so an observer bug cannot take the run down, and a nil observer
// or nil agent is a no-op. ctx cancellation is not checked: the observation
// happened, so reporting it is still correct on a cancelled run.
func (a *Agent) deliverObservation(runID RunID, executionID string, step int, stepID StepID, threadID ThreadID, call ModelToolCall, result ToolExecutionResult, started, finished time.Time) {
	if a == nil || a.toolObserver == nil {
		return
	}
	observation := ToolExecutionObservation{
		RunID:       runID,
		ExecutionID: executionID,
		Step:        step,
		StepID:      stepID,
		ThreadID:    threadID,
		ToolCallID:  call.ID,
		ToolID:      call.ToolID,
		Arguments:   cloneRawMessage(call.Arguments),
		State:       result.State,
		Err:         result.Err,
		StartedAt:   started,
		FinishedAt:  finished,
	}
	if result.State == ToolExecutionSucceeded {
		observation.Result = cloneRawMessage(result.Output)
	}
	func() {
		defer func() {
			_ = recover()
		}()
		a.toolObserver.ObserveToolExecution(observation)
	}()
}
