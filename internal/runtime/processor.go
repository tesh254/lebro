package runtime

import (
	"context"
	"errors"
	"fmt"
)

// ProcessorPhase identifies the point in an agent run at which a Processor is
// invoked. Calls within a phase follow the configured processor order.
type ProcessorPhase string

const (
	ProcessorPhaseInput         ProcessorPhase = "input"
	ProcessorPhaseModelRequest  ProcessorPhase = "model_request"
	ProcessorPhaseModelResponse ProcessorPhase = "model_response"
	ProcessorPhaseStreamDelta   ProcessorPhase = "stream_delta"
	ProcessorPhaseOutput        ProcessorPhase = "output"
)

// ProcessorDecisionKind is the typed outcome of a processor invocation.
type ProcessorDecisionKind string

const (
	// ProcessorAllow continues the pipeline without changing the phase value.
	ProcessorAllow ProcessorDecisionKind = "allow"
	// ProcessorTransform continues the pipeline with the value in the result.
	ProcessorTransform ProcessorDecisionKind = "transform"
	// ProcessorBlock stops the current run before the next processor or runtime
	// action is invoked.
	ProcessorBlock ProcessorDecisionKind = "block"
)

// ProcessorDecision controls whether a phase continues, uses a transformed
// value, or stops. A zero Decision is normalized to ProcessorAllow so a result
// can leave it unset when it does not alter control flow.
type ProcessorDecision struct {
	Kind   ProcessorDecisionKind
	Reason string
}

// NormalizeProcessorDecision validates a decision and resolves its zero value
// to ProcessorAllow.
func NormalizeProcessorDecision(decision ProcessorDecision) (ProcessorDecision, error) {
	if decision.Kind == "" {
		decision.Kind = ProcessorAllow
	}
	switch decision.Kind {
	case ProcessorAllow, ProcessorTransform, ProcessorBlock:
		return decision, nil
	default:
		return ProcessorDecision{}, fmt.Errorf("%w: %q", ErrProcessorInvalidDecision, decision.Kind)
	}
}

// Processor is the common identity contract for optional phase processors.
// A processor implements one or more of the phase-specific interfaces below.
// Implementations must be safe for concurrent use and must not retain or
// mutate request values after their method returns.
type Processor interface {
	Name() string
}

// ProcessorPipeline is an immutable, ordered collection of processors. The
// runtime invokes phase-specific implementations in this order and stops at a
// ProcessorBlock decision. Processors are not copied; their implementations
// must therefore be safe for concurrent use.
type ProcessorPipeline struct {
	processors []Processor
}

// NewProcessorPipeline validates and retains processors in declaration order.
func NewProcessorPipeline(processors ...Processor) (ProcessorPipeline, error) {
	for i, processor := range processors {
		if processor == nil || isNilInterface(processor) {
			return ProcessorPipeline{}, fmt.Errorf("lebro: processor %d is nil", i)
		}
	}
	return ProcessorPipeline{processors: append([]Processor(nil), processors...)}, nil
}

// Processors returns the configured processors in invocation order. The slice
// is caller-owned; its elements retain their original processor identities.
func (p ProcessorPipeline) Processors() []Processor {
	return append([]Processor(nil), p.processors...)
}

// ProcessorRun is immutable run metadata supplied to every processor. Its
// Clone method returns a caller-owned copy of its maps and slices.
type ProcessorRun struct {
	ID       RunID
	Agent    AgentDefinition
	ThreadID ThreadID
	Metadata map[string]string
	Memory   *MemoryProcessorConfig
}

// Clone returns a caller-owned copy of the run metadata.
func (r ProcessorRun) Clone() ProcessorRun {
	r.Agent.Tools = append([]ToolID(nil), r.Agent.Tools...)
	r.Metadata = cloneMetadata(r.Metadata)
	r.Memory = r.Memory.Clone()
	return r
}

// InputProcessor handles input immediately before an agent begins its run.
type InputProcessor interface {
	Processor
	ProcessInput(context.Context, ProcessorInputRequest) (ProcessorInputResult, error)
}

type ProcessorInputRequest struct {
	Run   ProcessorRun
	Input RunInput
}

func (r ProcessorInputRequest) Clone() ProcessorInputRequest {
	r.Run = r.Run.Clone()
	r.Input.Messages = cloneMessages(r.Input.Messages)
	r.Input.Metadata = cloneMetadata(r.Input.Metadata)
	r.Input.OutputSchema = cloneModelOutputSchema(r.Input.OutputSchema)
	r.Input.Memory = r.Input.Memory.Clone()
	return r
}

type ProcessorInputResult struct {
	Decision ProcessorDecision
	Input    RunInput
}

func (r ProcessorInputResult) Clone() ProcessorInputResult {
	r.Input.Messages = cloneMessages(r.Input.Messages)
	r.Input.Metadata = cloneMetadata(r.Input.Metadata)
	r.Input.OutputSchema = cloneModelOutputSchema(r.Input.OutputSchema)
	r.Input.Memory = r.Input.Memory.Clone()
	return r
}

// ModelRequestProcessor handles each provider-neutral model request.
type ModelRequestProcessor interface {
	Processor
	ProcessModelRequest(context.Context, ProcessorModelRequest) (ProcessorModelRequestResult, error)
}

type ProcessorModelRequest struct {
	Run     ProcessorRun
	Step    int
	Request ModelRequest
}

func (r ProcessorModelRequest) Clone() ProcessorModelRequest {
	r.Run = r.Run.Clone()
	r.Request.Messages = cloneMessages(r.Request.Messages)
	r.Request.Tools = cloneToolDefinitions(r.Request.Tools)
	r.Request.OutputSchema = cloneModelOutputSchema(r.Request.OutputSchema)
	r.Request.Extension = cloneRawMessage(r.Request.Extension)
	return r
}

type ProcessorModelRequestResult struct {
	Decision ProcessorDecision
	Request  ModelRequest
}

func (r ProcessorModelRequestResult) Clone() ProcessorModelRequestResult {
	r.Request.Messages = cloneMessages(r.Request.Messages)
	r.Request.Tools = cloneToolDefinitions(r.Request.Tools)
	r.Request.OutputSchema = cloneModelOutputSchema(r.Request.OutputSchema)
	r.Request.Extension = cloneRawMessage(r.Request.Extension)
	return r
}

// ModelResponseProcessor handles each completed model response.
type ModelResponseProcessor interface {
	Processor
	ProcessModelResponse(context.Context, ProcessorModelResponseRequest) (ProcessorModelResponseResult, error)
}

type ProcessorModelResponseRequest struct {
	Run      ProcessorRun
	Step     int
	Request  ModelRequest
	Response ModelResponse
}

func (r ProcessorModelResponseRequest) Clone() ProcessorModelResponseRequest {
	r.Run = r.Run.Clone()
	r.Request = ProcessorModelRequest{Request: r.Request}.Clone().Request
	r.Response.Extension = cloneRawMessage(r.Response.Extension)
	return r
}

type ProcessorModelResponseResult struct {
	Decision ProcessorDecision
	Response ModelResponse
}

func (r ProcessorModelResponseResult) Clone() ProcessorModelResponseResult {
	r.Response.Extension = cloneRawMessage(r.Response.Extension)
	return r
}

// StreamDeltaProcessor handles each delta from a streaming model request.
type StreamDeltaProcessor interface {
	Processor
	ProcessStreamDelta(context.Context, ProcessorStreamDeltaRequest) (ProcessorStreamDeltaResult, error)
}

type ProcessorStreamDeltaRequest struct {
	Run   ProcessorRun
	Step  int
	Delta StreamDelta
}

func (r ProcessorStreamDeltaRequest) Clone() ProcessorStreamDeltaRequest {
	r.Run = r.Run.Clone()
	if r.Delta.ToolCall != nil {
		call := *r.Delta.ToolCall
		call.Arguments = cloneRawMessage(call.Arguments)
		r.Delta.ToolCall = &call
	}
	return r
}

type ProcessorStreamDeltaResult struct {
	Decision ProcessorDecision
	Delta    StreamDelta
}

func (r ProcessorStreamDeltaResult) Clone() ProcessorStreamDeltaResult {
	request := ProcessorStreamDeltaRequest{Delta: r.Delta}.Clone()
	return ProcessorStreamDeltaResult{Decision: r.Decision, Delta: request.Delta}
}

// OutputProcessor handles the terminal result before it is returned to the
// caller. It cannot turn a failed or cancelled result into a successful one.
type OutputProcessor interface {
	Processor
	ProcessOutput(context.Context, ProcessorOutputRequest) (ProcessorOutputResult, error)
}

type ProcessorOutputRequest struct {
	Run    ProcessorRun
	Result RunResult
}

func (r ProcessorOutputRequest) Clone() ProcessorOutputRequest {
	r.Run = r.Run.Clone()
	r.Result.Messages = cloneMessages(r.Result.Messages)
	r.Result.Metadata = cloneMetadata(r.Result.Metadata)
	r.Result.ModelAttempts = cloneProcessorModelAttempts(r.Result.ModelAttempts)
	return r
}

func cloneProcessorModelAttempts(attempts []ModelAttempt) []ModelAttempt {
	if attempts == nil {
		return nil
	}
	cloned := make([]ModelAttempt, len(attempts))
	for i, attempt := range attempts {
		cloned[i] = attempt
		if attempt.Error != nil {
			err := *attempt.Error
			err.Extension = cloneRawMessage(err.Extension)
			cloned[i].Error = &err
		}
	}
	return cloned
}

type ProcessorOutputResult struct {
	Decision ProcessorDecision
	Result   RunResult
}

func (r ProcessorOutputResult) Clone() ProcessorOutputResult {
	request := ProcessorOutputRequest{Result: r.Result}.Clone()
	return ProcessorOutputResult{Decision: r.Decision, Result: request.Result}
}

type ProcessorErrorKind string

const (
	ProcessorErrorFailed          ProcessorErrorKind = "failed"
	ProcessorErrorCancelled       ProcessorErrorKind = "cancelled"
	ProcessorErrorInvalidDecision ProcessorErrorKind = "invalid_decision"
)

var (
	ErrProcessorFailed          = errors.New("lebro: processor failed")
	ErrProcessorCancelled       = errors.New("lebro: processor cancelled")
	ErrProcessorInvalidDecision = errors.New("lebro: processor invalid decision")
)

// ProcessorError adds phase and processor identity to a normalized processor
// failure while retaining the original cause for errors.Is and errors.As.
type ProcessorError struct {
	Kind      ProcessorErrorKind
	Phase     ProcessorPhase
	Processor string
	Err       error
}

func (e *ProcessorError) Error() string {
	if e == nil {
		return "lebro: processor failure"
	}
	name := e.Processor
	if name == "" {
		name = "unnamed"
	}
	if e.Err == nil {
		return fmt.Sprintf("lebro: processor %s %s at %s", name, e.Kind, e.Phase)
	}
	return fmt.Sprintf("lebro: processor %s %s at %s: %v", name, e.Kind, e.Phase, e.Err)
}

func (e *ProcessorError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *ProcessorError) Is(target error) bool {
	if e == nil {
		return false
	}
	switch e.Kind {
	case ProcessorErrorCancelled:
		return target == ErrProcessorCancelled
	case ProcessorErrorInvalidDecision:
		return target == ErrProcessorInvalidDecision
	default:
		return target == ErrProcessorFailed
	}
}

// NormalizeProcessorError converts arbitrary processor failures to the stable
// processor error vocabulary. Context cancellation retains its original
// sentinel so callers can use errors.Is with both processor and context errors.
func NormalizeProcessorError(phase ProcessorPhase, processor string, err error) error {
	if err == nil {
		return nil
	}
	var existing *ProcessorError
	if errors.As(err, &existing) {
		return err
	}
	kind := ProcessorErrorFailed
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		kind = ProcessorErrorCancelled
	}
	return &ProcessorError{Kind: kind, Phase: phase, Processor: processor, Err: err}
}
