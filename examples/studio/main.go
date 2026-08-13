// studio demonstrates the local Studio-style developer UI: exposing an agent
// and a workflow, running them, and inspecting the ordered events a run
// records — without writing a one-off debugging program.
//
// The example is network-free: it drives the Studio handler through httptest
// and uses a scripted model, so it runs without an API key or an open port.
// Replace the httptest server with studio.Start(ctx, "127.0.0.1:4111", config)
// to serve the real UI for a browser.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
	"github.com/tesh254/lebro/obsv"
	"github.com/tesh254/lebro/studio"
)

// scriptedModel is a deterministic stand-in for a provider adapter. It echoes
// the last user message so a run has predictable output with no network call.
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

// newStudio wires an agent, a workflow, durable threads, and an observability
// repository onto one Studio. The repository is what makes the run's ordered
// events inspectable in the trace views; the agent's Listener feeds it.
func newStudio() (*studio.Studio, error) {
	store := lebro.NewMemoryStore()
	repository := obsv.NewMemoryRepository()

	// A synchronous observer keeps the example deterministic: a run's spans are
	// queryable the moment the run returns.
	observer, err := obsv.New(obsv.Config{Repository: repository, QueueSize: -1})
	if err != nil {
		return nil, err
	}

	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:           "assistant",
			Name:         "Assistant",
			Instructions: "You are a concise assistant.",
		},
		Model:    scriptedModel{},
		Listener: observer,
		Store:    store,
	})
	if err != nil {
		return nil, err
	}

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
		return nil, err
	}

	return studio.New(studio.Config{
		Title:     "lebro-studio-example",
		Agents:    []*lebro.Agent{agent},
		Workflows: []*lebro.LinearWorkflow{workflow},
		Store:     store,
		Traces:    repository,
	})
}

func main() {
	studioServer, err := newStudio()
	if err != nil {
		log.Fatal(err)
	}

	// httptest binds an ephemeral loopback port, so the example needs no fixed
	// port and no network egress. Replace this with
	// studio.Start(context.Background(), "127.0.0.1:4111", config) to serve the
	// UI for a browser.
	httpServer := httptest.NewServer(studioServer.Handler())
	defer httpServer.Close()

	fmt.Println("== run an agent through the API ==")
	fmt.Println(post(httpServer.URL+"/api/agents/assistant/runs", `{"messages":[{"content":"Hi there"}]}`))

	fmt.Println("\n== execute a workflow with custom JSON input ==")
	fmt.Println(post(httpServer.URL+"/api/workflows/greet/runs", `{"input":{"name":"Ada"}}`))

	fmt.Println("\n== list the traces the run recorded ==")
	traces := get(httpServer.URL + "/api/studio/traces")
	fmt.Println(traces)

	fmt.Println("\n== inspect one run's ordered events ==")
	fmt.Println(get(httpServer.URL + "/api/studio/traces/" + firstTraceID(traces)))
}

func firstTraceID(listBody string) string {
	// readBody prefixes the HTTP status line for display; the JSON body is
	// everything after the first newline.
	if newline := strings.IndexByte(listBody, '\n'); newline >= 0 {
		listBody = listBody[newline+1:]
	}
	var list struct {
		Traces []struct {
			TraceID string `json:"trace_id"`
		} `json:"traces"`
	}
	if err := json.Unmarshal([]byte(listBody), &list); err != nil || len(list.Traces) == 0 {
		return ""
	}
	return list.Traces[0].TraceID
}

func post(url, body string) string {
	response, err := http.Post(url, "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		log.Fatal(err)
	}
	return readBody(response)
}

func get(url string) string {
	response, err := http.Get(url)
	if err != nil {
		log.Fatal(err)
	}
	return readBody(response)
}

func readBody(response *http.Response) string {
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		log.Fatal(err)
	}
	return response.Status + "\n" + string(body)
}
