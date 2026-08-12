// http-client demonstrates calling a lebro HTTP API with the typed client:
// a complete run, a streamed run the caller cancels mid-flight, a workflow
// round trip, typed error handling, and the contract-version handshake.
//
// The example is network-free: it serves the API in-process through httptest
// and uses a scripted model, so it runs without an API key or an open port.
// Point ClientConfig.BaseURL at a real deployment to use it for real.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/httpapi"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

// scriptedModel is a deterministic stand-in for a provider adapter. Streaming
// emits one word at a time, slowly enough that the cancellation demonstration
// below interrupts a run that is genuinely still in flight.
type scriptedModel struct{}

func (scriptedModel) Generate(_ context.Context, request lebro.ModelRequest) (lebro.ModelResponse, error) {
	response := lebro.ModelResponse{
		Message:      lebro.Message{Role: lebro.RoleAssistant, Content: reply(request)},
		FinishReason: lebro.FinishReasonStop,
	}
	if request.OutputSchema != nil {
		response.Message.StructuredOutput = lebro.NewModelStructuredOutput(
			json.RawMessage(`{"city":"Nairobi","temperature_c":24}`))
	}
	return response, nil
}

// streamedWords is the scripted stream's payload. streamAndCancel derives its
// cancellation point from the length, so the two cannot drift apart.
var streamedWords = []string{"Streaming ", "one ", "word ", "at ", "a ", "time ", "until ", "cancelled."}

func (scriptedModel) Stream(ctx context.Context, _ lebro.ModelRequest) (lebro.StreamReader, error) {
	words := streamedWords
	index := 0
	return &lebro.StreamReaderFunc{
		NextFn: func() (lebro.StreamDelta, error) {
			if index >= len(words) {
				return lebro.StreamDelta{}, io.EOF
			}
			// Pace the stream so cancellation lands mid-run rather than after
			// the model has already finished.
			select {
			case <-ctx.Done():
				return lebro.StreamDelta{}, ctx.Err()
			case <-time.After(20 * time.Millisecond):
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

// newServer wires the API this example calls. It is the ordinary embedded
// server; the client has no privileged access to it.
func newServer() (*httpapi.Server, error) {
	store := lebro.NewMemoryStore()

	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "assistant", Name: "Assistant"},
		Model:      scriptedModel{},
		Store:      store,
	})
	if err != nil {
		return nil, err
	}

	forecaster, err := lebro.NewAgent(lebro.AgentConfig{
		Definition:     lebro.AgentDefinition{ID: "forecaster", Name: "Forecaster"},
		Model:          scriptedModel{},
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
		Title:   "lebro-http-client-example",
		Version: "1.0.0",
		Store:   store,
	})
	for _, expose := range []func() error{
		func() error { return server.ExposeAgent(agent) },
		func() error { return server.ExposeAgent(forecaster) },
		func() error { return server.ExposeWorkflow(workflow) },
	} {
		if err := expose(); err != nil {
			return nil, err
		}
	}
	return server, nil
}

func main() {
	server, err := newServer()
	if err != nil {
		log.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	// A real client points at a deployed base URL and supplies credentials
	// through the Header hook; the package ships no authentication scheme.
	client, err := httpapi.NewClient(httpapi.ClientConfig{
		BaseURL:    httpServer.URL,
		HTTPClient: &http.Client{},
		Header: func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer example-token")
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()

	fmt.Println("== contract handshake ==")
	if err := client.CheckCompatibility(ctx); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("server speaks contract %s\n", httpapi.ContractVersion)

	fmt.Println("\n== discover what is exposed ==")
	agents, err := client.ListAgents(ctx)
	if err != nil {
		log.Fatal(err)
	}
	for _, agent := range agents {
		fmt.Printf("agent %s (%s)\n", agent.ID, agent.Name)
	}

	fmt.Println("\n== run an agent, bound to a durable thread ==")
	result, err := client.Run(ctx, "assistant",
		httpapi.RunRequest{Messages: []httpapi.MessageInput{{Content: "Hi there"}}},
		httpapi.WithThread("demo"),
	)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s: %s\n", result.Status, result.Content)

	fmt.Println("\n== structured output round trip ==")
	forecast, err := client.Run(ctx, "forecaster",
		httpapi.RunRequest{Messages: []httpapi.MessageInput{{Content: "Forecast for Nairobi"}}})
	if err != nil {
		log.Fatal(err)
	}
	var decoded struct {
		City         string  `json:"city"`
		TemperatureC float64 `json:"temperature_c"`
	}
	if err := json.Unmarshal(forecast.StructuredOutput, &decoded); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s is %.0f°C\n", decoded.City, decoded.TemperatureC)

	fmt.Println("\n== stream a run, then cancel it mid-flight ==")
	streamAndCancel(ctx, client)

	fmt.Println("\n== run a workflow ==")
	workflowResult, err := client.RunWorkflow(ctx, "greet", httpapi.WorkflowRunRequest{
		Input: json.RawMessage(`{"name":"Ada"}`),
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s: %s\n", workflowResult.Status, workflowResult.Output)

	fmt.Println("\n== errors arrive typed ==")
	reportTypedErrors(ctx, client)

	fmt.Println("\n== read the thread the first run persisted ==")
	page, err := client.ListMessages(ctx, "demo", httpapi.WithLimit(10))
	if err != nil {
		log.Fatal(err)
	}
	for _, message := range page.Messages {
		fmt.Printf("%s: %s\n", message.Role, message.Content)
	}
}

// streamAndCancel consumes part of a streamed run and then cancels it. The
// deferred Cancel is what releases the connection and the reader goroutine when
// a caller stops early, which is why it belongs on every stream.
func streamAndCancel(ctx context.Context, client *httpapi.Client) {
	stream, err := client.RunStream(ctx, "assistant", httpapi.RunRequest{
		Messages: []httpapi.MessageInput{{Content: "Stream please"}},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer stream.Cancel()

	// Cancel partway through the model's word list rather than at a fixed
	// count, so shortening or lengthening the script cannot silently push the
	// cancellation past the end of the run and turn this into a demonstration
	// of a run that simply finished. Cancellation correctness does not depend
	// on the timing — the reader stops on Cancel immediately — but the demo's
	// meaning does.
	readBeforeCancel := len(streamedWords) / 2
	var seen int
	for event := range stream.Events {
		fmt.Printf("delta: %q\n", event.Text)
		seen++
		if seen == readBeforeCancel {
			// Cancelling closes the connection, which the server observes as a
			// client disconnect and turns into a cancelled run: the remote run
			// stops rather than finishing unobserved.
			stream.Cancel()
			break
		}
	}

	_, err = stream.Drain()
	switch {
	case errors.Is(err, context.Canceled):
		fmt.Println("run cancelled, as requested")
	case err != nil:
		log.Fatal(err)
	default:
		fmt.Println("run completed before the cancellation landed")
	}
}

// reportTypedErrors shows the mapping the SDK exists to provide: a remote
// failure matches the same lebro sentinel as the local one it stands for, so
// error handling does not change when an agent moves behind HTTP.
func reportTypedErrors(ctx context.Context, client *httpapi.Client) {
	_, err := client.Run(ctx, "no-such-agent", httpapi.RunRequest{})
	fmt.Printf("unknown agent: errors.Is(err, lebro.ErrNotFound) = %t\n", errors.Is(err, lebro.ErrNotFound))

	_, err = client.RunWorkflow(ctx, "greet", httpapi.WorkflowRunRequest{
		Input: json.RawMessage(`{"name":42}`),
	})
	var apiErr *httpapi.APIError
	if errors.As(err, &apiErr) {
		fmt.Printf("bad workflow input: code=%s status=%d\n", apiErr.Code, apiErr.StatusCode)
	}
}
