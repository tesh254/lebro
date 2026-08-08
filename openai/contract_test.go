package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
)

// TestOpenAIAdapterPassesProviderContract runs the contract cases that a
// text-generation adapter owns (text, failure, cancellation) against a
// recorded HTTP transport. Tool-call and structured-output cases are deferred
// to the richer adapters tracked by MAD-17 and MAD-19.
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
	case "text", "failure", "cancellation":
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

func serveContractCase(t *testing.T, w http.ResponseWriter, contractCase testkit.ProviderCase) {
	t.Helper()
	switch contractCase.Mode {
	case testkit.ContractResponse:
		writeJSON(t, w, http.StatusOK, chatResponse{
			ID: "chatcmpl-contract", Model: contractCase.Request.Model,
			Choices: []chatChoice{{
				Message:      chatChoiceMessage{Role: "assistant", Content: json.RawMessage(`"` + contractCase.Response.Message.Content + `"`)},
				FinishReason: string(contractCase.Response.FinishReason),
			}},
			Usage: chatUsageBody{
				PromptTokens:     contractCase.Response.Usage.InputTokens,
				CompletionTokens: contractCase.Response.Usage.OutputTokens,
				TotalTokens:      contractCase.Response.Usage.TotalTokens,
			},
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
	return lebro.ModelRequest{Model: parsed.Model, Messages: messages}
}

func assertContractResponse(t *testing.T, got, want lebro.ModelResponse) {
	t.Helper()
	if got.Message.Role != want.Message.Role {
		t.Fatalf("role = %q, want %q", got.Message.Role, want.Message.Role)
	}
	if got.Message.Content != want.Message.Content {
		t.Fatalf("content = %q, want %q", got.Message.Content, want.Message.Content)
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
	for i, message := range observed.Messages {
		if message.Role != want.Messages[i].Role || message.Content != want.Messages[i].Content {
			t.Fatalf("observed message %d = %#v, want %#v", i, message, want.Messages[i])
		}
	}
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
	Model    string            `json:"model"`
	Messages []chatWireMessage `json:"messages"`
}

type chatWireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
