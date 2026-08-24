package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tesh254/lebro"
)

func successfulAgent() *fixtureModel {
	return newFixtureModel(
		fixtureStep{toolCalls: []lebro.ModelToolCall{{ID: "call-1", ToolID: "weather", Arguments: json.RawMessage(`{}`)}}},
		fixtureStep{structured: json.RawMessage(`{}`)},
	)
}

func emptyStream() *streamingFixture { return newStreamingFixture([]string{"ok"}, nil) }

func TestRunUsesScriptedModelWithoutNetwork(t *testing.T) {
	var output bytes.Buffer
	err := run(&output,
		newFixtureModel(
			fixtureStep{toolCalls: []lebro.ModelToolCall{{ID: "call-1", ToolID: "weather", Arguments: json.RawMessage(`{"city":"Nairobi"}`)}}},
			fixtureStep{structured: json.RawMessage(`{"temperature_c":24.5}`)},
		),
		newStreamingFixture([]string{"forecast ", "ready"}, nil),
		newFixtureModel(fixtureStep{err: errors.New("model offline")}),
		newCancellingFixture(),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := "tool call call-1: weather {\"city\":\"Nairobi\"}\n" +
		"final: {\"temperature_c\":24.5}\n" +
		"stream: forecast ready\n" +
		"failure: model offline\n" +
		"cancelled: true\n"
	if output.String() != want {
		t.Fatalf("output = %q, want %q", output.String(), want)
	}
}

func TestRunReturnsModelAndStreamErrors(t *testing.T) {
	tests := []struct {
		name       string
		agent      generateModel
		stream     *streamingFixture
		failing    generateModel
		cancelling generateModel
		want       string
	}{
		{
			name: "first agent call", agent: newFixtureModel(fixtureStep{err: errors.New("first")}),
			stream: emptyStream(), failing: newFixtureModel(fixtureStep{content: "unused"}), cancelling: newCancellingFixture(), want: "first",
		},
		{
			name: "invalid tool response", agent: newFixtureModel(
				fixtureStep{content: "text but finish reason says tool calls"},
				fixtureStep{structured: json.RawMessage(`{}`)},
			),
			stream: emptyStream(), failing: newFixtureModel(fixtureStep{content: "unused"}), cancelling: newCancellingFixture(),
			want: "tool calls",
		},
		{
			name: "missing tool call", agent: newFixtureModel(
				fixtureStep{content: "plain answer"},
				fixtureStep{structured: json.RawMessage(`{}`)},
			),
			stream: emptyStream(), failing: newFixtureModel(fixtureStep{content: "unused"}), cancelling: newCancellingFixture(), want: "want 1",
		},
		{
			name: "second agent call", agent: newFixtureModel(
				fixtureStep{toolCalls: []lebro.ModelToolCall{{ID: "call-1", ToolID: "weather", Arguments: json.RawMessage(`{}`)}}},
				fixtureStep{err: errors.New("second")},
			),
			stream: emptyStream(), failing: newFixtureModel(fixtureStep{content: "unused"}), cancelling: newCancellingFixture(), want: "second",
		},
		{
			name: "missing structured output", agent: newFixtureModel(
				fixtureStep{toolCalls: []lebro.ModelToolCall{{ID: "call-1", ToolID: "weather", Arguments: json.RawMessage(`{}`)}}},
				fixtureStep{content: "not JSON"},
			),
			stream: emptyStream(), failing: newFixtureModel(fixtureStep{content: "unused"}), cancelling: newCancellingFixture(),
			want: "structured output is missing",
		},
		{
			name: "stream setup", agent: successfulAgent(), stream: newStreamingFixture(nil, errors.New("stream setup")),
			failing: newFixtureModel(fixtureStep{content: "unused"}), cancelling: newCancellingFixture(), want: "stream setup",
		},
		{
			name: "failing fixture unexpectedly succeeds", agent: successfulAgent(), stream: emptyStream(),
			failing: newFixtureModel(fixtureStep{content: "ok"}), cancelling: newCancellingFixture(),
			want: "unexpectedly succeeded",
		},
		{
			name: "wrong cancellation", agent: successfulAgent(), stream: emptyStream(),
			failing: newFixtureModel(fixtureStep{err: errors.New("expected")}), cancelling: ignoringCancelling{},
			want: "cancellation fixture",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			err := run(&output, test.agent, test.stream, test.failing, test.cancelling)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("run() error = %v, want it to contain %q", err, test.want)
			}
		})
	}
}

func TestStreamingFixtureFailsMidStream(t *testing.T) {
	stream := &streamingFixture{words: []string{"partial"}, chunkErr: errors.New("stream event")}
	reader, err := stream.Stream(context.Background(), lebro.ModelRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Next(); err != nil {
		t.Fatalf("first delta error = %v", err)
	}
	if _, err := reader.Next(); err == nil || !strings.Contains(err.Error(), "stream event") {
		t.Fatalf("second delta error = %v, want stream event failure", err)
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

type ignoringCancelling struct{}

func (ignoringCancelling) Generate(context.Context, lebro.ModelRequest) (lebro.ModelResponse, error) {
	return lebro.ModelResponse{}, nil
}
