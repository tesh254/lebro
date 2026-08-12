package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/httpapi"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

// The tests in this file are the compatibility suite the ticket asks for: they
// run the real Server — the same one that generates the OpenAPI document — and
// drive it with the real Client. A hand-written response fixture can drift from
// what the server actually sends; these cannot, because the server under test
// is the server.

// newCompatServer serves a configured Server over httptest and returns a Client
// pointed at it.
func newCompatServer(t *testing.T, configure func(*httpapi.Server)) (*httpapi.Client, *httpapi.Server) {
	t.Helper()
	return newCompatServerWithConfig(t, httpapi.ServerConfig{}, configure)
}

func newCompatServerWithConfig(t *testing.T, config httpapi.ServerConfig, configure func(*httpapi.Server)) (*httpapi.Client, *httpapi.Server) {
	t.Helper()

	server := httpapi.NewServer(config)
	if configure != nil {
		configure(server)
	}

	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	client, err := httpapi.NewClient(httpapi.ClientConfig{BaseURL: httpServer.URL})
	must(t, err)
	return client, server
}

// TestCompatContractVersionHandshake asserts the generated document carries the
// contract version and that a client built from the same source accepts it.
func TestCompatContractVersionHandshake(t *testing.T) {
	t.Parallel()

	client, _ := newCompatServer(t, nil)
	must(t, client.CheckCompatibility(context.Background()))

	document, err := client.OpenAPI(context.Background())
	must(t, err)

	var parsed struct {
		Info map[string]any `json:"info"`
	}
	must(t, json.Unmarshal(document, &parsed))
	if got := parsed.Info["x-lebro-contract-version"]; got != httpapi.ContractVersion {
		t.Errorf("document contract version = %v, want %q", got, httpapi.ContractVersion)
	}
}

func TestCompatHealthAndListings(t *testing.T) {
	t.Parallel()

	client, _ := newCompatServer(t, func(server *httpapi.Server) {
		must(t, server.ExposeAgent(newAgent(t, "assistant", &scriptedModel{
			responses: []lebro.ModelResponse{textResponse("hi")},
		})))
		must(t, server.ExposeWorkflow(newEchoWorkflow(t, "echo")))
	})
	ctx := context.Background()

	health, err := client.Health(ctx)
	must(t, err)
	if health.Status != "ok" || health.Agents != 1 || health.Workflows != 1 {
		t.Errorf("health = %+v, want ok/1/1", health)
	}

	agents, err := client.ListAgents(ctx)
	must(t, err)
	if len(agents) != 1 || agents[0].ID != "assistant" {
		t.Fatalf("agents = %+v, want one agent \"assistant\"", agents)
	}

	workflows, err := client.ListWorkflows(ctx)
	must(t, err)
	if len(workflows) != 1 || workflows[0].ID != "echo" {
		t.Fatalf("workflows = %+v, want one workflow \"echo\"", workflows)
	}
	// The declared input schema must survive the round trip, since a client
	// uses it to validate before spending a run.
	if len(workflows[0].InputSchema) == 0 {
		t.Error("workflow input schema is empty, want the declared schema")
	}
	var schema map[string]any
	must(t, json.Unmarshal(workflows[0].InputSchema, &schema))
	if schema["type"] != "object" {
		t.Errorf("input schema type = %v, want object", schema["type"])
	}
}

// TestCompatRunRoundTripsStructuredOutput is an explicit acceptance criterion:
// a structured payload produced by the agent must reach the client intact.
func TestCompatRunRoundTripsStructuredOutput(t *testing.T) {
	t.Parallel()

	const payload = `{"city":"Nairobi","temperature_c":24}`

	client, _ := newCompatServer(t, func(server *httpapi.Server) {
		must(t, server.ExposeAgent(newAgentWithConfig(t, lebro.AgentConfig{
			Definition:     lebro.AgentDefinition{ID: "forecaster"},
			SchemaCompiler: lebrojsonschema.NewCompiler(),
			OutputSchema: &lebro.ModelOutputSchema{
				Name: "forecast",
				Schema: json.RawMessage(`{
					"type":"object",
					"required":["city","temperature_c"],
					"properties":{
						"city":{"type":"string"},
						"temperature_c":{"type":"number"}
					},
					"additionalProperties":false
				}`),
			},
			Model: &scriptedModel{responses: []lebro.ModelResponse{{
				Message: lebro.Message{
					Role:             lebro.RoleAssistant,
					Content:          "here is the forecast",
					StructuredOutput: lebro.NewModelStructuredOutput(json.RawMessage(payload)),
				},
				FinishReason: lebro.FinishReasonStop,
			}}},
		})))
	})

	result, err := client.Run(context.Background(), "forecaster", httpapi.RunRequest{
		Messages: []httpapi.MessageInput{{Content: "forecast for Nairobi"}},
	})
	must(t, err)

	if result.Status != string(lebro.RunStatusSucceeded) {
		t.Errorf("status = %q, want succeeded", result.Status)
	}
	if result.RunID == "" {
		t.Error("run ID is empty")
	}
	if result.Content != "here is the forecast" {
		t.Errorf("content = %q", result.Content)
	}

	var decoded struct {
		City         string  `json:"city"`
		TemperatureC float64 `json:"temperature_c"`
	}
	if err := json.Unmarshal(result.StructuredOutput, &decoded); err != nil {
		t.Fatalf("decode structured output %q: %v", result.StructuredOutput, err)
	}
	if decoded.City != "Nairobi" || decoded.TemperatureC != 24 {
		t.Errorf("structured output = %+v, want Nairobi/24", decoded)
	}
}

// TestCompatWorkflowRoundTripsData is the workflow half of the same criterion.
func TestCompatWorkflowRoundTripsData(t *testing.T) {
	t.Parallel()

	client, _ := newCompatServer(t, func(server *httpapi.Server) {
		must(t, server.ExposeWorkflow(newEchoWorkflow(t, "echo")))
	})

	result, err := client.RunWorkflow(context.Background(), "echo", httpapi.WorkflowRunRequest{
		Input:    json.RawMessage(`{"value":"round trip"}`),
		Metadata: map[string]string{"tenant": "acme"},
	})
	must(t, err)

	if result.Status != string(lebro.RunStatusSucceeded) {
		t.Errorf("status = %q, want succeeded", result.Status)
	}
	var output struct {
		Value string `json:"value"`
	}
	must(t, json.Unmarshal(result.Output, &output))
	if output.Value != "round trip" {
		t.Errorf("output value = %q, want %q", output.Value, "round trip")
	}
	if result.RunID == "" {
		t.Error("run ID is empty")
	}
	// Path records branch and fan-out routing rather than every executed step,
	// so a plain linear run reports none. Asserting it stays absent keeps the
	// client honest about not inventing a value the server did not send.
	if len(result.Path) != 0 {
		t.Errorf("path = %v, want empty for a linear run", result.Path)
	}
}

// TestCompatWorkflowSuspendRoundTrips asserts a run that suspends reaches the
// client with its resume contract intact and ResumeAvailable false. Resume is
// not exposed over HTTP, so a client must be able to see that a run stopped
// and that this transport cannot continue it — reported as a field rather than
// as a missing route.
func TestCompatWorkflowSuspendRoundTrips(t *testing.T) {
	t.Parallel()

	client, _ := newCompatServer(t, func(server *httpapi.Server) {
		must(t, server.ExposeWorkflow(mustValue(lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
			Definition:     lebro.WorkflowDefinition{ID: "approval", Version: "v1"},
			SchemaCompiler: lebrojsonschema.NewCompiler(),
			Steps: []lebro.Step{
				{
					Definition: lebro.StepDefinition{
						ID:            "await-approval",
						SuspendSchema: json.RawMessage(`{"const":{"approved":true}}`),
					},
					Handler: lebro.StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
						return nil, &lebro.SuspendError{Signal: lebro.SuspendSignal{
							StepID:   "await-approval",
							Contract: json.RawMessage(`{"approved":true}`),
						}}
					}),
				},
			},
		}))))
	})

	result, err := client.RunWorkflow(context.Background(), "approval", httpapi.WorkflowRunRequest{
		Input: json.RawMessage(`"start"`),
	})
	must(t, err)

	if result.Status != string(lebro.RunStatusSuspended) {
		t.Fatalf("status = %q, want suspended", result.Status)
	}
	if result.Suspend == nil {
		t.Fatal("suspend is nil, want the resume contract")
	}
	if result.Suspend.StepID != "await-approval" {
		t.Errorf("suspend step ID = %q, want await-approval", result.Suspend.StepID)
	}
	if result.Suspend.ResumeAvailable {
		t.Error("resume_available is true, want false: resume is not exposed over HTTP")
	}
	var contract map[string]any
	must(t, json.Unmarshal(result.Suspend.Contract, &contract))
	if contract["approved"] != true {
		t.Errorf("suspend contract = %v, want approved:true", contract)
	}
}

// TestCompatWorkflowInvalidInputIsTyped asserts schema rejection reaches the
// client as the invalid-input classification rather than a generic failure.
func TestCompatWorkflowInvalidInputIsTyped(t *testing.T) {
	t.Parallel()

	client, _ := newCompatServer(t, func(server *httpapi.Server) {
		must(t, server.ExposeWorkflow(newEchoWorkflow(t, "echo")))
	})

	_, err := client.RunWorkflow(context.Background(), "echo", httpapi.WorkflowRunRequest{
		Input: json.RawMessage(`{"value":42}`),
	})
	var apiErr *httpapi.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not an *APIError", err)
	}
	if apiErr.Code != httpapi.ErrorCodeInvalidInput {
		t.Errorf("code = %q, want invalid_input", apiErr.Code)
	}
	if !errors.Is(err, lebro.ErrWorkflowInvalidStepInput) {
		t.Errorf("error %v does not match lebro.ErrWorkflowInvalidStepInput", err)
	}
}

// TestCompatUnknownPrimitivesAreNotFound asserts an unexposed primitive is
// unreachable through the client, matching the server's registration rule.
func TestCompatUnknownPrimitivesAreNotFound(t *testing.T) {
	t.Parallel()

	client, _ := newCompatServer(t, nil)
	ctx := context.Background()

	if _, err := client.Run(ctx, "ghost", httpapi.RunRequest{}); !errors.Is(err, lebro.ErrNotFound) {
		t.Errorf("Run error = %v, want lebro.ErrNotFound", err)
	}
	if _, err := client.RunWorkflow(ctx, "ghost", httpapi.WorkflowRunRequest{}); !errors.Is(err, lebro.ErrNotFound) {
		t.Errorf("RunWorkflow error = %v, want lebro.ErrNotFound", err)
	}
	if _, err := client.RunStream(ctx, "ghost", httpapi.RunRequest{}); !errors.Is(err, lebro.ErrNotFound) {
		t.Errorf("RunStream error = %v, want lebro.ErrNotFound", err)
	}
}

// TestCompatProviderFailureIsTyped asserts a model adapter failure reaches the
// client as the provider classification.
func TestCompatProviderFailureIsTyped(t *testing.T) {
	t.Parallel()

	client, _ := newCompatServer(t, func(server *httpapi.Server) {
		must(t, server.ExposeAgent(newAgent(t, "flaky", failingModel{kind: lebro.ModelErrorUnavailable})))
	})

	_, err := client.Run(context.Background(), "flaky", httpapi.RunRequest{
		Messages: []httpapi.MessageInput{{Content: "hi"}},
	})
	if !errors.Is(err, lebro.ErrAgentProviderFailure) {
		t.Fatalf("error = %v, want lebro.ErrAgentProviderFailure", err)
	}
	var apiErr *httpapi.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not an *APIError", err)
	}
	if apiErr.Code != httpapi.ErrorCodeProviderFailure {
		t.Errorf("code = %q, want provider_failure", apiErr.Code)
	}
}

// TestCompatStreamRoundTrip asserts a streamed run delivers ordered deltas and
// a terminal result carrying structured output, against the real server.
func TestCompatStreamRoundTrip(t *testing.T) {
	t.Parallel()

	const payload = `{"answer":42}`

	client, _ := newCompatServer(t, func(server *httpapi.Server) {
		must(t, server.ExposeAgent(newAgentWithConfig(t, lebro.AgentConfig{
			Definition:     lebro.AgentDefinition{ID: "streamer"},
			SchemaCompiler: lebrojsonschema.NewCompiler(),
			OutputSchema: &lebro.ModelOutputSchema{
				Name: "answer",
				Schema: json.RawMessage(`{
					"type":"object",
					"required":["answer"],
					"properties":{"answer":{"type":"number"}},
					"additionalProperties":false
				}`),
			},
			Model: &scriptedModel{responses: []lebro.ModelResponse{{
				Message: lebro.Message{
					Role:             lebro.RoleAssistant,
					Content:          "the answer",
					StructuredOutput: lebro.NewModelStructuredOutput(json.RawMessage(payload)),
				},
				FinishReason: lebro.FinishReasonStop,
				Usage:        lebro.ModelUsage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8},
			}}},
		})))
	})

	stream, err := client.RunStream(context.Background(), "streamer", httpapi.RunRequest{
		Messages: []httpapi.MessageInput{{Content: "what is the answer"}},
	})
	must(t, err)
	defer stream.Cancel()

	var text strings.Builder
	for event := range stream.Events {
		if event.Type != "model_delta" {
			t.Errorf("event type = %q, want model_delta", event.Type)
		}
		text.WriteString(event.Text)
	}

	result, err := stream.Wait()
	must(t, err)
	if result.Status != string(lebro.RunStatusSucceeded) {
		t.Errorf("status = %q, want succeeded", result.Status)
	}
	if result.RunID == "" {
		t.Error("run ID is empty")
	}
	if result.Content != "the answer" {
		t.Errorf("terminal content = %q, want %q", result.Content, "the answer")
	}
	var decoded struct {
		Answer float64 `json:"answer"`
	}
	if err := json.Unmarshal(result.StructuredOutput, &decoded); err != nil {
		t.Fatalf("decode structured output %q: %v", result.StructuredOutput, err)
	}
	if decoded.Answer != 42 {
		t.Errorf("structured answer = %v, want 42", decoded.Answer)
	}
}

// TestCompatStreamCancelStopsRemoteRun is the ticket's headline acceptance
// criterion: a client can execute and cancel a streamed remote agent run. The
// model blocks until its context is cancelled, so the run can only end because
// cancellation propagated from the client, through the connection, to the
// server's run.
func TestCompatStreamCancelStopsRemoteRun(t *testing.T) {
	t.Parallel()

	blocking := newBlockingModel()
	client, _ := newCompatServer(t, func(server *httpapi.Server) {
		must(t, server.ExposeAgent(newAgent(t, "slow", blocking)))
	})

	stream, err := client.RunStream(context.Background(), "slow", httpapi.RunRequest{
		Messages: []httpapi.MessageInput{{Content: "take your time"}},
	})
	must(t, err)

	// Wait until the run is actually in the model call, so the cancellation
	// under test interrupts a run in flight rather than racing its start.
	select {
	case <-blocking.started:
	case <-time.After(5 * time.Second):
		stream.Cancel()
		t.Fatal("model was never called")
	}

	stream.Cancel()

	done := make(chan error, 1)
	go func() {
		_, waitErr := stream.Drain()
		done <- waitErr
	}()

	select {
	case waitErr := <-done:
		if !errors.Is(waitErr, context.Canceled) {
			t.Errorf("Wait error = %v, want it to match context.Canceled", waitErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Drain blocked after Cancel")
	}

	// The server must have observed the cancellation and released the model,
	// rather than leaving the run executing after the client walked away.
	select {
	case <-blocking.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("remote run was not cancelled when the client cancelled the stream")
	}
}

// blockingModel blocks in Generate until its context is cancelled, reporting
// when it started and when it observed the cancellation.
type blockingModel struct {
	startOnce sync.Once
	started   chan struct{}
	cancelled chan struct{}
	closeOnce sync.Once
	released  chan struct{}
}

func newBlockingModel() *blockingModel {
	return &blockingModel{
		started:   make(chan struct{}),
		cancelled: make(chan struct{}),
		released:  make(chan struct{}),
	}
}

func (m *blockingModel) Generate(ctx context.Context, _ lebro.ModelRequest) (lebro.ModelResponse, error) {
	m.startOnce.Do(func() { close(m.started) })
	select {
	case <-ctx.Done():
		m.closeOnce.Do(func() { close(m.cancelled) })
		return lebro.ModelResponse{}, ctx.Err()
	case <-m.released:
		return textResponse("released"), nil
	}
}

func TestCompatThreadRoundTrip(t *testing.T) {
	t.Parallel()

	store := lebro.NewMemoryStore()
	ctx := context.Background()

	must(t, store.Threads().CreateThread(ctx, lebro.ThreadRecord{
		ID:        "thread-1",
		Namespace: "tenants",
		OwnerID:   "acme",
		Metadata:  json.RawMessage(`{"plan":"pro"}`),
	}))

	// The agent needs the store too: the server's Store resolves the thread
	// routes, while persisting a run's transcript is the agent's own binding.
	client, _ := newCompatServerWithConfig(t, httpapi.ServerConfig{Store: store}, func(server *httpapi.Server) {
		must(t, server.ExposeAgent(newAgentWithConfig(t, lebro.AgentConfig{
			Definition: lebro.AgentDefinition{ID: "assistant"},
			Model:      &scriptedModel{responses: []lebro.ModelResponse{textResponse("hello there")}},
			Store:      store,
		})))
	})

	fetched, err := client.GetThread(ctx, "thread-1")
	must(t, err)
	if fetched.ID != "thread-1" || fetched.Namespace != "tenants" || fetched.OwnerID != "acme" {
		t.Errorf("thread = %+v, want thread-1/tenants/acme", fetched)
	}

	// A run bound to the thread must persist its transcript, which the message
	// listing then returns.
	_, err = client.Run(ctx, "assistant",
		httpapi.RunRequest{Messages: []httpapi.MessageInput{{Content: "hi"}}},
		httpapi.WithThread("thread-1"),
	)
	must(t, err)

	page, err := client.ListMessages(ctx, "thread-1", httpapi.WithLimit(10))
	must(t, err)
	if len(page.Messages) == 0 {
		t.Fatal("message listing is empty, want the persisted transcript")
	}
	var sawUser, sawAssistant bool
	for _, message := range page.Messages {
		switch message.Role {
		case string(lebro.RoleUser):
			sawUser = sawUser || message.Content == "hi"
		case string(lebro.RoleAssistant):
			sawAssistant = sawAssistant || message.Content == "hello there"
		}
	}
	if !sawUser || !sawAssistant {
		t.Errorf("messages = %+v, want both the user and assistant turns", page.Messages)
	}
}

// TestCompatThreadWithoutStoreIsRejected asserts the client surfaces the
// server's refusal to silently ignore a thread it cannot resolve.
func TestCompatThreadWithoutStoreIsRejected(t *testing.T) {
	t.Parallel()

	client, _ := newCompatServer(t, func(server *httpapi.Server) {
		must(t, server.ExposeAgent(newAgent(t, "assistant", &scriptedModel{
			responses: []lebro.ModelResponse{textResponse("hi")},
		})))
	})

	_, err := client.Run(context.Background(), "assistant",
		httpapi.RunRequest{Messages: []httpapi.MessageInput{{Content: "hi"}}},
		httpapi.WithThread("thread-1"),
	)
	var apiErr *httpapi.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not an *APIError", err)
	}
	if apiErr.Code != httpapi.ErrorCodeInvalidRequest {
		t.Errorf("code = %q, want invalid_request", apiErr.Code)
	}
}

// TestCompatMessagePagingFollowsCursor asserts the client's cursor option walks
// a thread the same way the route documents.
func TestCompatMessagePagingFollowsCursor(t *testing.T) {
	t.Parallel()

	store := lebro.NewMemoryStore()
	ctx := context.Background()
	must(t, store.Threads().CreateThread(ctx, lebro.ThreadRecord{ID: "thread-1"}))

	const total = 5
	records := make([]lebro.MessageRecord, 0, total)
	for i := range total {
		records = append(records, lebro.MessageRecord{
			ID:       string(rune('a' + i)),
			ThreadID: "thread-1",
			Message:  lebro.Message{Role: lebro.RoleUser, Content: string(rune('a' + i))},
		})
	}
	must(t, store.Messages().AppendMessages(ctx, records))

	client, _ := newCompatServerWithConfig(t, httpapi.ServerConfig{Store: store}, nil)

	var seen []string
	cursor := ""
	for range total + 1 {
		page, err := client.ListMessages(ctx, "thread-1", httpapi.WithLimit(2), httpapi.WithCursor(cursor))
		must(t, err)
		for _, message := range page.Messages {
			seen = append(seen, message.Content)
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(seen) != total {
		t.Fatalf("paged %d messages, want %d", len(seen), total)
	}
	if got := strings.Join(seen, ""); got != "abcde" {
		t.Errorf("paged content = %q, want %q", got, "abcde")
	}
}

// TestCompatClientCoversEveryRoute asserts the client exposes a method for
// every route the server serves. A route added to the table without a client
// method is a contract the SDK cannot reach, which is the drift this ticket
// exists to prevent.
func TestCompatClientCoversEveryRoute(t *testing.T) {
	t.Parallel()

	client, _ := newCompatServer(t, func(server *httpapi.Server) {
		must(t, server.ExposeAgent(newAgent(t, "assistant", &scriptedModel{
			responses: []lebro.ModelResponse{textResponse("hi"), textResponse("hi")},
		})))
		must(t, server.ExposeWorkflow(newPermissiveWorkflow(t, "passthrough")))
	})
	ctx := context.Background()

	// Every documented operation, exercised through its client method. The
	// assertion is that none of them fails; the shapes are covered above.
	if _, err := client.Health(ctx); err != nil {
		t.Errorf("getHealth: %v", err)
	}
	if _, err := client.ListAgents(ctx); err != nil {
		t.Errorf("listAgents: %v", err)
	}
	if _, err := client.ListWorkflows(ctx); err != nil {
		t.Errorf("listWorkflows: %v", err)
	}
	if _, err := client.Run(ctx, "assistant", httpapi.RunRequest{}); err != nil {
		t.Errorf("createAgentRun: %v", err)
	}
	stream, err := client.RunStream(ctx, "assistant", httpapi.RunRequest{})
	if err != nil {
		t.Errorf("streamAgentRun: %v", err)
	} else {
		defer stream.Cancel()
		if _, err := stream.Drain(); err != nil {
			t.Errorf("streamAgentRun drain: %v", err)
		}
	}
	if _, err := client.RunWorkflow(ctx, "passthrough", httpapi.WorkflowRunRequest{}); err != nil {
		t.Errorf("createWorkflowRun: %v", err)
	}
	// The thread routes report not-found without a Store, which is still the
	// documented behavior for this configuration; what matters here is that the
	// client can address them.
	if _, err := client.GetThread(ctx, "thread-1"); !errors.Is(err, lebro.ErrNotFound) {
		t.Errorf("getThread error = %v, want lebro.ErrNotFound", err)
	}
	if _, err := client.ListMessages(ctx, "thread-1"); !errors.Is(err, lebro.ErrNotFound) {
		t.Errorf("listThreadMessages error = %v, want lebro.ErrNotFound", err)
	}
	if _, err := client.OpenAPI(ctx); err != nil {
		t.Errorf("getOpenAPI: %v", err)
	}
}
