package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tesh254/lebro"
)

func TestRunStreamsDeltasAndReturnsResult(t *testing.T) {
	t.Parallel()
	output := &bytes.Buffer{}
	if err := run(output); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	got := output.String()
	if !strings.Contains(got, "streaming: The streaming agent is now live.") {
		t.Fatalf("output = %q", got)
	}
	if !strings.Contains(got, "status:   succeeded") {
		t.Fatalf("output = %q", got)
	}
	if !strings.Contains(got, "final:    The streaming agent is now live.") {
		t.Fatalf("output = %q", got)
	}
	if !strings.Contains(got, "deltas:   6 text chunks delivered in real time") {
		t.Fatalf("output = %q", got)
	}
}

func TestAsStreamingModelPublicSurface(t *testing.T) {
	t.Parallel()
	model := newFixtureModel([]fixtureChunk{{text: "hi"}})
	if lebro.AsStreamingModel(model) == nil {
		t.Fatal("AsStreamingModel(fixtureModel) = nil, want StreamingModel")
	}
}

func TestRunCancelReleasesResources(t *testing.T) {
	t.Parallel()
	model := newFixtureModel([]fixtureChunk{{text: "never delivered", delay: 5 * time.Second}})
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
