package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
	genai "google.golang.org/genai"
)

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New() error = nil, want API key error")
	}
}

func TestAPIErrorIsNormalized(t *testing.T) {
	model, err := New(Config{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	err = model.error(context.Background(), genai.APIError{Code: 429, Status: "RESOURCE_EXHAUSTED", Message: "slow down"})
	var modelErr *lebro.ModelError
	if !errors.As(err, &modelErr) || modelErr.Kind != lebro.ModelErrorRateLimited || modelErr.StatusCode != 429 {
		t.Fatalf("error = %#v", err)
	}
}

func TestStreamSendUnblocksOnClose(t *testing.T) {
	r := &geminiStream{values: make(chan lebro.StreamDelta), done: make(chan struct{})}
	finished := make(chan bool, 1)
	go func() { finished <- r.send(lebro.StreamDelta{Text: "blocked"}) }()
	close(r.done)
	select {
	case delivered := <-finished:
		if delivered {
			t.Fatal("send reported delivery after close")
		}
	case <-time.After(time.Second):
		t.Fatal("send remained blocked after close")
	}
}

func TestProviderContractFixturesBuildRequests(t *testing.T) {
	model, err := New(Config{APIKey: "key", Model: "fixture-model"})
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range testkit.ProviderContractCases() {
		if _, _, _, err := model.params(fixture.Request); err != nil {
			t.Fatalf("%s: %v", fixture.Name, err)
		}
	}
}

func TestResponseMapsTextToolsAndStructuredOutput(t *testing.T) {
	model, err := New(Config{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("tool", func(t *testing.T) {
		response, err := model.response(lebro.ModelRequest{}, &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{FinishReason: genai.FinishReasonStop, Content: genai.NewContentFromParts([]*genai.Part{genai.NewPartFromText("checking"), {FunctionCall: &genai.FunctionCall{ID: "call-1", Name: "lookup", Args: map[string]any{"city": "Nairobi"}}}}, genai.RoleModel)}}})
		if err != nil {
			t.Fatal(err)
		}
		calls := response.Message.ToolCalls.Values()
		if response.FinishReason != lebro.FinishReasonToolCalls || len(calls) != 1 || calls[0].ToolID != "lookup" {
			t.Fatalf("response = %#v", response)
		}
	})
	t.Run("structured", func(t *testing.T) {
		request := lebro.ModelRequest{OutputSchema: &lebro.ModelOutputSchema{Schema: json.RawMessage(`{"type":"object"}`)}}
		response, err := model.response(request, &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{FinishReason: genai.FinishReasonStop, Content: genai.NewContentFromText(`{"ok":true}`, genai.RoleModel)}}})
		if err != nil {
			t.Fatal(err)
		}
		if string(response.Message.StructuredOutput) != `{"ok":true}` {
			t.Fatalf("structured = %s", response.Message.StructuredOutput)
		}
	})
}

func TestCancelledContextIsReturnedDirectly(t *testing.T) {
	model, err := New(Config{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(model.error(ctx, context.Canceled), context.Canceled) {
		t.Fatal("cancellation was not preserved")
	}
}
