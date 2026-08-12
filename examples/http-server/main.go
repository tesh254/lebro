// http-server demonstrates serving registered lebro agents and workflows over
// HTTP, including a streamed run and the generated OpenAPI contract.
//
// The example is network-free: it drives its own handler through httptest and
// uses a scripted model, so it runs without an API key or an open port.
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

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/httpapi"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

// scriptedModel is a deterministic stand-in for a provider adapter. It streams
// the greeting one word at a time so the streaming route has something to
// deliver incrementally.
type scriptedModel struct{}

func (scriptedModel) Generate(_ context.Context, request lebro.ModelRequest) (lebro.ModelResponse, error) {
	return lebro.ModelResponse{
		Message:      lebro.Message{Role: lebro.RoleAssistant, Content: reply(request)},
		FinishReason: lebro.FinishReasonStop,
	}, nil
}

func (scriptedModel) Stream(_ context.Context, request lebro.ModelRequest) (lebro.StreamReader, error) {
	words := []string{"Hello, ", "this ", "is ", "a ", "streamed ", "reply."}
	index := 0
	return &lebro.StreamReaderFunc{
		NextFn: func() (lebro.StreamDelta, error) {
			if index >= len(words) {
				return lebro.StreamDelta{}, io.EOF
			}
			delta := lebro.StreamDelta{Text: words[index]}
			index++
			if index == len(words) {
				delta.FinishReason = lebro.FinishReasonStop
			}
			return delta, nil
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

// newServer wires an agent, a workflow, and durable threads onto one HTTP
// server. Middleware is where authentication belongs; the package deliberately
// ships none, so this example demonstrates the hook rather than a scheme.
func newServer() (*httpapi.Server, error) {
	store := lebro.NewMemoryStore()

	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:           "assistant",
			Name:         "Assistant",
			Instructions: "You are a concise assistant.",
		},
		Model: scriptedModel{},
		Store: store,
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

	server := httpapi.NewServer(httpapi.ServerConfig{
		Title:       "lebro-http-example",
		Version:     "1.0.0",
		Description: "An embedded lebro HTTP server.",
		Store:       store,
		Middleware: []func(http.Handler) http.Handler{
			func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					log.Printf("%s %s", r.Method, r.URL.Path)
					next.ServeHTTP(w, r)
				})
			},
		},
	})
	if err := server.ExposeAgent(agent); err != nil {
		return nil, err
	}
	if err := server.ExposeWorkflow(workflow); err != nil {
		return nil, err
	}
	return server, nil
}

func main() {
	server, err := newServer()
	if err != nil {
		log.Fatal(err)
	}

	// httptest binds an ephemeral loopback port, so the example needs no fixed
	// port, no API key, and no network egress. Replace this with
	// http.ListenAndServe(":8080", server.Handler()) to serve for real.
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	fmt.Println("== run an agent ==")
	fmt.Println(post(httpServer.URL+"/agents/assistant/runs?thread_id=demo",
		`{"messages":[{"content":"Hi there"}]}`))

	fmt.Println("\n== stream an agent run ==")
	fmt.Println(post(httpServer.URL+"/agents/assistant/runs/stream", `{"messages":[{"content":"Stream please"}]}`))

	fmt.Println("\n== run a workflow ==")
	fmt.Println(post(httpServer.URL+"/workflows/greet/runs", `{"input":{"name":"Ada"}}`))

	fmt.Println("\n== a schema violation is rejected before the run ==")
	fmt.Println(post(httpServer.URL+"/workflows/greet/runs", `{"input":{"name":42}}`))

	fmt.Println("\n== read the thread the first run persisted ==")
	fmt.Println(get(httpServer.URL + "/threads/demo/messages"))

	fmt.Println("\n== the generated contract ==")
	document, err := server.OpenAPI()
	if err != nil {
		log.Fatal(err)
	}
	var contract struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(document, &contract); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("OpenAPI %s describing %d paths; served at GET /openapi.json\n",
		contract.OpenAPI, len(contract.Paths))
}

func post(url, body string) string {
	response, err := http.Post(url, "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		log.Fatal(err)
	}
	return fmt.Sprintf("%s\n%s", response.Status, payload)
}

func get(url string) string {
	response, err := http.Get(url)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		log.Fatal(err)
	}
	return fmt.Sprintf("%s\n%s", response.Status, payload)
}
