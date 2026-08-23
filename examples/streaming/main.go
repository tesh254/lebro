// The streaming example runs a bounded agent against a scripted streaming
// model and emits text deltas in real time, then collects the final result.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/tesh254/lebro"
)

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "streaming example failed: %v\n", err)
		os.Exit(1)
	}
}

func run(output io.Writer) error {
	buf := bufio.NewWriter(output)
	defer func() { _ = buf.Flush() }()

	model := newFixtureModel([]fixtureChunk{
		{text: "The ", delay: 100 * time.Millisecond},
		{text: "streaming ", delay: 150 * time.Millisecond},
		{text: "agent ", delay: 150 * time.Millisecond},
		{text: "is ", delay: 100 * time.Millisecond},
		{text: "now ", delay: 120 * time.Millisecond},
		{text: "live.", delay: 80 * time.Millisecond},
	})
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "streaming-echo", Model: "fixture-model", Instructions: "be brief"},
		Model:      model,
	})
	if err != nil {
		return fmt.Errorf("new agent: %w", err)
	}

	write(buf, "streaming: ")
	if err := buf.Flush(); err != nil {
		return err
	}
	runHandle, err := agent.RunStream(context.Background(), lebro.RunInput{
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "greet me in real time"}},
		Metadata: map[string]string{"source": "streaming-example"},
	})
	if err != nil {
		return fmt.Errorf("run stream: %w", err)
	}
	defer runHandle.Cancel()

	var deltaCount int
	for delta := range runHandle.Deltas {
		if delta.Text != "" {
			write(buf, delta.Text)
			if err := buf.Flush(); err != nil {
				return err
			}
			deltaCount++
		}
	}
	writeln(buf)
	if err := buf.Flush(); err != nil {
		return err
	}
	if deltaCount == 0 {
		return errors.New("no deltas were emitted")
	}

	result, err := runHandle.Wait()
	if err != nil {
		return fmt.Errorf("wait: %w", err)
	}
	writef(buf, "status:   %s\n", result.Status)
	writef(buf, "final:    %s\n", result.Messages[len(result.Messages)-1].Content)
	writef(buf, "deltas:   %d text chunks delivered in real time\n", deltaCount)
	return buf.Flush()
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

// fixtureChunk is one scripted streaming delta and the pause before it.
type fixtureChunk struct {
	text  string
	delay time.Duration
}

// fixtureModel is a deterministic stand-in for a streaming provider adapter.
// Generate returns the full reply in one turn; Stream delivers the same words
// chunk by chunk with their delays, honoring cancellation so a cancelled run
// releases the caller immediately. A real deployment supplies openai.New or
// any other lebro.Model instead.
type fixtureModel struct {
	chunks []fixtureChunk
}

func newFixtureModel(chunks []fixtureChunk) *fixtureModel { return &fixtureModel{chunks: chunks} }

func (m *fixtureModel) fullText() string {
	var text string
	for _, chunk := range m.chunks {
		text += chunk.text
	}
	return text
}

// Generate consumes the whole script synchronously.
func (m *fixtureModel) Generate(_ context.Context, _ lebro.ModelRequest) (lebro.ModelResponse, error) {
	return lebro.ModelResponse{
		Message:      lebro.Message{Role: lebro.RoleAssistant, Content: m.fullText()},
		FinishReason: lebro.FinishReasonStop,
	}, nil
}

// Stream delivers the script chunk by chunk. Cancellation between chunks (or
// while a reader waits) stops delivery so no goroutine outlives the run.
func (m *fixtureModel) Stream(ctx context.Context, _ lebro.ModelRequest) (lebro.StreamReader, error) {
	index := 0
	return &lebro.StreamReaderFunc{
		NextFn: func() (lebro.StreamDelta, error) {
			if index >= len(m.chunks) {
				return lebro.StreamDelta{}, io.EOF
			}
			chunk := m.chunks[index]
			if chunk.delay > 0 {
				timer := time.NewTimer(chunk.delay)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-ctx.Done():
					return lebro.StreamDelta{}, ctx.Err()
				}
			}
			index++
			delta := lebro.StreamDelta{Text: chunk.text}
			if index == len(m.chunks) {
				delta.FinishReason = lebro.FinishReasonStop
			}
			return delta, nil
		},
		CloseFn: func() error { return nil },
	}, nil
}

var _ lebro.StreamingModel = (*fixtureModel)(nil)
