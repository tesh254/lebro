package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
)

func TestRunUsesScriptedModelWithoutNetwork(t *testing.T) {
	output := temporaryOutput(t)
	err := run(output,
		testkit.NewModel(
			testkit.ToolCallResponse(testkit.ToolCall{Name: "weather", Arguments: json.RawMessage(`{"city":"Nairobi"}`)}),
			testkit.StructuredOutput(json.RawMessage(`{"temperature_c":24.5}`)),
		),
		testkit.NewModel(testkit.Stream(testkit.TextChunk("forecast "), testkit.TextChunk("ready"))),
		testkit.NewModel(testkit.Failure(errors.New("model offline"))),
		testkit.NewModel(testkit.Text("unused")),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	want := "tool call tool-call-0001: weather {\"city\":\"Nairobi\"}\n" +
		"final: {\"temperature_c\":24.5}\n" +
		"stream: forecast ready\n" +
		"failure: model offline\n" +
		"cancelled: true\n"
	if string(content) != want {
		t.Fatalf("output = %q, want %q", content, want)
	}
}

func TestExampleMain(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	t.Cleanup(func() { os.Stdout = original })

	main()
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = read.Close() })
	content, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "cancelled: true") {
		t.Fatalf("main output = %q", content)
	}
}

func TestRunReturnsModelAndStreamErrors(t *testing.T) {
	tests := []struct {
		name       string
		agent      scriptedModel
		stream     scriptedModel
		failing    scriptedModel
		cancelling scriptedModel
		want       string
	}{
		{
			name: "first agent call", agent: testkit.NewModel(testkit.Failure(errors.New("first"))),
			stream: testkit.NewModel(), failing: testkit.NewModel(), cancelling: testkit.NewModel(), want: "first",
		},
		{
			name: "second agent call", agent: testkit.NewModel(testkit.Text("tool"), testkit.Failure(errors.New("second"))),
			stream: testkit.NewModel(), failing: testkit.NewModel(), cancelling: testkit.NewModel(), want: "second",
		},
		{
			name: "stream setup", agent: successfulAgent(), stream: testkit.NewModel(testkit.Failure(errors.New("stream setup"))),
			failing: testkit.NewModel(), cancelling: testkit.NewModel(), want: "stream setup",
		},
		{
			name: "stream event", agent: successfulAgent(), stream: testkit.NewModel(testkit.Stream(testkit.FailureChunk(errors.New("stream event")))),
			failing: testkit.NewModel(), cancelling: testkit.NewModel(), want: "stream event",
		},
		{
			name: "failure unexpectedly succeeds", agent: successfulAgent(), stream: testkit.NewModel(testkit.Stream()),
			failing: testkit.NewModel(testkit.Text("ok")), cancelling: testkit.NewModel(), want: "unexpectedly succeeded",
		},
		{
			name: "wrong cancellation", agent: successfulAgent(), stream: testkit.NewModel(testkit.Stream()),
			failing: testkit.NewModel(testkit.Failure(errors.New("expected"))), cancelling: ignoringModel{}, want: "cancellation fixture",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output := temporaryOutput(t)
			err := run(output, test.agent, test.stream, test.failing, test.cancelling)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("run() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestMust(t *testing.T) {
	must(nil)
	defer func() {
		if recover() == nil {
			t.Fatal("must did not panic")
		}
	}()
	must(errors.New("boom"))
}

type ignoringModel struct{}

func (ignoringModel) Generate(context.Context, lebro.ModelRequest) (lebro.ModelResponse, error) {
	return lebro.ModelResponse{}, nil
}

func (ignoringModel) Stream(context.Context, lebro.ModelRequest) (<-chan testkit.StreamEvent, error) {
	stream := make(chan testkit.StreamEvent)
	close(stream)
	return stream, nil
}

func successfulAgent() scriptedModel {
	return testkit.NewModel(testkit.Text("tool"), testkit.Text("final"))
}

func temporaryOutput(t *testing.T) *os.File {
	t.Helper()
	output, err := os.CreateTemp(t.TempDir(), "model-fixtures-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	return output
}
