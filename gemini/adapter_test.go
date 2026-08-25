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
	t.Run("reasoning", func(t *testing.T) {
		response, err := model.response(lebro.ModelRequest{}, &genai.GenerateContentResponse{
			Candidates: []*genai.Candidate{{
				FinishReason: genai.FinishReasonStop,
				Content: genai.NewContentFromParts([]*genai.Part{
					{Text: "check constraints", Thought: true, ThoughtSignature: []byte("opaque")},
					genai.NewPartFromText("answer"),
				}, genai.RoleModel),
			}},
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 3, CandidatesTokenCount: 8, ThoughtsTokenCount: 5, TotalTokenCount: 11},
		})
		if err != nil {
			t.Fatal(err)
		}
		if response.Message.Content != "answer" || response.Message.Reasoning.Text != "check constraints" {
			t.Fatalf("message = %#v", response.Message)
		}
		if response.Usage.ReasoningTokens != 5 {
			t.Fatalf("usage = %#v", response.Usage)
		}
	})
}

func TestReasoningConfigMapsByGeminiGenerationAndReplaysSignatures(t *testing.T) {
	config, err := geminiThinkingConfig("gemini-2.5-flash", lebro.ReasoningConfig{Effort: lebro.ReasoningHigh})
	if err != nil {
		t.Fatal(err)
	}
	if config.ThinkingBudget == nil || *config.ThinkingBudget != 8192 || !config.IncludeThoughts {
		t.Fatalf("Gemini 2.5 config = %#v", config)
	}
	config, err = geminiThinkingConfig("gemini-3-pro-preview", lebro.ReasoningConfig{Effort: lebro.ReasoningLow})
	if err != nil {
		t.Fatal(err)
	}
	if config.ThinkingLevel != genai.ThinkingLevelLow || !config.IncludeThoughts {
		t.Fatalf("Gemini 3 config = %#v", config)
	}
	if _, err := geminiThinkingConfig("gemini-3-pro-preview", lebro.ReasoningConfig{Effort: lebro.ReasoningXHigh}); err == nil {
		t.Fatal("Gemini 3 accepted unsupported xhigh effort")
	}

	model, err := New(Config{APIKey: "key", Model: "gemini-2.5-flash"})
	if err != nil {
		t.Fatal(err)
	}
	_, contents, _, err := model.params(lebro.ModelRequest{Messages: []lebro.Message{{
		Role:      lebro.RoleAssistant,
		Reasoning: lebro.ModelReasoning{Details: lebro.NewModelReasoningDetails(json.RawMessage(`[{"text":"check","thought_signature":"b3BhcXVl"}]`))},
		Content:   "answer",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) != 1 || len(contents[0].Parts) != 2 || !contents[0].Parts[0].Thought || string(contents[0].Parts[0].ThoughtSignature) != "opaque" {
		t.Fatalf("replayed assistant parts = %#v", contents)
	}
	foreign, err := geminiReasoningParts(lebro.ModelReasoning{Details: lebro.NewModelReasoningDetails(json.RawMessage(`[{"type":"thinking","signature":"opaque"}]`))})
	if err != nil || len(foreign) != 0 {
		t.Fatalf("foreign reasoning replay = %#v, %v", foreign, err)
	}
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
