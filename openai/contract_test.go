package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
)

// TestOpenAIAdapterPassesProviderContract runs the canonical provider contract
// cases (text, tool call, structured output, failure, cancellation) against a
// recorded HTTP transport, asserting both directions of each exchange: the
// neutral request translated onto the chat-completions wire and the provider
// response mapped back to neutral values.
func TestOpenAIAdapterPassesProviderContract(t *testing.T) {
	t.Parallel()
	for _, contractCase := range testkit.ProviderContractCases() {
		contractCase := contractCase
		if !supportsContractCase(contractCase) {
			continue
		}
		t.Run(contractCase.Name, func(t *testing.T) {
			t.Parallel()
			factory := buildFactory(t, contractCase)
			ctx := context.Background()
			if contractCase.Mode == testkit.ContractCancellation {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}

			response, err := factory.Model.Generate(ctx, cloneRequest(contractCase.Request))
			switch contractCase.Mode {
			case testkit.ContractResponse:
				assertContractResponse(t, response, contractCase.Response)
				assertContractObservedRequest(t, factory.observedBody, contractCase.Request)
			case testkit.ContractFailure:
				var modelErr *lebro.ModelError
				if !errors.As(err, &modelErr) || modelErr.Kind != contractCase.ErrorKind {
					t.Fatalf("Generate() error = %v, want model error kind %q", err, contractCase.ErrorKind)
				}
			case testkit.ContractCancellation:
				testkit.AssertCancellation(t, err)
			}
		})
	}
}

func supportsContractCase(contractCase testkit.ProviderCase) bool {
	switch contractCase.Name {
	case "text", "tool call", "structured output", "reasoning", "failure", "cancellation":
		return true
	default:
		return false
	}
}

type contractFactory struct {
	Model        lebro.Model
	observedBody []byte
}

func buildFactory(t *testing.T, contractCase testkit.ProviderCase) *contractFactory {
	t.Helper()
	factory := &contractFactory{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		factory.observedBody = append([]byte(nil), body...)
		serveContractCase(t, w, contractCase)
	}))
	t.Cleanup(server.Close)

	factory.Model = newAdapter(t, server, Config{APIKey: "contract-key", Model: "contract-model", Timeout: 1 * time.Second})
	return factory
}

// serveContractCase answers each contract mode with the wire shape a real
// OpenAI-compatible endpoint would produce for that exchange.
func serveContractCase(t *testing.T, w http.ResponseWriter, contractCase testkit.ProviderCase) {
	t.Helper()
	switch contractCase.Mode {
	case testkit.ContractResponse:
		message := chatChoiceMessage{Role: "assistant"}
		switch contractCase.Name {
		case "tool call":
			call := contractCase.Response.Message.ToolCalls.Values()[0]
			message.ToolCalls = []chatToolCall{{
				ID:       call.ID,
				Type:     "function",
				Function: chatToolFunction{Name: string(call.ToolID), Arguments: string(call.Arguments)},
			}}
		case "structured output":
			// Standard strict-mode shape: the JSON payload travels inside a
			// string content field.
			message.Content = json.RawMessage(`"{\"ok\":true}"`)
		case "reasoning":
			message.Content = json.RawMessage(`"answer"`)
			message.Reasoning = contractCase.Response.Message.Reasoning.Text
		default:
			message.Content = json.RawMessage(`"` + contractCase.Response.Message.Content + `"`)
		}
		usage := chatUsageBody{
			PromptTokens:     contractCase.Response.Usage.InputTokens,
			CompletionTokens: contractCase.Response.Usage.OutputTokens,
			TotalTokens:      contractCase.Response.Usage.TotalTokens,
		}
		usage.CompletionTokensDetails.ReasoningTokens = contractCase.Response.Usage.ReasoningTokens
		writeJSON(t, w, http.StatusOK, chatResponse{
			ID: "chatcmpl-contract", Model: contractCase.Request.Model,
			Choices: []chatChoice{{
				Message:      message,
				FinishReason: string(contractCase.Response.FinishReason),
			}},
			Usage: usage,
		})
	case testkit.ContractFailure:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"contract provider failure","type":"unavailable"}}`)
	case testkit.ContractCancellation:
		// The context is already cancelled; the client never reaches the
		// handler. Block for a bounded time as a guard so a buggy adapter
		// that ignored the deadline would time out (via the 1s Config.Timeout)
		// instead of succeeding or hanging the test indefinitely.
		time.Sleep(5 * time.Second)
	}
}

func decodeObservedRequest(t *testing.T, body []byte) lebro.ModelRequest {
	t.Helper()
	var parsed chatWireRequest
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode observed wire request: %v", err)
	}
	messages := make([]lebro.Message, 0, len(parsed.Messages))
	for _, message := range parsed.Messages {
		messages = append(messages, lebro.Message{Role: mapWireRole(message.Role), Content: message.Content})
	}
	request := lebro.ModelRequest{Model: parsed.Model, Messages: messages}
	if parsed.Reasoning != nil {
		if effort, ok := parsed.Reasoning["effort"].(string); ok {
			if effort == "none" {
				effort = "off"
			}
			request.Reasoning.Effort = lebro.ReasoningEffort(effort)
		}
		if budget, ok := parsed.Reasoning["max_tokens"].(float64); ok {
			request.Reasoning.BudgetTokens = int64(budget)
		}
	}
	for _, tool := range parsed.Tools {
		request.Tools = append(request.Tools, lebro.ToolDefinition{
			ID: lebro.ToolID(tool.Function.Name), Description: tool.Function.Description,
		})
	}
	if parsed.ResponseFormat != nil {
		jsonSchema, _ := parsed.ResponseFormat["json_schema"].(map[string]any)
		name, _ := jsonSchema["name"].(string)
		strict, _ := jsonSchema["strict"].(bool)
		schemaEncoded, _ := json.Marshal(jsonSchema["schema"])
		request.OutputSchema = &lebro.ModelOutputSchema{Name: name, Strict: strict, Schema: schemaEncoded}
	}
	return request
}

func assertContractResponse(t *testing.T, got, want lebro.ModelResponse) {
	t.Helper()
	if got.Message.Role != want.Message.Role {
		t.Fatalf("role = %q, want %q", got.Message.Role, want.Message.Role)
	}
	if want.Message.StructuredOutput != "" {
		if got.Message.StructuredOutput == "" {
			t.Fatal("structured output missing")
		}
		if got, want := string(got.Message.StructuredOutput.Raw()), string(want.Message.StructuredOutput.Raw()); got != want {
			t.Fatalf("structured output = %s, want %s", got, want)
		}
	} else if got.Message.Content != want.Message.Content {
		t.Fatalf("content = %q, want %q", got.Message.Content, want.Message.Content)
	}
	if got.Message.Reasoning.Text != want.Message.Reasoning.Text {
		t.Fatalf("reasoning = %q, want %q", got.Message.Reasoning.Text, want.Message.Reasoning.Text)
	}
	gotCalls := got.Message.ToolCalls.Values()
	wantCalls := want.Message.ToolCalls.Values()
	if len(gotCalls) != len(wantCalls) {
		t.Fatalf("tool calls = %#v, want %#v", gotCalls, wantCalls)
	}
	for i, call := range wantCalls {
		if gotCalls[i].ID != call.ID || gotCalls[i].ToolID != call.ToolID || string(gotCalls[i].Arguments) != string(call.Arguments) {
			t.Fatalf("tool call %d = %#v, want %#v", i, gotCalls[i], call)
		}
	}
	if got.FinishReason != want.FinishReason {
		t.Fatalf("finish reason = %q, want %q", got.FinishReason, want.FinishReason)
	}
	if got.Usage != want.Usage {
		t.Fatalf("usage = %#v, want %#v", got.Usage, want.Usage)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("response validate = %v", err)
	}
}

func assertContractObservedRequest(t *testing.T, body []byte, want lebro.ModelRequest) {
	t.Helper()
	observed := decodeObservedRequest(t, body)
	if observed.Model != want.Model || len(observed.Messages) != len(want.Messages) {
		t.Fatalf("observed request = %#v, want %#v", observed, want)
	}
	if observed.Reasoning != want.Reasoning {
		t.Fatalf("observed reasoning = %#v, want %#v", observed.Reasoning, want.Reasoning)
	}
	for i, message := range observed.Messages {
		if message.Role != want.Messages[i].Role || message.Content != want.Messages[i].Content {
			t.Fatalf("observed message %d = %#v, want %#v", i, message, want.Messages[i])
		}
	}
	if len(observed.Tools) != len(want.Tools) {
		t.Fatalf("observed tools = %#v, want %#v", observed.Tools, want.Tools)
	}
	for i, tool := range want.Tools {
		if observed.Tools[i].ID != tool.ID {
			t.Fatalf("observed tool %d id = %q, want %q", i, observed.Tools[i].ID, tool.ID)
		}
	}
	if want.OutputSchema != nil {
		if observed.OutputSchema == nil {
			t.Fatal("observed response_format missing")
		}
		if observed.OutputSchema.Name != want.OutputSchema.Name || observed.OutputSchema.Strict != want.OutputSchema.Strict {
			t.Fatalf("observed output schema = %#v, want name %q strict %v", observed.OutputSchema, want.OutputSchema.Name, want.OutputSchema.Strict)
		}
		if got, want := normalizeJSON(t, string(observed.OutputSchema.Schema)), normalizeJSON(t, string(want.OutputSchema.Schema)); !reflect.DeepEqual(got, want) {
			t.Fatalf("observed schema = %v, want %v", got, want)
		}
	} else if observed.OutputSchema != nil {
		t.Fatalf("unexpected response_format = %#v", observed.OutputSchema)
	}
}

// normalizeJSON decodes JSON into plain Go values so comparisons are
// insensitive to key order introduced by wire round-trips.
func normalizeJSON(t *testing.T, raw string) any {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return value
}

func mapWireRole(role string) lebro.Role {
	switch role {
	case "system":
		return lebro.RoleSystem
	case "user":
		return lebro.RoleUser
	case "assistant":
		return lebro.RoleAssistant
	case "tool":
		return lebro.RoleTool
	default:
		return lebro.Role(role)
	}
}

func cloneRequest(request lebro.ModelRequest) lebro.ModelRequest {
	request.Messages = append([]lebro.Message(nil), request.Messages...)
	if request.OutputSchema != nil {
		schema := *request.OutputSchema
		schema.Schema = append(json.RawMessage(nil), request.OutputSchema.Schema...)
		request.OutputSchema = &schema
	}
	request.Tools = append([]lebro.ToolDefinition(nil), request.Tools...)
	request.Extension = append(json.RawMessage(nil), request.Extension...)
	return request
}

type chatWireRequest struct {
	Model          string            `json:"model"`
	Messages       []chatWireMessage `json:"messages"`
	Tools          []chatWireTool    `json:"tools,omitempty"`
	ResponseFormat map[string]any    `json:"response_format,omitempty"`
	Reasoning      map[string]any    `json:"reasoning,omitempty"`
}

type chatWireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatWireTool struct {
	Type     string               `json:"type"`
	Function chatWireToolFunction `json:"function"`
}

type chatWireToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}
