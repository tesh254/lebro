package runtime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestAgentProcessorsOrderAndTransformRun(t *testing.T) {
	model := newScriptedModel(textResponse("model"))
	var phases []ProcessorPhase
	processor := ProcessorFunc(func(_ context.Context, value ProcessorContext) (ProcessorDecision, error) {
		phases = append(phases, value.Phase)
		switch value.Phase {
		case ProcessorPhaseInput:
			input := value.Input
			input.Messages[0].Content = "sanitized input"
			return ProcessorDecision{Action: ProcessorTransform, Input: &input}, nil
		case ProcessorPhaseModelResponse:
			response := value.Response
			response.Message.Content = "sanitized output"
			return ProcessorDecision{Action: ProcessorTransform, Response: &response}, nil
		default:
			return ProcessorDecision{}, nil
		}
	})
	agent, err := NewAgent(AgentConfig{Definition: AgentDefinition{ID: "processor"}, Model: model, Processors: []Processor{processor}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := result.Messages[len(result.Messages)-1].Content, "sanitized output"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if got, want := model.calls[0].Messages[0].Content, "sanitized input"; got != want {
		t.Fatalf("model input = %q, want %q", got, want)
	}
	if want := []ProcessorPhase{ProcessorPhaseInput, ProcessorPhaseModelRequest, ProcessorPhaseModelResponse, ProcessorPhaseOutput}; !reflect.DeepEqual(phases, want) {
		t.Fatalf("phases = %#v, want %#v", phases, want)
	}
}

func TestAgentProcessorBlockShortCircuitsWithoutPayloadEvent(t *testing.T) {
	model := newScriptedModel(textResponse("must not run"))
	recorder := NewRunRecorder()
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "processor"}, Model: model, Listener: recorder,
		Processors: []Processor{ProcessorFunc(func(context.Context, ProcessorContext) (ProcessorDecision, error) {
			return ProcessorDecision{Action: ProcessorBlock, Reason: "secret input"}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = agent.Run(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "secret input"}}})
	if !errors.Is(err, ErrAgentProcessor) || !errors.Is(err, ErrProcessorBlocked) {
		t.Fatalf("error = %v, want processor block", err)
	}
	if strings.Contains(err.Error(), "secret input") {
		t.Fatalf("error leaked processor reason: %v", err)
	}
	if got := len(model.calls); got != 0 {
		t.Fatalf("model calls = %d, want 0", got)
	}
	for _, event := range recorder.Events() {
		if event.Type == RunEventProcessor && (event.DeltaText != "" || event.DeltaStructuredOutput != "") {
			t.Fatalf("processor event leaked payload: %#v", event)
		}
	}
}

func TestAgentProcessorsMatchRunAndStreamResponse(t *testing.T) {
	processor := ProcessorFunc(func(_ context.Context, value ProcessorContext) (ProcessorDecision, error) {
		if value.Phase != ProcessorPhaseModelResponse {
			return ProcessorDecision{}, nil
		}
		response := value.Response
		response.Message.Content = "redacted"
		return ProcessorDecision{Action: ProcessorTransform, Response: &response}, nil
	})
	nonStream, err := NewAgent(AgentConfig{Definition: AgentDefinition{ID: "processor"}, Model: newScriptedModel(textResponse("secret")), Processors: []Processor{processor}})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := NewAgent(AgentConfig{Definition: AgentDefinition{ID: "processor"}, Model: newStreamScriptedModel(textDeltas("secret")), Processors: []Processor{processor}})
	if err != nil {
		t.Fatal(err)
	}
	nonStreamResult, err := nonStream.Run(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	streamRun, err := stream.RunStream(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	streamResult, err := streamRun.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := streamResult.Messages[len(streamResult.Messages)-1].Content, nonStreamResult.Messages[len(nonStreamResult.Messages)-1].Content; got != want {
		t.Fatalf("stream output = %q, want %q", got, want)
	}
}

func TestAgentStreamDeltaProcessorTransformsCallerDelta(t *testing.T) {
	processor := ProcessorFunc(func(_ context.Context, value ProcessorContext) (ProcessorDecision, error) {
		if value.Phase != ProcessorPhaseStreamDelta || value.Delta.Text == "" {
			return ProcessorDecision{}, nil
		}
		delta := value.Delta
		delta.Text = "redacted"
		return ProcessorDecision{Action: ProcessorTransform, Delta: &delta}, nil
	})
	agent, err := NewAgent(AgentConfig{Definition: AgentDefinition{ID: "processor"}, Model: newStreamScriptedModel(textDeltas("secret")), Processors: []Processor{processor}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := agent.RunStream(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	var deltas []StreamDelta
	for delta := range run.Deltas {
		deltas = append(deltas, delta)
	}
	result, err := run.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := deltas[0].Text, "redacted"; got != want {
		t.Fatalf("delta = %q, want %q", got, want)
	}
	if got, want := result.Messages[len(result.Messages)-1].Content, "redacted"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}
