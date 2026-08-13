package runtime

import (
	"context"
	"errors"
	"testing"
)

type runtimeProcessor struct {
	name     string
	input    func(ProcessorInputRequest) ProcessorInputResult
	response func(ProcessorModelResponseRequest) ProcessorModelResponseResult
	delta    func(ProcessorStreamDeltaRequest) ProcessorStreamDeltaResult
}

type inputCancellationProcessor struct{}

func (inputCancellationProcessor) Name() string { return "cancel" }
func (inputCancellationProcessor) ProcessInput(context.Context, ProcessorInputRequest) (ProcessorInputResult, error) {
	return ProcessorInputResult{}, context.Canceled
}

type inactiveProcessor struct{}

func (inactiveProcessor) Name() string { return "inactive" }

func (p runtimeProcessor) Name() string { return p.name }
func (p runtimeProcessor) ProcessInput(_ context.Context, request ProcessorInputRequest) (ProcessorInputResult, error) {
	if p.input == nil {
		return ProcessorInputResult{Input: request.Input}, nil
	}
	return p.input(request), nil
}
func (p runtimeProcessor) ProcessModelResponse(_ context.Context, request ProcessorModelResponseRequest) (ProcessorModelResponseResult, error) {
	if p.response == nil {
		return ProcessorModelResponseResult{Response: request.Response}, nil
	}
	return p.response(request), nil
}
func (p runtimeProcessor) ProcessStreamDelta(_ context.Context, request ProcessorStreamDeltaRequest) (ProcessorStreamDeltaResult, error) {
	if p.delta == nil {
		return ProcessorStreamDeltaResult{Delta: request.Delta}, nil
	}
	return p.delta(request), nil
}

func TestAgentProcessorPipelineTransformsRun(t *testing.T) {
	pipeline, err := NewProcessorPipeline(runtimeProcessor{name: "redactor", input: func(request ProcessorInputRequest) ProcessorInputResult {
		input := request.Input
		input.Messages[0].Content = "safe input"
		return ProcessorInputResult{Decision: ProcessorDecision{Kind: ProcessorTransform}, Input: input}
	}, response: func(request ProcessorModelResponseRequest) ProcessorModelResponseResult {
		response := request.Response
		response.Message.Content = "safe output"
		return ProcessorModelResponseResult{Decision: ProcessorDecision{Kind: ProcessorTransform}, Response: response}
	}})
	if err != nil {
		t.Fatal(err)
	}
	model := newScriptedModel(textResponse("secret"))
	agent, err := NewAgent(AgentConfig{Definition: AgentDefinition{ID: "processor"}, Model: model, Processors: pipeline})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := model.calls[0].Messages[0].Content; got != "safe input" {
		t.Fatalf("request = %q", got)
	}
	if got := result.Messages[len(result.Messages)-1].Content; got != "safe output" {
		t.Fatalf("output = %q", got)
	}
}

func TestAgentProcessorPipelinePassesRequestToResponseProcessor(t *testing.T) {
	var got ModelRequest
	pipeline, err := NewProcessorPipeline(runtimeProcessor{name: "inspect", response: func(request ProcessorModelResponseRequest) ProcessorModelResponseResult {
		got = request.Request
		return ProcessorModelResponseResult{Response: request.Response}
	}})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgent(AgentConfig{Definition: AgentDefinition{ID: "processor"}, Model: newScriptedModel(textResponse("done")), Processors: pipeline})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "hello"}}}); err != nil {
		t.Fatal(err)
	}
	if got.Messages[len(got.Messages)-1].Content != "hello" {
		t.Fatalf("response processor request = %#v", got)
	}
}

func TestAgentProcessorPipelineCancellationIsRunCancellation(t *testing.T) {
	pipeline, err := NewProcessorPipeline(inputCancellationProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgent(AgentConfig{Definition: AgentDefinition{ID: "processor"}, Model: newScriptedModel(textResponse("must not run")), Processors: pipeline})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "hello"}}})
	if !errors.Is(err, ErrAgentCancelled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if result.Status != RunStatusCancelled {
		t.Fatalf("status = %q", result.Status)
	}
}

func TestAgentProcessorPipelineSkipsInactivePhaseEvents(t *testing.T) {
	pipeline, err := NewProcessorPipeline(inactiveProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	recorder := NewRunRecorder()
	agent, err := NewAgent(AgentConfig{Definition: AgentDefinition{ID: "processor"}, Model: newScriptedModel(textResponse("done")), Processors: pipeline, Listener: recorder})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "hello"}}}); err != nil {
		t.Fatal(err)
	}
	for _, event := range recorder.Events() {
		if event.Type == RunEventProcessor {
			t.Fatalf("inactive processor emitted %#v", event)
		}
	}
}

func TestAgentProcessorPipelineBlockShortCircuits(t *testing.T) {
	pipeline, err := NewProcessorPipeline(runtimeProcessor{name: "blocker", input: func(request ProcessorInputRequest) ProcessorInputResult {
		return ProcessorInputResult{Decision: ProcessorDecision{Kind: ProcessorBlock, Reason: "sensitive"}, Input: request.Input}
	}})
	if err != nil {
		t.Fatal(err)
	}
	model := newScriptedModel(textResponse("must not run"))
	agent, err := NewAgent(AgentConfig{Definition: AgentDefinition{ID: "processor"}, Model: model, Processors: pipeline})
	if err != nil {
		t.Fatal(err)
	}
	_, err = agent.Run(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "secret"}}})
	if !errors.Is(err, ErrAgentProcessor) || !errors.Is(err, ErrProcessorBlocked) {
		t.Fatalf("error = %v", err)
	}
	if len(model.calls) != 0 {
		t.Fatalf("model calls = %d", len(model.calls))
	}
}

func TestAgentProcessorPipelineTransformsStreamDelta(t *testing.T) {
	pipeline, err := NewProcessorPipeline(runtimeProcessor{name: "redactor", delta: func(request ProcessorStreamDeltaRequest) ProcessorStreamDeltaResult {
		delta := request.Delta
		if delta.Text != "" {
			delta.Text = "safe"
			return ProcessorStreamDeltaResult{Decision: ProcessorDecision{Kind: ProcessorTransform}, Delta: delta}
		}
		return ProcessorStreamDeltaResult{Delta: delta}
	}})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgent(AgentConfig{Definition: AgentDefinition{ID: "processor"}, Model: newStreamScriptedModel(textDeltas("secret")), Processors: pipeline})
	if err != nil {
		t.Fatal(err)
	}
	run, err := agent.RunStream(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	var first StreamDelta
	for delta := range run.Deltas {
		if delta.Text != "" {
			first = delta
		}
	}
	result, err := run.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if first.Text != "safe" {
		t.Fatalf("delta = %q", first.Text)
	}
	if got := result.Messages[len(result.Messages)-1].Content; got != "safe" {
		t.Fatalf("output = %q", got)
	}
}
