package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestProcessorDecisionNormalizesAndRejectsUnknownKinds(t *testing.T) {
	t.Parallel()
	decision, err := NormalizeProcessorDecision(ProcessorDecision{})
	if err != nil || decision.Kind != ProcessorAllow {
		t.Fatalf("NormalizeProcessorDecision() = %#v, %v", decision, err)
	}
	for _, kind := range []ProcessorDecisionKind{ProcessorAllow, ProcessorTransform, ProcessorBlock} {
		if _, err := NormalizeProcessorDecision(ProcessorDecision{Kind: kind}); err != nil {
			t.Fatalf("%q: %v", kind, err)
		}
	}
	if _, err := NormalizeProcessorDecision(ProcessorDecision{Kind: "drop"}); err == nil || !errors.Is(err, ErrProcessorInvalidDecision) {
		t.Fatal("unknown decision was accepted")
	}
}

func TestProcessorPipelinePreservesOrderAndDefensivelyCopiesSlice(t *testing.T) {
	t.Parallel()
	first, second := namedProcessor("first"), namedProcessor("second")
	pipeline, err := NewProcessorPipeline(first, second)
	if err != nil {
		t.Fatal(err)
	}
	processors := pipeline.Processors()
	if len(processors) != 2 || processors[0].Name() != "first" || processors[1].Name() != "second" {
		t.Fatalf("processors = %#v", processors)
	}
	processors[0] = second
	if pipeline.Processors()[0].Name() != "first" {
		t.Fatal("Processors returned the pipeline-owned slice")
	}
	if _, err := NewProcessorPipeline(nil); err == nil {
		t.Fatal("nil processor was accepted")
	}
}

type namedProcessor string

func (p namedProcessor) Name() string { return string(p) }

func TestProcessorRequestsAndResultsDefensivelyCopy(t *testing.T) {
	t.Parallel()
	request := ProcessorModelRequest{Run: ProcessorRun{Agent: AgentDefinition{Tools: []ToolID{"lookup"}}, Metadata: map[string]string{"tenant": "a"}}, Request: ModelRequest{Messages: []Message{{Role: RoleUser, Content: "original"}}, Tools: []ToolDefinition{{ID: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}, Extension: json.RawMessage(`{"source":"test"}`)}}
	copy := request.Clone()
	copy.Run.Agent.Tools[0] = "changed"
	copy.Run.Metadata["tenant"] = "b"
	copy.Request.Messages[0].Content = "changed"
	copy.Request.Tools[0].InputSchema[2] = 'X'
	copy.Request.Extension[2] = 'X'
	if request.Run.Agent.Tools[0] != "lookup" || request.Run.Metadata["tenant"] != "a" || request.Request.Messages[0].Content != "original" || request.Request.Tools[0].InputSchema[2] != 't' || request.Request.Extension[2] != 's' {
		t.Fatalf("Clone mutated source: %#v", request)
	}

	result := ProcessorOutputResult{Result: RunResult{Messages: []Message{{Role: RoleAssistant, Content: "original"}}, Metadata: map[string]string{"request": "1"}, ModelAttempts: []ModelAttempt{{Error: &ModelError{Extension: json.RawMessage(`{"code":"original"}`)}}}}}
	resultCopy := result.Clone()
	resultCopy.Result.Messages[0].Content = "changed"
	resultCopy.Result.Metadata["request"] = "2"
	resultCopy.Result.ModelAttempts[0].Error.Extension[9] = 'X'
	if result.Result.Messages[0].Content != "original" || result.Result.Metadata["request"] != "1" || result.Result.ModelAttempts[0].Error.Extension[9] != 'o' {
		t.Fatalf("result Clone mutated source: %#v", result)
	}
}

func TestProcessorStreamDeltaCloneCopiesToolArguments(t *testing.T) {
	t.Parallel()
	request := ProcessorStreamDeltaRequest{Delta: StreamDelta{ToolCall: &ModelToolCall{ID: "call", ToolID: "tool", Arguments: json.RawMessage(`{"a":1}`)}}}
	copy := request.Clone()
	copy.Delta.ToolCall.Arguments[2] = 'X'
	if request.Delta.ToolCall.Arguments[2] != 'a' {
		t.Fatalf("Clone mutated source arguments: %s", request.Delta.ToolCall.Arguments)
	}
}

func TestNormalizeProcessorError(t *testing.T) {
	t.Parallel()
	ordinary := errors.New("boom")
	err := NormalizeProcessorError(ProcessorPhaseInput, "redactor", ordinary)
	var processorErr *ProcessorError
	if !errors.As(err, &processorErr) || processorErr.Kind != ProcessorErrorFailed || !errors.Is(err, ErrProcessorFailed) || !errors.Is(err, ordinary) {
		t.Fatalf("ordinary error = %v", err)
	}
	cancelled := NormalizeProcessorError(ProcessorPhaseModelRequest, "guard", context.Canceled)
	if !errors.Is(cancelled, ErrProcessorCancelled) || !errors.Is(cancelled, context.Canceled) {
		t.Fatalf("cancelled error = %v", cancelled)
	}
	if NormalizeProcessorError(ProcessorPhaseInput, "x", nil) != nil {
		t.Fatal("nil error did not remain nil")
	}
}
