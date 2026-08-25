package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	claude "github.com/anthropics/anthropic-sdk-go"
	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
)

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New() error = nil, want API key error")
	}
	if _, err := New(Config{APIKey: "key", MaxTokens: -1}); err == nil {
		t.Fatal("New() error = nil, want max tokens error")
	}
}

func TestStreamSendUnblocksOnClose(t *testing.T) {
	r := &anthropicStream{values: make(chan lebro.StreamDelta), done: make(chan struct{})}
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

func TestStreamInitializesValues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()
	model, err := New(Config{APIKey: "key", Model: "fixture-model", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := model.Stream(context.Background(), lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	stream, ok := reader.(*anthropicStream)
	if !ok || stream.values == nil {
		t.Fatalf("Stream() reader = %#v, want initialized anthropic stream", reader)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	for {
		_, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestProviderContractFixturesBuildRequests(t *testing.T) {
	model, err := New(Config{APIKey: "key", Model: "fixture-model"})
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range testkit.ProviderContractCases() {
		if _, err := model.params(fixture.Request); err != nil {
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
		response, err := model.response(lebro.ModelRequest{}, &claude.Message{Content: []claude.ContentBlockUnion{{Type: "text", Text: "checking"}, {Type: "tool_use", ID: "call-1", Name: "lookup", Input: json.RawMessage(`{"city":"Nairobi"}`)}}, StopReason: "tool_use"})
		if err != nil {
			t.Fatal(err)
		}
		if response.FinishReason != lebro.FinishReasonToolCalls || response.Message.Content != "checking" {
			t.Fatalf("response = %#v", response)
		}
		calls := response.Message.ToolCalls.Values()
		if len(calls) != 1 || calls[0].ID != "call-1" {
			t.Fatalf("calls = %#v", calls)
		}
	})
	t.Run("structured", func(t *testing.T) {
		request := lebro.ModelRequest{OutputSchema: &lebro.ModelOutputSchema{Schema: json.RawMessage(`{"type":"object"}`)}}
		response, err := model.response(request, &claude.Message{Content: []claude.ContentBlockUnion{{Type: "text", Text: `{"ok":true}`}}, StopReason: "end_turn"})
		if err != nil {
			t.Fatal(err)
		}
		if got := string(response.Message.StructuredOutput); got != `{"ok":true}` {
			t.Fatalf("structured output = %s", got)
		}
	})
	t.Run("reasoning", func(t *testing.T) {
		response, err := model.response(lebro.ModelRequest{}, &claude.Message{
			Content: []claude.ContentBlockUnion{
				{Type: "thinking", Thinking: "check constraints", Signature: "opaque-signature"},
				{Type: "text", Text: "answer"},
			},
			StopReason: "end_turn",
			Usage: claude.Usage{
				InputTokens: 3, OutputTokens: 8,
				OutputTokensDetails: claude.OutputTokensDetails{ThinkingTokens: 5},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if response.Message.Content != "answer" || response.Message.Reasoning.Text != "check constraints" {
			t.Fatalf("message = %#v", response.Message)
		}
		if got := string(response.Message.Reasoning.Details.Raw()); got != `[{"type":"thinking","thinking":"check constraints","signature":"opaque-signature"}]` {
			t.Fatalf("reasoning details = %s", got)
		}
		if response.Usage.ReasoningTokens != 5 {
			t.Fatalf("usage = %#v", response.Usage)
		}
	})
}

func TestReasoningParamsMapBudgetsAndReplayOpaqueBlocks(t *testing.T) {
	model, err := New(Config{APIKey: "key", Model: "fixture-model", MaxTokens: 8000})
	if err != nil {
		t.Fatal(err)
	}
	params, err := model.params(lebro.ModelRequest{
		Reasoning: lebro.ReasoningConfig{Effort: lebro.ReasoningMedium},
		Messages: []lebro.Message{{
			Role:      lebro.RoleAssistant,
			Reasoning: lebro.ModelReasoning{Details: lebro.NewModelReasoningDetails(json.RawMessage(`[{"type":"thinking","thinking":"check","signature":"opaque"}]`))},
			Content:   "answer",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if params.Thinking.OfEnabled == nil || params.Thinking.OfEnabled.BudgetTokens != 4000 {
		t.Fatalf("thinking params = %#v", params.Thinking)
	}
	if len(params.Messages) != 1 {
		t.Fatalf("messages = %#v", params.Messages)
	}
	encoded, err := json.Marshal(params.Messages[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"signature":"opaque"`) {
		t.Fatalf("replayed assistant turn lost thinking signature: %s", encoded)
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
