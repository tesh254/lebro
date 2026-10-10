// tool-approval shows a model-requested side effect pausing at a durable,
// reviewed boundary. The fixture model and in-memory store keep it network-free.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

type transferTool struct{}

func (transferTool) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{
		ID:          "payments.transfer",
		Description: "Transfer funds to an approved recipient",
		InputSchema: json.RawMessage(`{"type":"object","required":["amount","recipient"],"properties":{"amount":{"type":"number"},"recipient":{"type":"string"}}}`),
	}
}

func (transferTool) Execute(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"status":"sent","reviewed_arguments":` + string(input) + `}`), nil
}

type fixtureModel struct{ step int }

func (m *fixtureModel) Generate(_ context.Context, _ lebro.ModelRequest) (lebro.ModelResponse, error) {
	m.step++
	if m.step == 1 {
		calls, err := lebro.NewModelToolCalls(lebro.ModelToolCall{ID: "transfer-1", ToolID: "payments.transfer", Arguments: json.RawMessage(`{"amount":1250,"recipient":"Amina"}`)})
		if err != nil {
			return lebro.ModelResponse{}, err
		}
		return lebro.ModelResponse{Message: lebro.Message{Role: lebro.RoleAssistant, ToolCalls: calls}, FinishReason: lebro.FinishReasonToolCalls}, nil
	}
	return lebro.ModelResponse{Message: lebro.Message{Role: lebro.RoleAssistant, Content: "The transfer was approved and sent."}, FinishReason: lebro.FinishReasonStop}, nil
}

func main() { must(run(os.Stdout)) }

func run(output io.Writer) error {
	registry, err := lebro.NewToolRegistry(lebrojsonschema.NewCompiler())
	if err != nil {
		return err
	}
	if err := registry.Register(transferTool{}); err != nil {
		return err
	}
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "payments-agent", Instructions: "Use the transfer tool only when requested.", Tools: []lebro.ToolID{"payments.transfer"}},
		Model:      &fixtureModel{},
		Tools:      registry,
		Store:      lebro.NewMemoryStore(),
		ToolApprovalPolicy: lebro.ToolApprovalPolicyFunc(func(_ context.Context, invocation lebro.ToolApprovalInvocation) (lebro.ToolApprovalResolution, error) {
			if invocation.ToolID == "payments.transfer" {
				return lebro.ToolApprovalResolution{Outcome: lebro.ToolApprovalRequire, Reason: "moves customer funds", TTL: 10 * time.Minute}, nil
			}
			return lebro.ToolApprovalResolution{Outcome: lebro.ToolApprovalAllow}, nil
		}),
	})
	if err != nil {
		return err
	}

	suspended, err := agent.Run(context.Background(), lebro.RunInput{RunID: "transfer-demo", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "Send KES 1,250 to Amina."}}})
	if err != nil {
		return err
	}
	if suspended.ToolApproval == nil {
		return errors.New("expected a pending tool approval")
	}
	request := suspended.ToolApproval
	fmt.Fprintf(output, "pending: %s %s\n", request.ToolID, request.Arguments)

	result, err := agent.ResumeToolApproval(context.Background(), lebro.ToolApprovalDecision{RunID: suspended.ID, RequestID: request.ID, Approved: true, Decider: "operations", DecidedAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "status: %s\n", result.Status)
	fmt.Fprintf(output, "assistant: %s\n", result.Messages[len(result.Messages)-1].Content)
	return nil
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
