// The reasoning example runs a real architecture review through OpenRouter.
// It keeps the assistant transcript in a durable thread so provider replay
// metadata is available unchanged on the next turn.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/openai"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	modelName := os.Getenv("OPENROUTER_MODEL")
	if apiKey == "" || modelName == "" {
		return errors.New("set OPENROUTER_API_KEY and OPENROUTER_MODEL")
	}

	model, err := openai.New(openai.Config{
		BaseURL: "https://openrouter.ai/api/v1",
		APIKey:  apiKey,
		Model:   modelName,
	})
	if err != nil {
		return err
	}
	store := lebro.NewMemoryStore()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := store.CreateThread(ctx, lebro.ThreadRecord{ID: "architecture-review-42"}); err != nil {
		return err
	}
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:           "architecture-reviewer",
			Model:        modelName,
			Instructions: "Review architecture tradeoffs. State assumptions, risks, and a concise recommendation.",
		},
		Model: model,
		Store: store,
	})
	if err != nil {
		return err
	}

	run, err := agent.RunStream(ctx, lebro.RunInput{
		ThreadID: "architecture-review-42",
		Messages: []lebro.Message{{
			Role:    lebro.RoleUser,
			Content: "Should our Go SDK expose high-level reasoning tiers or provider-specific knobs?",
		}},
		Reasoning: lebro.ReasoningConfig{Effort: lebro.ReasoningHigh},
	})
	if err != nil {
		return err
	}
	defer run.Cancel()

	for delta := range run.Deltas {
		if delta.Reasoning.Text != "" {
			fmt.Fprint(os.Stderr, delta.Reasoning.Text)
		}
		if delta.Text != "" {
			fmt.Print(delta.Text)
		}
	}
	result, err := run.Wait()
	if err != nil {
		return err
	}
	message := result.Messages[len(result.Messages)-1]
	fmt.Printf("\n\nreasoning retained: %t\n", !message.Reasoning.IsZero())
	return nil
}
