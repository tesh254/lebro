// The streaming example runs a bounded agent against a scripted streaming
// model and emits text deltas in real time, then collects the final result.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
)

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "streaming example failed: %v\n", err)
		os.Exit(1)
	}
}

func run(output io.Writer) error {
	model := testkit.NewModel(testkit.Stream(
		testkit.TextChunk("Hello "),
		testkit.TextChunk("from "),
		testkit.TextChunk("the "),
		testkit.TextChunk("streaming "),
		testkit.TextChunk("agent!"),
	))
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "streaming-echo", Model: "fixture-model", Instructions: "be brief"},
		Model:      model,
	})
	if err != nil {
		return fmt.Errorf("new agent: %w", err)
	}

	run, err := agent.RunStream(context.Background(), lebro.RunInput{
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "greet me"}},
		Metadata: map[string]string{"source": "streaming-example"},
	})
	if err != nil {
		return fmt.Errorf("run stream: %w", err)
	}
	defer run.Cancel()

	write(output, "deltas: ")
	var deltaCount int
	for delta := range run.Deltas {
		if delta.Text != "" {
			write(output, delta.Text)
			deltaCount++
		}
	}
	writeln(output)
	if deltaCount == 0 {
		return errors.New("no deltas were emitted")
	}

	result, err := run.Wait()
	if err != nil {
		return fmt.Errorf("wait: %w", err)
	}
	writef(output, "status: %s\n", result.Status)
	writef(output, "final: %s\n", result.Messages[len(result.Messages)-1].Content)
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
