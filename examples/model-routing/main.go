// The model-routing example demonstrates provider registry, routing, and
// fallback policies. A primary provider fails with a retryable error, and the
// router falls back to a secondary provider that succeeds.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
)

func main() {
	must(run(os.Stdout))
}

func run(output io.Writer) error {
	// Create two fixture models: one that fails, one that succeeds.
	failModel := testkit.NewModel(
		testkit.Failure(&lebro.ModelError{
			Kind:    lebro.ModelErrorUnavailable,
			Message: "provider down",
		}),
	)
	okModel := testkit.NewModel(
		testkit.Text("Hello from fallback provider!"),
	)

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
