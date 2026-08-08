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
	"github.com/tesh254/lebro/internal/testkit"
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

	model := testkit.NewModel(testkit.Stream(
		testkit.StreamChunk{Text: "The ", Delay: 100 * time.Millisecond},
		testkit.StreamChunk{Text: "streaming ", Delay: 150 * time.Millisecond},
		testkit.StreamChunk{Text: "agent ", Delay: 150 * time.Millisecond},
		testkit.StreamChunk{Text: "is ", Delay: 100 * time.Millisecond},
		testkit.StreamChunk{Text: "now ", Delay: 120 * time.Millisecond},
		testkit.StreamChunk{Text: "live.", Delay: 80 * time.Millisecond},
	))
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
