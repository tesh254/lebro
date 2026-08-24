// The model-routing example demonstrates provider registry, routing, and
// fallback policies. A primary provider fails with a retryable error, and the
// router falls back to a secondary provider that succeeds.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/tesh254/lebro"
)

func main() {
	must(run(os.Stdout))
}

func run(output io.Writer) error {
	// Create two fixture models: one that fails, one that succeeds.
	failModel := newFailingModel(&lebro.ModelError{
		Kind:    lebro.ModelErrorUnavailable,
		Message: "provider down",
	})
	okModel := newFixtureModel([]string{"Hello from fallback provider!"})

	// Register both providers in a registry.
	registry := lebro.NewProviderRegistry()
	if err := registry.Register(lebro.ProviderEntry{
		ID:    "primary",
		Model: failModel,
	}); err != nil {
		return err
	}
	if err := registry.Register(lebro.ProviderEntry{
		ID:    "fallback",
		Model: okModel,
	}); err != nil {
		return err
	}

	// Configure the router with a primary provider and fallback chain.
	router, err := lebro.NewModelRouter(lebro.ModelRouterConfig{
		Registry: registry,
		Policy:   lebro.RoutingPolicy{Primary: "primary"},
		Fallback: &lebro.FallbackPolicy{
			Chain: []lebro.ProviderID{"fallback"},
		},
	})
	if err != nil {
		return err
	}

	// Create an agent using the router instead of a direct model.
	recorder := lebro.NewRunRecorder()
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:    "routing-agent",
			Name:  "Router",
			Model: "fixture-model",
		},
		Router:   router,
		Listener: recorder,
	})
	if err != nil {
		return err
	}

	// Run the agent. The primary provider will fail, and the router will
	// fall back to the secondary provider.
	result, err := agent.Run(context.Background(), lebro.RunInput{
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "Hello"}},
	})
	if err != nil {
		return fmt.Errorf("agent run: %w", err)
	}

	writef(output, "status: %s\n", result.Status)
	writef(output, "run_id: %s\n", result.ID)

	// Show the model attempts recorded in the result.
	writef(output, "model_attempts: %d\n", len(result.ModelAttempts))
	for i, attempt := range result.ModelAttempts {
		status := string(attempt.Status)
		errMsg := ""
		if attempt.Error != nil {
			errMsg = fmt.Sprintf(" (%s)", attempt.Error.Kind)
		}
		writef(output, "  [%d] provider=%s status=%s%s\n", i+1, attempt.Provider, status, errMsg)
	}

	// Show the assistant response.
	for _, msg := range result.Messages {
		if msg.Role == lebro.RoleAssistant {
			writef(output, "assistant: %s\n", msg.Content)
		}
	}

	// Show the routing events from the recorder.
	writef(output, "events:\n")
	for _, event := range recorder.Events() {
		if event.Type == lebro.RunEventModelAttemptStarted || event.Type == lebro.RunEventModelAttemptFinished {
			writef(output, "  %s provider=%s\n", event.Type, event.Provider)
		}
	}

	return nil
}

// fixtureModel is a deterministic stand-in for a provider adapter; a real
// deployment registers openai.New or any other lebro.Model instead.
type fixtureModel struct {
	replies []string
	calls   int
}

func newFixtureModel(replies []string) *fixtureModel { return &fixtureModel{replies: replies} }

func (m *fixtureModel) Generate(_ context.Context, _ lebro.ModelRequest) (lebro.ModelResponse, error) {
	if m.calls >= len(m.replies) {
		return lebro.ModelResponse{}, errors.New("fixture model script exhausted")
	}
	reply := m.replies[m.calls]
	m.calls++
	return lebro.ModelResponse{
		Message:      lebro.Message{Role: lebro.RoleAssistant, Content: reply},
		FinishReason: lebro.FinishReasonStop,
	}, nil
}

// failingModel always returns the supplied provider error, standing in for a
// primary provider that is down.
type failingModel struct {
	err error
}

func newFailingModel(err error) *failingModel { return &failingModel{err: err} }

func (m *failingModel) Generate(context.Context, lebro.ModelRequest) (lebro.ModelResponse, error) {
	return lebro.ModelResponse{}, m.err
}

func writef(output io.Writer, format string, args ...any) {
	if _, err := fmt.Fprintf(output, format, args...); err != nil {
		panic(err)
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
