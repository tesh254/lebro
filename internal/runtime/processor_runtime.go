package runtime

import (
	"context"
	"errors"
)

// ProcessorContext is internal execution state used to dispatch the stable
// phase-specific processor contracts.
type ProcessorContext struct {
	Phase    ProcessorPhase
	ThreadID ThreadID
	Metadata map[string]string
	Usage    ModelUsage
	Input    RunInput
	Request  ModelRequest
	Response ModelResponse
	Delta    StreamDelta
	Result   RunResult
	Memory   *MemoryProcessorConfig
}

type processorResult struct {
	Input    *RunInput
	Request  *ModelRequest
	Response *ModelResponse
	Delta    *StreamDelta
	Result   *RunResult
}

var ErrProcessorBlocked = errors.New("lebro: processor blocked run")

func (a *Agent) process(ctx context.Context, emitter *runEmitter, runID RunID, step int, stepID StepID, value ProcessorContext) (processorResult, error) {
	result := processorResult{Input: &value.Input, Request: &value.Request, Response: &value.Response, Delta: &value.Delta, Result: &value.Result}
	run := ProcessorRun{ID: runID, Agent: a.definition, ThreadID: value.ThreadID, Metadata: cloneMetadata(value.Metadata), Memory: value.Memory.Clone()}
	for _, processor := range a.processors.Processors() {
		decision, next, invoked, err := a.processOne(ctx, processor, run, step, value)
		if !invoked {
			continue
		}
		if err != nil {
			emitter.emitProcessor(runID, step, stepID, value.Phase, ProcessorBlock)
			return processorResult{}, NormalizeProcessorError(value.Phase, processor.Name(), err)
		}
		decision, err = NormalizeProcessorDecision(decision)
		if err != nil {
			emitter.emitProcessor(runID, step, stepID, value.Phase, ProcessorBlock)
			return processorResult{}, NormalizeProcessorError(value.Phase, processor.Name(), err)
		}
		emitter.emitProcessor(runID, step, stepID, value.Phase, decision.Kind)
		if decision.Kind == ProcessorBlock {
			return processorResult{}, &ProcessorError{Kind: ProcessorErrorFailed, Phase: value.Phase, Processor: processor.Name(), Err: ErrProcessorBlocked}
		}
		if decision.Kind == ProcessorTransform {
			value = next
			result = processorResult{Input: &value.Input, Request: &value.Request, Response: &value.Response, Delta: &value.Delta, Result: &value.Result}
		}
	}
	return result, nil
}

func (a *Agent) processOne(ctx context.Context, processor Processor, run ProcessorRun, step int, value ProcessorContext) (ProcessorDecision, ProcessorContext, bool, error) {
	switch value.Phase {
	case ProcessorPhaseInput:
		p, ok := processor.(InputProcessor)
		if !ok {
			return ProcessorDecision{}, value, false, nil
		}
		out, err := p.ProcessInput(ctx, (ProcessorInputRequest{Run: run, Input: value.Input}).Clone())
		if err != nil {
			return ProcessorDecision{}, value, true, err
		}
		out = out.Clone()
		if out.Decision.Kind == ProcessorTransform {
			value.Input = out.Input
		}
		return out.Decision, value, true, nil
	case ProcessorPhaseModelRequest:
		p, ok := processor.(ModelRequestProcessor)
		if !ok {
			return ProcessorDecision{}, value, false, nil
		}
		out, err := p.ProcessModelRequest(ctx, (ProcessorModelRequest{Run: run, Step: step, Request: value.Request}).Clone())
		if err != nil {
			return ProcessorDecision{}, value, true, err
		}
		out = out.Clone()
		if out.Decision.Kind == ProcessorTransform {
			value.Request = out.Request
		}
		return out.Decision, value, true, nil
	case ProcessorPhaseModelResponse:
		p, ok := processor.(ModelResponseProcessor)
		if !ok {
			return ProcessorDecision{}, value, false, nil
		}
		out, err := p.ProcessModelResponse(ctx, (ProcessorModelResponseRequest{Run: run, Step: step, Request: value.Request, Response: value.Response}).Clone())
		if err != nil {
			return ProcessorDecision{}, value, true, err
		}
		out = out.Clone()
		if out.Decision.Kind == ProcessorTransform {
			value.Response = out.Response
		}
		return out.Decision, value, true, nil
	case ProcessorPhaseStreamDelta:
		p, ok := processor.(StreamDeltaProcessor)
		if !ok {
			return ProcessorDecision{}, value, false, nil
		}
		out, err := p.ProcessStreamDelta(ctx, (ProcessorStreamDeltaRequest{Run: run, Step: step, Delta: value.Delta}).Clone())
		if err != nil {
			return ProcessorDecision{}, value, true, err
		}
		out = out.Clone()
		if out.Decision.Kind == ProcessorTransform {
			value.Delta = out.Delta
		}
		return out.Decision, value, true, nil
	case ProcessorPhaseOutput:
		p, ok := processor.(OutputProcessor)
		if !ok {
			return ProcessorDecision{}, value, false, nil
		}
		out, err := p.ProcessOutput(ctx, (ProcessorOutputRequest{Run: run, Result: value.Result}).Clone())
		if err != nil {
			return ProcessorDecision{}, value, true, err
		}
		out = out.Clone()
		if out.Decision.Kind == ProcessorTransform {
			value.Result = out.Result
		}
		return out.Decision, value, true, nil
	}
	return ProcessorDecision{}, value, false, nil
}

func processorAgentError(step int, err error) *AgentError {
	return &AgentError{Kind: AgentErrorProcessor, Step: step, Err: err}
}

func processorCancelled(err error) bool {
	return errors.Is(err, ErrProcessorCancelled) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
