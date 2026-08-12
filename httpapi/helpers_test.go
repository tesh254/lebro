package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// mustValue unwraps a (value, error) pair, panicking on error.
//
// It takes no *testing.T because Go permits forwarding a multi-value call only
// when it is the sole argument: mustValue(lebro.NewToolRegistry(c)) compiles,
// while a leading t parameter would not. The panic surfaces as a test failure
// with a full stack, which is enough for a constructor that is not the subject
// of the test.
func mustValue[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

// scriptedModel returns a fixed sequence of responses, one per Generate call,
// so a test can drive a multi-step agent loop deterministically. Exhausting the
// script is an error rather than a repeat of the last response, so a test that
// loops more than it intended fails instead of hanging.
type scriptedModel struct {
	responses []lebro.ModelResponse
	calls     int
}

func (m *scriptedModel) Generate(_ context.Context, _ lebro.ModelRequest) (lebro.ModelResponse, error) {
	if m.calls >= len(m.responses) {
		return lebro.ModelResponse{}, errors.New("scripted model exhausted")
	}
	response := m.responses[m.calls]
	m.calls++
	return response, nil
}

// textResponse is a terminal assistant response carrying text.
func textResponse(text string) lebro.ModelResponse {
	return lebro.ModelResponse{
		Message:      lebro.Message{Role: lebro.RoleAssistant, Content: text},
		FinishReason: lebro.FinishReasonStop,
		Usage:        lebro.ModelUsage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8},
	}
}

// failingModel reports a fixed model error on every call.
type failingModel struct {
	kind lebro.ModelErrorKind
}

func (m failingModel) Generate(_ context.Context, _ lebro.ModelRequest) (lebro.ModelResponse, error) {
	return lebro.ModelResponse{}, &lebro.ModelError{
		Kind:     m.kind,
		Provider: "test",
		Message:  "provider is unavailable",
	}
}

// newAgent builds an agent with the given ID over a model.
func newAgent(t *testing.T, id string, model lebro.Model) *lebro.Agent {
	t.Helper()
	return newAgentWithConfig(t, lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: lebro.AgentID(id), Name: id},
		Model:      model,
	})
}

func newAgentWithConfig(t *testing.T, config lebro.AgentConfig) *lebro.Agent {
	t.Helper()
	return mustValue(lebro.NewAgent(config))
}

// newEchoWorkflow builds a single-step workflow that echoes its input, with a
// declared input schema so validation is exercised.
func newEchoWorkflow(t *testing.T, id string) *lebro.LinearWorkflow {
	t.Helper()
	return mustValue(lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition:     lebro.WorkflowDefinition{ID: lebro.WorkflowID(id), Name: id, Version: "1"},
		SchemaCompiler: lebrojsonschema.NewCompiler(),
		Steps: []lebro.Step{
			{
				Definition: lebro.StepDefinition{
					ID: "echo",
					InputSchema: json.RawMessage(`{
						"type":"object",
						"required":["value"],
						"properties":{"value":{"type":"string"}},
						"additionalProperties":false
					}`),
				},
				Handler: lebro.StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
					return input, nil
				}),
			},
		},
	}))
}

// newPermissiveWorkflow builds a single-step workflow with no declared input
// schema, so it accepts any input including none at all.
func newPermissiveWorkflow(t *testing.T, id string) *lebro.LinearWorkflow {
	t.Helper()
	return mustValue(lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition: lebro.WorkflowDefinition{ID: lebro.WorkflowID(id), Name: id},
		Steps: []lebro.Step{
			{
				Definition: lebro.StepDefinition{ID: "passthrough"},
				Handler: lebro.StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
					if len(input) == 0 {
						return json.RawMessage(`{"received":null}`), nil
					}
					return input, nil
				}),
			},
		},
	}))
}

// doJSON issues a request against a handler and returns the recorded response.
func doJSON(t *testing.T, handler http.Handler, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		must(t, err)
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, target, reader)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// doRaw issues a request with a literal body, for malformed-input cases that
// cannot be produced by marshalling a Go value.
func doRaw(t *testing.T, handler http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, bytes.NewReader([]byte(body)))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// decodeBody decodes a recorded JSON response body.
func decodeBody[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode response body %q: %v", recorder.Body.String(), err)
	}
	return value
}
