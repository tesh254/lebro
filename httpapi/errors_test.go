package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/httpapi"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

// assertErrorCode checks the public error body of a recorded response.
func assertErrorCode(t *testing.T, recorder *httptest.ResponseRecorder, want httpapi.ErrorCode) {
	t.Helper()
	response := decodeBody[httpapi.ErrorResponse](t, recorder)
	if response.Error.Code != want {
		t.Fatalf("error code = %q, want %q; body = %s", response.Error.Code, want, recorder.Body)
	}
	if response.Error.Message == "" {
		t.Fatal("error message is empty")
	}
}

// Each runtime failure mode must surface as its own status and public code, so
// a client can distinguish an upstream provider outage from its own bad input
// without parsing prose.
func TestRuntimeFailuresMapToTypedErrors(t *testing.T) {
	compiler := lebrojsonschema.NewCompiler()

	tests := map[string]struct {
		agent      func(*testing.T) *lebro.Agent
		wantStatus int
		wantCode   httpapi.ErrorCode
	}{
		"provider failure": {
			agent: func(t *testing.T) *lebro.Agent {
				return newAgent(t, "assistant", failingModel{kind: lebro.ModelErrorUnavailable})
			},
			wantStatus: http.StatusBadGateway,
			wantCode:   httpapi.ErrorCodeProviderFailure,
		},
		"step limit exhausted": {
			agent: func(t *testing.T) *lebro.Agent {
				// A model that only ever requests tools never terminates, so
				// the loop consumes its budget.
				calls, err := lebro.NewModelToolCalls(lebro.ModelToolCall{
					ID: "call-1", ToolID: "echo", Arguments: json.RawMessage(`{"value":"x"}`),
				})
				must(t, err)
				registry := mustValue(lebro.NewToolRegistry(compiler))
				must(t, registry.Register(echoTool{}))
				return newAgentWithConfig(t, lebro.AgentConfig{
					Definition: lebro.AgentDefinition{ID: "assistant", Tools: []lebro.ToolID{"echo"}},
					Tools:      registry,
					MaxSteps:   2,
					Model: modelFunc(func(context.Context, lebro.ModelRequest) (lebro.ModelResponse, error) {
						return lebro.ModelResponse{
							Message:      lebro.Message{Role: lebro.RoleAssistant, ToolCalls: calls},
							FinishReason: lebro.FinishReasonToolCalls,
						}, nil
					}),
				})
			},
			wantStatus: http.StatusBadGateway,
			wantCode:   httpapi.ErrorCodeStepLimitExhausted,
		},
		"tool failure": {
			agent: func(t *testing.T) *lebro.Agent {
				calls, err := lebro.NewModelToolCalls(lebro.ModelToolCall{
					ID: "call-1", ToolID: "boom", Arguments: json.RawMessage(`{}`),
				})
				must(t, err)
				registry := mustValue(lebro.NewToolRegistry(compiler))
				must(t, registry.Register(failingTool{}))
				return newAgentWithConfig(t, lebro.AgentConfig{
					Definition: lebro.AgentDefinition{ID: "assistant", Tools: []lebro.ToolID{"boom"}},
					Tools:      registry,
					Model: modelFunc(func(context.Context, lebro.ModelRequest) (lebro.ModelResponse, error) {
						return lebro.ModelResponse{
							Message:      lebro.Message{Role: lebro.RoleAssistant, ToolCalls: calls},
							FinishReason: lebro.FinishReasonToolCalls,
						}, nil
					}),
				})
			},
			wantStatus: http.StatusInternalServerError,
			wantCode:   httpapi.ErrorCodeToolFailure,
		},
		"unknown tool": {
			agent: func(t *testing.T) *lebro.Agent {
				registry := mustValue(lebro.NewToolRegistry(compiler))
				must(t, registry.Register(echoTool{}))
				return newAgentWithConfig(t, lebro.AgentConfig{
					// The definition references a tool that was never
					// registered, so resolution fails at run start.
					Definition: lebro.AgentDefinition{ID: "assistant", Tools: []lebro.ToolID{"absent"}},
					Tools:      registry,
					Model:      &scriptedModel{responses: []lebro.ModelResponse{textResponse("unused")}},
				})
			},
			wantStatus: http.StatusNotFound,
			wantCode:   httpapi.ErrorCodeNotFound,
		},
		"invalid structured output": {
			agent: func(t *testing.T) *lebro.Agent {
				return newAgentWithConfig(t, lebro.AgentConfig{
					Definition:     lebro.AgentDefinition{ID: "assistant"},
					SchemaCompiler: compiler,
					OutputSchema: &lebro.ModelOutputSchema{
						Schema: json.RawMessage(`{"type":"object","required":["answer"],"properties":{"answer":{"type":"string"}}}`),
					},
					Model: modelFunc(func(context.Context, lebro.ModelRequest) (lebro.ModelResponse, error) {
						return lebro.ModelResponse{
							Message: lebro.Message{
								Role:             lebro.RoleAssistant,
								StructuredOutput: lebro.NewModelStructuredOutput(json.RawMessage(`{"wrong":1}`)),
							},
							FinishReason: lebro.FinishReasonStop,
						}, nil
					}),
				})
			},
			wantStatus: http.StatusBadGateway,
			wantCode:   httpapi.ErrorCodeInvalidOutput,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			server := httpapi.NewServer(httpapi.ServerConfig{})
			must(t, server.ExposeAgent(test.agent(t)))

			recorder := doJSON(t, server.Handler(), http.MethodPost, "/agents/assistant/runs", httpapi.RunRequest{
				Messages: []httpapi.MessageInput{{Content: "go"}},
			})
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, test.wantStatus, recorder.Body)
			}
			assertErrorCode(t, recorder, test.wantCode)
		})
	}
}

func TestWorkflowStepFailureMapsToStepFailure(t *testing.T) {
	workflow := mustValue(lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition: lebro.WorkflowDefinition{ID: "boom"},
		Steps: []lebro.Step{{
			Definition: lebro.StepDefinition{ID: "explode"},
			Handler: lebro.StepHandlerFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
				return nil, errAlwaysFails
			}),
		}},
	}))

	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeWorkflow(workflow))

	recorder := doRaw(t, server.Handler(), http.MethodPost, "/workflows/boom/runs", `{"input":{}}`)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusInternalServerError, recorder.Body)
	}
	assertErrorCode(t, recorder, httpapi.ErrorCodeStepFailure)
}

// The public body must never carry internal error text: a handler error can
// contain connection strings, file paths, or prompt content.
func TestErrorBodiesDoNotLeakInternalDetail(t *testing.T) {
	const secret = "postgres://user:hunter2@db.internal/prod"
	workflow := mustValue(lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition: lebro.WorkflowDefinition{ID: "leaky"},
		Steps: []lebro.Step{{
			Definition: lebro.StepDefinition{ID: "leak"},
			Handler: lebro.StepHandlerFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
				return nil, errorWithMessage(secret)
			}),
		}},
	}))

	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeWorkflow(workflow))

	recorder := doRaw(t, server.Handler(), http.MethodPost, "/workflows/leaky/runs", `{"input":{}}`)
	if body := recorder.Body.String(); strings.Contains(body, secret) || strings.Contains(body, "hunter2") {
		t.Fatalf("response body leaked internal detail: %s", body)
	}
	assertErrorCode(t, recorder, httpapi.ErrorCodeStepFailure)
}

var errAlwaysFails = errorWithMessage("step handler failed")

type stringError string

func (e stringError) Error() string { return string(e) }

func errorWithMessage(message string) error { return stringError(message) }

// echoTool returns its input unchanged.
type echoTool struct{}

func (echoTool) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{
		ID:          "echo",
		Description: "Echo the input",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`),
	}
}

func (echoTool) Execute(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
	return input, nil
}

// failingTool always returns an error.
type failingTool struct{}

func (failingTool) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{
		ID:          "boom",
		Description: "Always fails",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
}

func (failingTool) Execute(context.Context, json.RawMessage) (json.RawMessage, error) {
	return nil, errAlwaysFails
}
