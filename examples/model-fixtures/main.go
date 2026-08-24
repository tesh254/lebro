// The model-fixtures example runs a deterministic, network-free agent turn
// against scripted models: a two-turn tool call resolved into structured JSON,
// a streamed reply, a provider failure, and a cancelled call. It shows what a
// lebro.Model implementation owes the runtime and how each failure mode
// surfaces, using local stand-ins where a deployment would supply real
// provider adapters.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/tesh254/lebro"
)

func main() {
	must(run(os.Stdout,
		newFixtureModel(
			fixtureStep{toolCalls: []lebro.ModelToolCall{{ID: "call-1", ToolID: "weather", Arguments: json.RawMessage(`{"city":"Nairobi"}`)}}},
			fixtureStep{structured: json.RawMessage(`{"temperature_c":24.5}`)},
		),
		newStreamingFixture([]string{"forecast ", "ready"}, nil),
		newFixtureModel(fixtureStep{err: errors.New("model offline")}),
		newCancellingFixture(),
	))
}

// generateModel is the subset of the provider protocol run() drives
// synchronously; both scripted fixtures and real adapters satisfy it.
type generateModel interface {
	Generate(context.Context, lebro.ModelRequest) (lebro.ModelResponse, error)
}

func run(output io.Writer, agent generateModel, stream *streamingFixture, failing generateModel, cancelling generateModel) error {
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

	reader, err := stream.Stream(ctx, request)
	if err != nil {
		return err
	}
	write(output, "stream: ")
	for {
		delta, deltaErr := reader.Next()
		if errors.Is(deltaErr, io.EOF) {
			break
		}
		if deltaErr != nil {
			return deltaErr
		}
		write(output, delta.Text)
	}
	writeln(output)

	if _, failingErr := failing.Generate(ctx, request); failingErr == nil {
		return errors.New("failing fixture unexpectedly succeeded")
	} else {
		writef(output, "failure: %v\n", failingErr)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, cancellationErr := cancelling.Generate(cancelled, request); !errors.Is(cancellationErr, context.Canceled) {
		return fmt.Errorf("cancellation fixture: %w", cancellationErr)
	}
	writeln(output, "cancelled: true")
	return nil
}

// fixtureStep scripts one Generate call: a tool-call request, a structured
// payload, plain text, or a failure.
type fixtureStep struct {
	content    string
	structured json.RawMessage
	toolCalls  []lebro.ModelToolCall
	err        error
}

// fixtureModel is a deterministic stand-in for a provider adapter: one
// scripted step per call, consumed in order. A real deployment supplies
// openai.New or any other lebro.Model instead.
type fixtureModel struct {
	steps []fixtureStep
	next  int
}

func newFixtureModel(steps ...fixtureStep) *fixtureModel {
	return &fixtureModel{steps: steps}
}

func (m *fixtureModel) Generate(_ context.Context, _ lebro.ModelRequest) (lebro.ModelResponse, error) {
	if m.next >= len(m.steps) {
		return lebro.ModelResponse{}, errors.New("fixture model script exhausted")
	}
	step := m.steps[m.next]
	m.next++
	if step.err != nil {
		return lebro.ModelResponse{}, step.err
	}
	message := lebro.Message{Role: lebro.RoleAssistant, Content: step.content}
	finish := lebro.FinishReasonStop
	if len(step.toolCalls) > 0 {
		calls, err := lebro.NewModelToolCalls(step.toolCalls...)
		if err != nil {
			return lebro.ModelResponse{}, err
		}
		message.Content = ""
		message.ToolCalls = calls
		finish = lebro.FinishReasonToolCalls
	}
	if step.structured != nil {
		message.StructuredOutput = lebro.NewModelStructuredOutput(step.structured)
	}
	response := lebro.ModelResponse{Message: message, FinishReason: finish}
	if err := response.Validate(); err != nil {
		return lebro.ModelResponse{}, &lebro.ModelError{Kind: lebro.ModelErrorMalformedResponse, Message: err.Error(), Err: err}
	}
	return response, nil
}

// streamingFixture replays canned words as stream deltas. err fails the Stream
// call itself; chunkErr fails delivery partway through, so callers can prove
// both setup and mid-stream failures surface.
type streamingFixture struct {
	words    []string
	err      error
	chunkErr error
}

func newStreamingFixture(words []string, err error) *streamingFixture {
	return &streamingFixture{words: words, err: err}
}

// Stream implements the streaming half of the provider protocol.
func (m *streamingFixture) Stream(_ context.Context, _ lebro.ModelRequest) (lebro.StreamReader, error) {
	if m.err != nil {
		return nil, m.err
	}
	index := 0
	return &lebro.StreamReaderFunc{
		NextFn: func() (lebro.StreamDelta, error) {
			if index >= len(m.words) {
				if m.chunkErr != nil {
					return lebro.StreamDelta{}, m.chunkErr
				}
				return lebro.StreamDelta{}, io.EOF
			}
			delta := lebro.StreamDelta{Text: m.words[index]}
			index++
			// A scripted mid-stream failure means the stream did not finish,
			// so no terminal finish reason is emitted before the error.
			if index == len(m.words) && m.chunkErr == nil {
				delta.FinishReason = lebro.FinishReasonStop
			}
			return delta, nil
		},
		CloseFn: func() error { return nil },
	}, nil
}

// cancellingFixture stands in for a provider call that only ever observes its
// caller's context, so a cancelled run surfaces as context.Canceled.
type cancellingFixture struct{}

func newCancellingFixture() *cancellingFixture { return &cancellingFixture{} }

func (cancellingFixture) Generate(ctx context.Context, _ lebro.ModelRequest) (lebro.ModelResponse, error) {
	<-ctx.Done()
	return lebro.ModelResponse{}, ctx.Err()
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

func writef(writer io.Writer, format string, args ...any) {
	if _, err := fmt.Fprintf(writer, format, args...); err != nil {
		panic(err)
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
