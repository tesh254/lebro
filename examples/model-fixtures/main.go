// The model-fixtures example runs a deterministic, network-free agent turn.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
)

type scriptedModel interface {
	lebro.Model
	Stream(context.Context, lebro.ModelRequest) (<-chan testkit.StreamEvent, error)
}

func main() {
	must(run(os.Stdout,
		testkit.NewModel(
			testkit.ToolCallResponse(testkit.ToolCall{Name: "weather", Arguments: json.RawMessage(`{"city":"Nairobi"}`)}),
			testkit.StructuredOutput(json.RawMessage(`{"temperature_c":24.5}`)),
		),
		testkit.NewModel(testkit.Stream(testkit.TextChunk("forecast "), testkit.TextChunk("ready"))),
		testkit.NewModel(testkit.Failure(errors.New("model offline"))),
		testkit.NewModel(testkit.Text("unused")),
	))
}

func run(output *os.File, agent, stream, failing, cancelling scriptedModel) error {
	ctx := context.Background()
	request := lebro.ModelRequest{
		Model:    "fixture-model",
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "What is the temperature in Nairobi?"}},
		Tools:    []lebro.ToolDefinition{{ID: "weather"}},
	}

	toolResponse, err := agent.Generate(ctx, request)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "tool call %s: %s %s\n", toolResponse.Message.ToolCallID, toolResponse.Message.Name, toolResponse.Message.Content)

	request.Messages = append(request.Messages,
		toolResponse.Message,
		lebro.Message{Role: lebro.RoleTool, ToolCallID: toolResponse.Message.ToolCallID, Content: `{"temperature_c":24.5}`},
	)
	finalResponse, err := agent.Generate(ctx, request)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "final: %s\n", finalResponse.Message.Content)

	events, err := stream.Stream(ctx, request)
	if err != nil {
		return err
	}
	fmt.Fprint(output, "stream: ")
	for event := range events {
		if event.Err != nil {
			return event.Err
		}
		fmt.Fprint(output, event.Text)
	}
	fmt.Fprintln(output)

	_, failureErr := failing.Generate(ctx, request)
	if failureErr == nil {
		return errors.New("failure fixture unexpectedly succeeded")
	}
	fmt.Fprintf(output, "failure: %v\n", failureErr)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, cancellationErr := cancelling.Generate(cancelled, request)
	if !errors.Is(cancellationErr, context.Canceled) {
		return fmt.Errorf("cancellation fixture: %w", cancellationErr)
	}
	fmt.Fprintln(output, "cancelled: true")
	return nil
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
