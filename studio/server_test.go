package studio_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
	"github.com/tesh254/lebro/obsv"
	"github.com/tesh254/lebro/studio"
)

// scriptedModel is a deterministic stand-in for a provider adapter, mirroring
// the http-server example. It echoes the last user message so a run has a
// predictable terminal output without a network call or an API key.
type scriptedModel struct{}

func (scriptedModel) Generate(_ context.Context, request lebro.ModelRequest) (lebro.ModelResponse, error) {
	return lebro.ModelResponse{
		Message:      lebro.Message{Role: lebro.RoleAssistant, Content: reply(request)},
		FinishReason: lebro.FinishReasonStop,
	}, nil
}

func (scriptedModel) Stream(_ context.Context, request lebro.ModelRequest) (lebro.StreamReader, error) {
	sent := false
	return &lebro.StreamReaderFunc{
		NextFn: func() (lebro.StreamDelta, error) {
			if sent {
				return lebro.StreamDelta{}, io.EOF
			}
			sent = true
			return lebro.StreamDelta{Text: reply(request), FinishReason: lebro.FinishReasonStop}, nil
		},
	}, nil
}

func reply(request lebro.ModelRequest) string {
	for i := len(request.Messages) - 1; i >= 0; i-- {
		if request.Messages[i].Role == lebro.RoleUser {
			return "You said: " + request.Messages[i].Content
		}
	}
	return "Hello."
}

// newStudio builds a Studio and fails the test on a construction error.
func newStudio(t *testing.T, config studio.Config) *studio.Studio {
	t.Helper()
	studioServer, err := studio.New(config)
	if err != nil {
		t.Fatalf("new studio: %v", err)
	}
	return studioServer
}

// getJSON drives a GET through the Studio handler and decodes a 200 body.
func getJSON(t *testing.T, studioServer *studio.Studio, path string, into any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	studioServer.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s: want 200, got %d (%s)", path, recorder.Code, recorder.Body.String())
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), into); err != nil {
		t.Fatalf("GET %s: decode: %v", path, err)
	}
}

// post drives a POST through the Studio handler and returns the recorder.
func post(t *testing.T, studioServer *studio.Studio, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	studioServer.Handler().ServeHTTP(recorder, request)
	return recorder
}

func newAgent(t *testing.T, listener lebro.RunListener, store lebro.Store) *lebro.Agent {
	t.Helper()
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "assistant", Name: "Assistant", Instructions: "Be concise."},
		Model:      scriptedModel{},
		Listener:   listener,
		Store:      store,
	})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	return agent
}

func newWorkflow(t *testing.T) *lebro.LinearWorkflow {
	t.Helper()
	workflow, err := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition:     lebro.WorkflowDefinition{ID: "greet", Name: "Greet", Version: "1"},
		SchemaCompiler: lebrojsonschema.NewCompiler(),
		Steps: []lebro.Step{{
			Definition: lebro.StepDefinition{
				ID: "greet",
				InputSchema: json.RawMessage(`{
					"type":"object",
					"required":["name"],
					"properties":{"name":{"type":"string"}},
					"additionalProperties":false
				}`),
			},
			Handler: lebro.StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
				var args struct {
					Name string `json:"name"`
				}
				if err := json.Unmarshal(input, &args); err != nil {
					return nil, err
				}
				return json.Marshal(map[string]string{"greeting": "Hello, " + args.Name + "!"})
			}),
		}},
	})
	if err != nil {
		t.Fatalf("new workflow: %v", err)
	}
	return workflow
}

func newObserver(t *testing.T, repo obsv.Repository) *obsv.Observer {
	t.Helper()
	// A negative QueueSize exports synchronously on the emitting goroutine, so a
	// span is queryable the moment the run returns — no drain race in the test.
	observer, err := obsv.New(obsv.Config{Repository: repo, QueueSize: -1})
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	t.Cleanup(func() { _ = observer.Close() })
	return observer
}

// TestAgentRunProducesInspectableOrderedEvents covers the first acceptance
// criterion: a developer runs an agent and inspects its ordered events. The run
// goes through the API mounted under /api, and the trace it records is then read
// back through the trace view in recorded order.
func TestAgentRunProducesInspectableOrderedEvents(t *testing.T) {
	repo := obsv.NewMemoryRepository()
	observer := newObserver(t, repo)
	agent := newAgent(t, observer, nil)

	studioServer := newStudio(t, studio.Config{Agents: []*lebro.Agent{agent}, Traces: repo})

	// Run the agent through the mounted API.
	recorder := post(t, studioServer, "/api/agents/assistant/runs", `{"messages":[{"content":"Hi there"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("agent run: want 200, got %d (%s)", recorder.Code, recorder.Body.String())
	}

	// The listing now shows the trace the run recorded.
	var list studio.TraceListResponse
	getJSON(t, studioServer, "/api/studio/traces", &list)
	if len(list.Traces) != 1 {
		t.Fatalf("want 1 recorded trace, got %d", len(list.Traces))
	}

	// The trace's spans are the ordered events. The first is the run root; a
	// model span follows, because the scripted model was called once.
	var trace studio.TraceResponse
	getJSON(t, studioServer, "/api/studio/traces/"+list.Traces[0].TraceID, &trace)
	if len(trace.Spans) < 2 {
		t.Fatalf("want at least run and model spans, got %d", len(trace.Spans))
	}
	if !trace.Spans[0].IsRoot() || trace.Spans[0].Kind != obsv.SpanKindRun {
		t.Fatalf("first span should be the run root, got kind %q root=%v", trace.Spans[0].Kind, trace.Spans[0].IsRoot())
	}
	sawModel := false
	for _, span := range trace.Spans {
		if span.Kind == obsv.SpanKindModel {
			sawModel = true
		}
	}
	if !sawModel {
		t.Fatalf("ordered events missing the model call span")
	}
}

// TestWorkflowRunWithCustomJSONInput covers the second acceptance criterion: a
// developer executes a workflow with custom JSON input through the UI's API.
func TestWorkflowRunWithCustomJSONInput(t *testing.T) {
	workflow := newWorkflow(t)
	studioServer := newStudio(t, studio.Config{Workflows: []*lebro.LinearWorkflow{workflow}})

	recorder := post(t, studioServer, "/api/workflows/greet/runs", `{"input":{"name":"Ada"}}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("workflow run: want 200, got %d (%s)", recorder.Code, recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("Hello, Ada!")) {
		t.Fatalf("workflow output missing greeting: %s", recorder.Body.String())
	}
}

// TestWorkflowRunRejectsInvalidCustomInput confirms the input is validated
// against the step schema before the run, including the JSON null literal — a
// decode of null into the step's object would otherwise slip past as an empty
// object rather than a validation error.
func TestWorkflowRunRejectsInvalidCustomInput(t *testing.T) {
	workflow := newWorkflow(t)
	studioServer := newStudio(t, studio.Config{Workflows: []*lebro.LinearWorkflow{workflow}})

	for _, body := range []string{
		`{"input":{"name":42}}`, // wrong type
		`{"input":null}`,        // null literal, not a valid object for a required field
		`{"input":{}}`,          // missing required field
	} {
		recorder := post(t, studioServer, "/api/workflows/greet/runs", body)
		if recorder.Code == http.StatusOK {
			t.Fatalf("invalid input %q was accepted", body)
		}
	}
}

// TestUIServedAtRootAsPlaceholderWithoutBundle confirms the UI shell is served
// at the root even when no bundle is embedded, so the tool is usable from a
// from-source build.
func TestUIServedAtRootAsPlaceholderWithoutBundle(t *testing.T) {
	studioServer := newStudio(t, studio.Config{})

	recorder := httptest.NewRecorder()
	studioServer.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("root: want 200, got %d", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "text/html; charset=utf-8" {
		t.Fatalf("root: want html, got %q", contentType)
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("lebro studio")) {
		t.Fatalf("root: placeholder missing title")
	}
}

// TestAPIMountedUnderAPIPrefix confirms the httpapi routes are reachable under
// /api, so the UI's client can find them, and that the bare API path does not
// shadow the UI root.
func TestAPIMountedUnderAPIPrefix(t *testing.T) {
	agent := newAgent(t, nil, nil)
	studioServer := newStudio(t, studio.Config{Agents: []*lebro.Agent{agent}})

	var list struct {
		Agents []struct {
			ID string `json:"id"`
		} `json:"agents"`
	}
	getJSON(t, studioServer, "/api/agents", &list)
	if len(list.Agents) != 1 || list.Agents[0].ID != "assistant" {
		t.Fatalf("want assistant agent listed under /api, got %+v", list.Agents)
	}
}

// TestStartServesThenShutsDownOnContextCancel exercises the explicit opt-in:
// Start binds a listener, serves, and stops cleanly when its context is
// cancelled. It also demonstrates the off-by-default property — nothing listens
// until Start is called.
func TestStartServesThenShutsDownOnContextCancel(t *testing.T) {
	agent := newAgent(t, nil, nil)

	// Bind an ephemeral port ourselves to learn the address, then hand it to
	// Start. Closing our probe listener first frees the port for Start.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- studio.Start(ctx, addr, studio.Config{Agents: []*lebro.Agent{agent}}) }()

	// Poll until the server answers, so the test does not race the goroutine's
	// bind.
	if !waitForServer(t, "http://"+addr+"/api/agents") {
		cancel()
		<-done
		t.Fatal("studio did not start serving")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Fatalf("Start returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after context cancel")
	}
}

func waitForServer(t *testing.T, url string) bool {
	t.Helper()
	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(url)
		if err == nil {
			_ = response.Body.Close()
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
