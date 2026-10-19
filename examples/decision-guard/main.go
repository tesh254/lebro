// The decision-guard example puts a decision model in front of a dangerous
// tool. The tool handler runs only when the decision, the application's
// allowlist, and the durable approval boundary all agree. A denial, an
// uncertain answer, a timeout, or a provider failure never invokes the
// handler, and the reviewed payload stays bound to the exact tool call.
//
// The decision model and the generative model are in-file stand-ins, so the
// example runs without a network call or an API key.
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

func main() {
	must(run(os.Stdout))
}

func run(output io.Writer) error {
	scenarios := []struct {
		name       string
		model      lebro.DecisionModel
		action     string
		wantStatus string
		wantCalls  int
	}{
		{
			name:       "allowed",
			model:      guardDecisionModel{probability: 0.95, confidence: 0.9},
			action:     "payments.transfer",
			wantStatus: "approved",
			wantCalls:  1,
		},
		{
			name:       "uncertain",
			model:      guardDecisionModel{probability: 0.55, confidence: 0.9},
			action:     "payments.transfer",
			wantStatus: "needs-review",
			wantCalls:  0,
		},
		{
			name:       "denied",
			model:      guardDecisionModel{probability: 0.05, confidence: 0.9},
			action:     "payments.transfer",
			wantStatus: "denied",
			wantCalls:  0,
		},
		{
			name:       "provider failure",
			model:      failingDecisionModel{},
			action:     "payments.transfer",
			wantStatus: "denied",
			wantCalls:  0,
		},
		{
			name:       "tool outside the allowlist",
			model:      guardDecisionModel{probability: 0.95, confidence: 0.9},
			action:     "files.delete",
			wantStatus: "denied",
			wantCalls:  0,
		},
	}
	for _, scenario := range scenarios {
		counter := &toolCounter{calls: map[string]int{}}
		registry, err := lebro.NewToolRegistry(lebrojsonschema.NewCompiler())
		if err != nil {
			return err
		}
		for _, toolID := range []lebro.ToolID{"payments.transfer", "files.delete"} {
			if err := registry.Register(&countingTool{id: toolID, counter: counter}); err != nil {
				return err
			}
		}
		agent, err := lebro.NewAgent(lebro.AgentConfig{
			Definition: lebro.AgentDefinition{
				ID:    "payments-agent",
				Model: "example-model",
				Tools: []lebro.ToolID{"payments.transfer", "files.delete"},
			},
			Model: &transferRequestingModel{toolID: lebro.ToolID(scenario.action)},
			Tools: registry,
			Store: lebro.NewMemoryStore(),
			ToolApprovalPolicy: lebro.ToolApprovalPolicyFunc(func(ctx context.Context, invocation lebro.ToolApprovalInvocation) (lebro.ToolApprovalResolution, error) {
				return guard(ctx, scenario.model, invocation)
			}),
		})
		if err != nil {
			return err
		}

		status := "approved"
		result, err := agent.Run(context.Background(), lebro.RunInput{
			RunID:    lebro.RunID("guard-" + scenario.name),
			Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "Send KES 1,250 to Amina."}},
		})
		switch {
		case err != nil:
			status = "denied"
		case result.ToolApproval != nil:
			status = "needs-review"
		}
		calls := counter.total()
		fmt.Fprintf(output, "%-26s status=%-12s handler_calls=%d\n", scenario.name, status, calls)
		if status != scenario.wantStatus {
			return fmt.Errorf("%s: status = %q, want %q", scenario.name, status, scenario.wantStatus)
		}
		if calls != scenario.wantCalls {
			return fmt.Errorf("%s: the handler ran %d times, want %d", scenario.name, calls, scenario.wantCalls)
		}
	}

	// A semantic compaction trigger. The decision supplies the signal that a
	// transcript is worth summarizing, and the deterministic context budget
	// supplies the limit. The application owns both, and lebro performs the
	// compaction itself once the run exceeds its budget.
	trigger, err := compactionTrigger(guardDecisionModel{probability: 0.92, confidence: 0.88}, "a long support transcript", lebro.ContextBudget{
		ContextWindowTokens: 8000,
		OutputReserveTokens: 1000,
		TriggerFraction:     0.7,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "%-26s compact=%v\n", "compaction trigger", trigger)
	return nil
}

// compactionTrigger combines a decision signal with a deterministic budget.
// A decision alone never compacts a transcript: the budget decides whether
// there is room, and the decision decides whether the content is worth
// summarizing.
func compactionTrigger(model lebro.DecisionModel, state string, budget lebro.ContextBudget) (bool, error) {
	response, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State: state,
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"worth_summarizing": lebro.NoulQuestion{
				Instructions: "Is this transcript worth compacting?",
				Criteria: &lebro.NoulCriteria{
					True:  "The transcript has durable content worth keeping",
					False: "The transcript is short and adds nothing",
				},
			},
		},
	})
	if err != nil {
		return false, err
	}
	answer, ok := response.Answers["worth_summarizing"].(lebro.NoulAnswer)
	if !ok {
		return false, errors.New("the decision provider returned no usable answer")
	}
	minimumConfidence := 0.6
	gate := lebro.DecisionGate{SelectProbability: 0.7, EscalateProbability: 0.3, MinimumConfidence: &minimumConfidence}
	if gate.Noul(answer) != lebro.DecisionOutcomeSelect {
		return false, nil
	}
	usable := budget.ContextWindowTokens - budget.OutputReserveTokens
	return usable > 0 && budget.TriggerFraction > 0, nil
}

// allowlist is the set of tools the application permits a decision to
// authorize. The decision never widens it.
var allowlist = map[lebro.ToolID]bool{
	"payments.transfer": true,
}

// guard turns a decision answer into an approval outcome.
//
// It is deliberately conservative: anything other than a confident selection
// either denies the call or sends it to a human, and a provider failure never
// authorizes anything.
func guard(ctx context.Context, model lebro.DecisionModel, invocation lebro.ToolApprovalInvocation) (lebro.ToolApprovalResolution, error) {
	if !allowlist[invocation.ToolID] {
		return lebro.ToolApprovalResolution{
			Outcome: lebro.ToolApprovalDeny,
			Reason:  fmt.Sprintf("tool %q is not on the allowlist", invocation.ToolID),
		}, nil
	}
	response, err := model.Decide(ctx, lebro.DecisionRequest{
		State: string(invocation.Arguments),
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"is_safe": lebro.NoulQuestion{
				Instructions: "Is this transfer within the policy for the account?",
				Criteria: &lebro.NoulCriteria{
					True:  "The transfer is allowed by the policy",
					False: "The transfer breaks the policy",
				},
			},
		},
	})
	if err != nil {
		return lebro.ToolApprovalResolution{
			Outcome: lebro.ToolApprovalDeny,
			Reason:  "the decision provider failed: " + err.Error(),
		}, nil
	}
	answer, ok := response.Answers["is_safe"].(lebro.NoulAnswer)
	if !ok {
		return lebro.ToolApprovalResolution{
			Outcome: lebro.ToolApprovalDeny,
			Reason:  "the decision provider returned no usable answer",
		}, nil
	}
	minimumConfidence := 0.7
	gate := lebro.DecisionGate{
		SelectProbability:   0.85,
		EscalateProbability: 0.30,
		MinimumConfidence:   &minimumConfidence,
	}
	switch gate.Noul(answer) {
	case lebro.DecisionOutcomeSelect:
		return lebro.ToolApprovalResolution{Outcome: lebro.ToolApprovalAllow}, nil
	case lebro.DecisionOutcomeReview:
		// Uncertainty is not authorization. The call suspends and a human
		// reviews this exact payload.
		return lebro.ToolApprovalResolution{
			Outcome: lebro.ToolApprovalRequire,
			Reason:  "the decision was uncertain",
			TTL:     10 * time.Minute,
		}, nil
	default:
		return lebro.ToolApprovalResolution{
			Outcome: lebro.ToolApprovalDeny,
			Reason:  "the decision denied the action",
		}, nil
	}
}

// toolCounter records how often each handler ran.
type toolCounter struct {
	calls map[string]int
}

// total returns the number of handler invocations across every tool.
func (c *toolCounter) total() int {
	total := 0
	for _, calls := range c.calls {
		total += calls
	}
	return total
}

// countingTool is a tool whose handler records that it ran.
type countingTool struct {
	id      lebro.ToolID
	counter *toolCounter
}

func (t *countingTool) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{
		ID:          t.id,
		Description: "A tool that moves customer funds",
		InputSchema: json.RawMessage(`{"type":"object","required":["amount","recipient"],"properties":{"amount":{"type":"number"},"recipient":{"type":"string"}}}`),
	}
}

func (t *countingTool) Execute(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
	t.counter.calls[string(t.id)]++
	return json.RawMessage(`{"status":"sent","reviewed_arguments":` + string(input) + `}`), nil
}

// transferRequestingModel asks for one tool call.
type transferRequestingModel struct {
	toolID lebro.ToolID
	step   int
}

func (m *transferRequestingModel) Generate(_ context.Context, _ lebro.ModelRequest) (lebro.ModelResponse, error) {
	m.step++
	if m.step == 1 {
		calls, err := lebro.NewModelToolCalls(lebro.ModelToolCall{
			ID:        "transfer-1",
			ToolID:    m.toolID,
			Arguments: json.RawMessage(`{"amount":1250,"recipient":"Amina"}`),
		})
		if err != nil {
			return lebro.ModelResponse{}, err
		}
		return lebro.ModelResponse{
			Message:      lebro.Message{Role: lebro.RoleAssistant, ToolCalls: calls},
			FinishReason: lebro.FinishReasonToolCalls,
		}, nil
	}
	return lebro.ModelResponse{
		Message:      lebro.Message{Role: lebro.RoleAssistant, Content: "The transfer was approved and sent."},
		FinishReason: lebro.FinishReasonStop,
	}, nil
}

// guardDecisionModel answers a noul question with a fixed probability.
type guardDecisionModel struct {
	probability float64
	confidence  float64
}

func (guardDecisionModel) ProviderID() lebro.ProviderID { return "example" }

func (guardDecisionModel) Capabilities() lebro.DecisionCapabilities {
	return lebro.DecisionCapabilities{Noul: true, Confidence: true}
}

func (m guardDecisionModel) Decide(_ context.Context, request lebro.DecisionRequest) (lebro.DecisionResponse, error) {
	confidence := m.confidence
	answers := make(map[lebro.DecisionQuestionID]lebro.DecisionAnswer, len(request.Questions))
	for id := range request.Questions {
		answers[id] = lebro.NoulAnswer{ProbabilityYes: m.probability, Confidence: &confidence}
	}
	return lebro.DecisionResponse{
		Provider: m.ProviderID(),
		Model:    "example-model",
		Answers:  answers,
		Attempts: 1,
	}, nil
}

// failingDecisionModel always fails, the way an unreachable provider does.
type failingDecisionModel struct{}

func (failingDecisionModel) ProviderID() lebro.ProviderID { return "example" }

func (failingDecisionModel) Capabilities() lebro.DecisionCapabilities {
	return lebro.DecisionCapabilities{Noul: true}
}

func (failingDecisionModel) Decide(_ context.Context, _ lebro.DecisionRequest) (lebro.DecisionResponse, error) {
	return lebro.DecisionResponse{}, &lebro.DecisionError{
		Kind:     lebro.DecisionErrorUnavailable,
		Provider: "example",
		Message:  "the decision provider is unavailable",
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
