// acp-openrouter serves a small OpenRouter-backed Lebro agent over ACP stdio.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"unicode/utf8"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/acp"
	"github.com/tesh254/lebro/jsonschema"
	"github.com/tesh254/lebro/openai"
)

type inspectText struct{}

func (inspectText) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{
		ID:          "inspect_text",
		Description: "Count characters and words in supplied text. Use when asked to inspect text.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`),
	}
}

func (inspectText) Execute(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
	var args struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Characters int `json:"characters"`
		Words      int `json:"words"`
	}{Characters: utf8.RuneCountInString(args.Text), Words: len(strings.Fields(args.Text))})
}

func newServer(apiKey, modelName, baseURL string) (*acp.Server, error) {
	// Environment values can arrive with surrounding whitespace from shell
	// profiles; normalize before validating so padded values do not fail at
	// request time.
	apiKey = strings.TrimSpace(apiKey)
	modelName = strings.TrimSpace(modelName)
	baseURL = strings.TrimSpace(baseURL)
	if apiKey == "" {
		return nil, errors.New("OPENROUTER_API_KEY is required")
	}
	if modelName == "" {
		return nil, errors.New("OPENROUTER_MODEL is required")
	}
	if baseURL == "" {
		baseURL = "https://openrouter.ai/api/v1"
	}
	model, err := openai.New(openai.Config{BaseURL: baseURL, APIKey: apiKey, Model: modelName})
	if err != nil {
		return nil, err
	}
	registry, err := lebro.NewToolRegistry(jsonschema.NewCompiler())
	if err != nil {
		return nil, err
	}
	if err := registry.Register(inspectText{}); err != nil {
		return nil, err
	}
	server, err := acp.NewServer(acp.ServerConfig{
		AgentConfig: &lebro.AgentConfig{
			Definition: lebro.AgentDefinition{ID: "openrouter-acp", Name: "Lebro OpenRouter", Model: modelName,
				Instructions: "You are a helpful programming assistant. Answer accurately and use the available inspect_text tool when asked to inspect or count supplied text. Ask for clarification when necessary.",
				Tools:        []lebro.ToolID{"inspect_text"}},
			Model: model, Tools: registry, Store: lebro.NewMemoryStore(),
		},
		AgentInfo: acp.Implementation{Name: "lebro-openrouter", Version: "0.1.0"},
	})
	if err != nil {
		return nil, err
	}
	if err := server.ExposeAgent(); err != nil {
		return nil, err
	}
	return server, nil
}

func main() {
	server, err := newServer(os.Getenv("OPENROUTER_API_KEY"), os.Getenv("OPENROUTER_MODEL"), os.Getenv("OPENROUTER_BASE_URL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "acp-openrouter:", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := server.RunStdio(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "acp-openrouter:", err)
		os.Exit(1)
	}
}
