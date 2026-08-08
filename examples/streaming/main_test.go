package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
)

func TestRunStreamsDeltasAndReturnsResult(t *testing.T) {
	t.Parallel()
	output := &bytes.Buffer{}
	if err := run(output); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	got := output.String()
	if !strings.Contains(got, "deltas: Hello from the streaming agent!") {
		t.Fatalf("output = %q", got)
	}
	if !strings.Contains(got, "status: succeeded") {
		t.Fatalf("output = %q", got)
	}
	if !strings.Contains(got, "final: Hello from the streaming agent!") {
		t.Fatalf("output = %q", got)
	}
}

func TestAsStreamingModelPublicSurface(t *testing.T) {
	t.Parallel()
	model := testkit.NewModel(testkit.Stream(testkit.TextChunk("hi")))
	if lebro.AsStreamingModel(model) == nil {
		t.Fatal("AsStreamingModel(testkit.Model) = nil, want StreamingModel")
	}
}

func TestRunCancelReleasesResources(t *testing.T) {
	t.Parallel()
	model := testkit.NewModel(testkit.Stream(testkit.TextChunk("never delivered")))
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "cancel-agent", Model: "fixture-model"},
		Model:      model,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run, err := agent.RunStream(ctx, lebro.RunInput{
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("RunStream() error = %v", err)
	}
	run.Cancel()
	_, _ = run.Wait()
}
