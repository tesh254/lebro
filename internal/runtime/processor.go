package runtime

import (
	"context"
	"errors"
	"fmt"
)

// ProcessorPhase identifies a point at which a Processor can inspect or alter
// an agent run.
type ProcessorPhase string

const (
	ProcessorPhaseInput         ProcessorPhase = "input"
	ProcessorPhaseModelRequest  ProcessorPhase = "model_request"
	ProcessorPhaseModelResponse ProcessorPhase = "model_response"
	ProcessorPhaseStreamDelta   ProcessorPhase = "stream_delta"
	ProcessorPhaseOutput        ProcessorPhase = "output"
)

// ProcessorAction controls how a processor pipeline continues.
type ProcessorAction string

const (
	ProcessorContinue  ProcessorAction = "continue"
	ProcessorTransform ProcessorAction = "transform"
	ProcessorBlock     ProcessorAction = "block"
)

// ProcessorContext is an immutable snapshot of the current run state. Values
// containing maps, slices, or raw JSON are copied before a Processor receives
// them, so processors cannot mutate agent-owned state through an alias.
type ProcessorContext struct {
	Phase    ProcessorPhase
	RunID    RunID
	Step     int
	ThreadID ThreadID
	Identity Identity
	Metadata map[string]string
	Usage    ModelUsage
	Input    RunInput
	Request  ModelRequest
	Response ModelResponse
	Delta    StreamDelta
	Result   RunResult
}

// ProcessorDecision is a processor's structured outcome. Continue retains the
// current value. Transform replaces the value for the active phase with its
// matching field. Block stops the run without exposing any payload in events.
type ProcessorDecision struct {
	Action   ProcessorAction
	Reason   string
	Input    *RunInput
	Request  *ModelRequest
	Response *ModelResponse
	Delta    *StreamDelta
	Result   *RunResult
}

// Processor observes one or more phases of an agent run. Implementations must
// honor context cancellation and be safe to reuse by concurrent agent runs.
type Processor interface {
	Process(context.Context, ProcessorContext) (ProcessorDecision, error)
}

// ProcessorFunc adapts a function into a Processor.
type ProcessorFunc func(context.Context, ProcessorContext) (ProcessorDecision, error)

func (f ProcessorFunc) Process(ctx context.Context, input ProcessorContext) (ProcessorDecision, error) {
	return f(ctx, input)
}

// ErrProcessorBlocked matches a processor decision that intentionally stopped
// a run.
var ErrProcessorBlocked = errors.New("lebro: processor blocked run")

// ProcessorError preserves processor failures and blocks as typed errors.
type ProcessorError struct {
	Phase  ProcessorPhase
	Step   int
	Reason string
	Err    error
}

func (e *ProcessorError) Error() string {
	if e == nil {
		return "lebro: processor failure"
	}
	return fmt.Sprintf("lebro: processor %s at %s", e.Phase, e.action())
}

func (e *ProcessorError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *ProcessorError) Is(target error) bool {
	return target == ErrProcessorBlocked && e != nil && errors.Is(e.Err, ErrProcessorBlocked)
}

func (e *ProcessorError) action() string {
	if errors.Is(e.Err, ErrProcessorBlocked) {
		return "block"
	}
	return "failure"
}

func cloneProcessorContext(value ProcessorContext) ProcessorContext {
	value.Metadata = cloneMetadata(value.Metadata)
	value.Input = cloneRunInput(value.Input)
	value.Request = cloneModelRequest(value.Request)
	value.Response = cloneModelResponse(value.Response)
	value.Delta = cloneStreamDelta(value.Delta)
	value.Result = cloneRunResult(value.Result)
	return value
}

func cloneRunInput(value RunInput) RunInput {
	value.Messages = cloneMessages(value.Messages)
	value.Metadata = cloneMetadata(value.Metadata)
	value.OutputSchema = cloneModelOutputSchema(value.OutputSchema)
	return value
}

func cloneModelRequest(value ModelRequest) ModelRequest {
	value.Messages = cloneMessages(value.Messages)
	value.Tools = cloneToolDefinitions(value.Tools)
	value.OutputSchema = cloneModelOutputSchema(value.OutputSchema)
	value.Extension = cloneRawMessage(value.Extension)
	return value
}

func cloneModelResponse(value ModelResponse) ModelResponse {
	value.Message = cloneMessage(value.Message)
	value.Extension = cloneRawMessage(value.Extension)
	return value
}

func cloneStreamDelta(value StreamDelta) StreamDelta {
	if value.ToolCall != nil {
		call := cloneToolCallValue(*value.ToolCall)
		value.ToolCall = &call
	}
	return value
}

func cloneRunResult(value RunResult) RunResult {
	value.Messages = cloneMessages(value.Messages)
	value.Metadata = cloneMetadata(value.Metadata)
	value.ModelAttempts = append([]ModelAttempt(nil), value.ModelAttempts...)
	return value
}

func (a *Agent) process(ctx context.Context, emitter *runEmitter, runID RunID, step int, stepID StepID, value ProcessorContext) (ProcessorDecision, error) {
	if len(a.processors) == 0 {
		return contextDecision(value), nil
	}
	identity, _ := IdentityFromContext(ctx)
	value.RunID, value.Step, value.Identity = runID, step, identity
	for _, processor := range a.processors {
		decision, err := processor.Process(ctx, cloneProcessorContext(value))
		if err != nil {
			emitter.emitProcessor(runID, step, stepID, value.Phase, ProcessorBlock)
			return ProcessorDecision{}, &ProcessorError{Phase: value.Phase, Step: step, Err: err}
		}
		if decision.Action == "" {
			decision.Action = ProcessorContinue
		}
		switch decision.Action {
		case ProcessorContinue:
			emitter.emitProcessor(runID, step, stepID, value.Phase, decision.Action)
		case ProcessorBlock:
			emitter.emitProcessor(runID, step, stepID, value.Phase, decision.Action)
			return ProcessorDecision{}, &ProcessorError{Phase: value.Phase, Step: step, Reason: decision.Reason, Err: ErrProcessorBlocked}
		case ProcessorTransform:
			if err := applyProcessorTransform(&value, decision); err != nil {
				emitter.emitProcessor(runID, step, stepID, value.Phase, ProcessorBlock)
				return ProcessorDecision{}, &ProcessorError{Phase: value.Phase, Step: step, Err: err}
			}
			emitter.emitProcessor(runID, step, stepID, value.Phase, decision.Action)
		default:
			emitter.emitProcessor(runID, step, stepID, value.Phase, ProcessorBlock)
			return ProcessorDecision{}, &ProcessorError{Phase: value.Phase, Step: step, Err: fmt.Errorf("lebro: invalid processor action %q", decision.Action)}
		}
	}
	return contextDecision(value), nil
}

func applyProcessorTransform(value *ProcessorContext, decision ProcessorDecision) error {
	switch value.Phase {
	case ProcessorPhaseInput:
		if decision.Input == nil {
			return errors.New("lebro: input processor transform is missing input")
		}
		value.Input = cloneRunInput(*decision.Input)
	case ProcessorPhaseModelRequest:
		if decision.Request == nil {
			return errors.New("lebro: model request processor transform is missing request")
		}
		value.Request = cloneModelRequest(*decision.Request)
	case ProcessorPhaseModelResponse:
		if decision.Response == nil {
			return errors.New("lebro: model response processor transform is missing response")
		}
		value.Response = cloneModelResponse(*decision.Response)
	case ProcessorPhaseStreamDelta:
		if decision.Delta == nil {
			return errors.New("lebro: stream delta processor transform is missing delta")
		}
		value.Delta = cloneStreamDelta(*decision.Delta)
	case ProcessorPhaseOutput:
		if decision.Result == nil {
			return errors.New("lebro: output processor transform is missing result")
		}
		value.Result = cloneRunResult(*decision.Result)
	default:
		return fmt.Errorf("lebro: invalid processor phase %q", value.Phase)
	}
	return nil
}

func contextDecision(value ProcessorContext) ProcessorDecision {
	switch value.Phase {
	case ProcessorPhaseInput:
		return ProcessorDecision{Action: ProcessorTransform, Input: ptr(cloneRunInput(value.Input))}
	case ProcessorPhaseModelRequest:
		return ProcessorDecision{Action: ProcessorTransform, Request: ptr(cloneModelRequest(value.Request))}
	case ProcessorPhaseModelResponse:
		return ProcessorDecision{Action: ProcessorTransform, Response: ptr(cloneModelResponse(value.Response))}
	case ProcessorPhaseStreamDelta:
		return ProcessorDecision{Action: ProcessorTransform, Delta: ptr(cloneStreamDelta(value.Delta))}
	case ProcessorPhaseOutput:
		return ProcessorDecision{Action: ProcessorTransform, Result: ptr(cloneRunResult(value.Result))}
	default:
		return ProcessorDecision{}
	}
}

func ptr[T any](value T) *T { return &value }

func processorAgentError(step int, err error) *AgentError {
	return &AgentError{Kind: AgentErrorProcessor, Step: step, Err: err}
}
