// The acp-agent example serves a lebro agent over the Agent Client Protocol
// v1 on stdio, so an ACP-compatible editor can launch it as a subprocess.
//
// The fixture model and in-memory store keep it network-free: the agent asks
// for approval of a protected "payments.refund" tool call through the ACP
// permission flow, streams text, and preserves context across turns.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"

	"github.com/tesh254/lebro/acp"
)

// refundTool is the protected side effect the editor reviews.
type refundTool struct{}

func (refundTool) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{
		ID:          "payments.refund",
		Description: "Refund a payment to a customer",
		InputSchema: json.RawMessage(`{"type":"object","required":["order_id","amount"],"properties":{"order_id":{"type":"string"},"amount":{"type":"number"}}}`),
	}
}

func (refundTool) Execute(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"status":"refunded"}`), nil
}

// demoModel scripts a refund request followed by a confirmation turn. In a
// real deployment this is a provider model; the ACP adapter is model-neutral.
type demoModel struct {
	mu   sync.Mutex
	step int
}

func (m *demoModel) Generate(_ context.Context, _ lebro.ModelRequest) (lebro.ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.step++
	if m.step == 1 {
		calls, err := lebro.NewModelToolCalls(lebro.ModelToolCall{
			ID:        "refund-1",
			ToolID:    "payments.refund",
			Arguments: json.RawMessage(`{"order_id":"A-1024","amount":42}`),
		})
		if err != nil {
			return lebro.ModelResponse{}, err
		}
		return lebro.ModelResponse{Message: lebro.Message{Role: lebro.RoleAssistant, ToolCalls: calls}, FinishReason: lebro.FinishReasonToolCalls}, nil
	}
	return lebro.ModelResponse{
		Message:      lebro.Message{Role: lebro.RoleAssistant, Content: "The refund was approved and issued."},
		FinishReason: lebro.FinishReasonStop,
	}, nil
}

func main() {
	verbose := flag.Bool("verbose", false, "log protocol diagnostics to stderr")
	flag.Parse()

	if err := run(os.Stdout, os.Stderr, *verbose); err != nil {
		fmt.Fprintf(os.Stderr, "acp-agent: %v\n", err)
		os.Exit(1)
	}
}

func run(stdout, stderr io.Writer, verbose bool) error {
	registry, err := lebro.NewToolRegistry(lebrojsonschema.NewCompiler())
	if err != nil {
		return err
	}
	if err := registry.Register(refundTool{}); err != nil {
		return err
	}

	agentConfig := &lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:           "refund-agent",
			Name:         "Refund Agent",
			Instructions: "Issue refunds when the customer asks. The refund tool requires approval.",
			Tools:        []lebro.ToolID{"payments.refund"},
		},
		Model: &demoModel{},
		Tools: registry,
		Store: lebro.NewMemoryStore(),
	}

	logger := slog.New(slog.NewTextHandler(stderr, nil))
	if !verbose {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	server, err := acp.NewServer(acp.ServerConfig{
		AgentConfig: agentConfig,
		AgentInfo:   acp.Implementation{Name: "lebro-acp-example", Version: "1.0.0"},
		Logger:      logger,
	})
	if err != nil {
		return err
	}
	if err := server.ExposeAgent(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	fmt.Fprintf(stderr, "acp-agent: serving ACP v1 on stdio (connect with an ACP-compatible editor or client)\n")
	// RunStdio owns the process stdin and stdout; diagnostics go to stderr.
	go func() {
		<-ctx.Done()
		// Closing stdin ends the serving loop.
		_ = os.Stdin.Close()
	}()
	if err := server.RunStdio(ctx); err != nil {
		return err
	}
	_ = acpsdk.ProtocolVersionNumber // keep the SDK import meaningful for readers
	return nil
}
