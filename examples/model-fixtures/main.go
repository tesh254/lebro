// The model-fixtures example runs a deterministic, network-free agent turn.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
			testkit.ToolCallResponse(testkit.ToolCall{ToolID: "weather", Arguments: json.RawMessage(`{"city":"Nairobi"}`)}),
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
		OutputSchema: &lebro.ModelOutputSchema{
			Name: "weather_result", Schema: json.RawMessage(`{"type":"object"}`), Strict: true,
		},
	}

	toolResponse, err := agent.Generate(ctx, request)
	if err != nil {
		return err
	}
	if validationErr := toolResponse.Validate(); validationErr != nil {
		return &lebro.ModelError{Kind: lebro.ModelErrorMalformedResponse, Message: validationErr.Error(), Err: validationErr}
	}
	toolCalls := toolResponse.Message.ToolCalls.Values()
	if len(toolCalls) != 1 {
		return fmt.Errorf("first response returned %d tool calls, want 1", len(toolCalls))
	}
	call := toolCalls[0]
	writef(output, "tool call %s: %s %s\n", call.ID, call.ToolID, call.Arguments)

	request.Messages = append(request.Messages,
		toolResponse.Message,
		lebro.Message{Role: lebro.RoleTool, ToolCallID: call.ID, Content: `{"temperature_c":24.5}`},
	)
	finalResponse, err := agent.Generate(ctx, request)
	if err != nil {
		return err
	}
	if validationErr := finalResponse.Validate(); validationErr != nil {
		return &lebro.ModelError{Kind: lebro.ModelErrorMalformedResponse, Message: validationErr.Error(), Err: validationErr}
	}
	if finalResponse.Message.StructuredOutput == "" {
		return &lebro.ModelError{Kind: lebro.ModelErrorMalformedResponse, Message: "structured output is missing"}
	}
	writef(output, "final: %s\n", finalResponse.Message.StructuredOutput.Raw())

	events, err := stream.Stream(ctx, request)
	if err != nil {
		return err
	}
	write(output, "stream: ")
	for event := range events {
		if event.Err != nil {
			return event.Err
		}
		write(output, event.Text)
	}
	writeln(output)

	_, failureErr := failing.Generate(ctx, request)
	if failureErr == nil {
		return errors.New("failure fixture unexpectedly succeeded")
	}
	writef(output, "failure: %v\n", failureErr)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, cancellationErr := cancelling.Generate(cancelled, request)
	if !errors.Is(cancellationErr, context.Canceled) {
		return fmt.Errorf("cancellation fixture: %w", cancellationErr)
	}
	writeln(output, "cancelled: true")
	return nil
}

func write(writer io.Writer, values ...any) {
	if _, err := fmt.Fprint(writer, values...); err != nil {
		panic(err)
	}
}

func writeln(writer io.Writer, values ...any) {
	if _, err := fmt.Fprintln(writer, values...); err != nil {
		panic(err)
	}
}

func writef(writer io.Writer, format string, values ...any) {
	if _, err := fmt.Fprintf(writer, format, values...); err != nil {
		panic(err)
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
