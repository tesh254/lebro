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
	"math"
	"os"
	"strings"
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
			model:      guardDecisionModel{probability: 0.95},
			action:     "payments.transfer",
			wantStatus: "approved",
			wantCalls:  1,
		},
		{
			name:       "uncertain",
			model:      guardDecisionModel{probability: 0.55},
			action:     "payments.transfer",
			wantStatus: "needs-review",
			wantCalls:  0,
		},
		{
			name:       "denied",
			model:      guardDecisionModel{probability: 0.05},
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
			name:       "provider timeout",
			model:      timeoutDecisionModel{},
			action:     "payments.transfer",
			wantStatus: "denied",
			wantCalls:  0,
		},
		{
			name:       "tool outside the allowlist",
			model:      guardDecisionModel{probability: 0.95},
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
		case err == nil && result.ToolApproval != nil:
			status = "needs-review"
		case err == nil:
			status = "approved"
		case errors.Is(err, lebro.ErrToolApprovalDenied):
			status = "denied"
		default:
			// An unrelated run failure is not a denial, and it must not be
			// reported as one.
			return fmt.Errorf("%s: the run failed for an unrelated reason: %w", scenario.name, err)
		}
		calls := counter.total()
		fmt.Fprintf(output, "%-26s status=%-12s handler_calls=%d\n", scenario.name, status, calls)
		if result.ToolApproval != nil {
			// The pending approval is bound to the exact tool call that was
			// reviewed, so a reviewer approves these arguments and not a
			// replacement payload.
			fmt.Fprintf(output, "%-26s pending=%s %s\n", "", result.ToolApproval.ToolID, string(result.ToolApproval.Arguments))
		}
		if status != scenario.wantStatus {
			return fmt.Errorf("%s: status = %q, want %q", scenario.name, status, scenario.wantStatus)
		}
		if calls != scenario.wantCalls {
			return fmt.Errorf("%s: the handler ran %d times, want %d", scenario.name, calls, scenario.wantCalls)
		}
	}

	// A semantic compaction trigger. The deterministic context budget
	// supplies the limit, and the decision supplies the signal that a
	// transcript is worth summarizing. The application owns both, and lebro
	// performs the compaction itself once the run exceeds its budget.
	// The budget is one lebro accepts: the reserves fit inside the window
	// and the target fraction is below the trigger fraction.
	budget := lebro.ContextBudget{
		ContextWindowTokens:     4000,
		OutputReserveTokens:     200,
		FutureToolReserveTokens: 200,
		TriggerFraction:         0.5,
		TargetFraction:          0.4,
	}
	decisionModel := guardDecisionModel{probability: 0.92}
	short, err := compactionTrigger(decisionModel, "a short transcript", budget)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "%-26s compact=%v\n", "compaction under budget", short)
	long, err := compactionTrigger(decisionModel, strings.Repeat("the customer keeps replying about the same refund. ", 200), budget)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "%-26s compact=%v\n", "compaction over budget", long)
	return nil
}

// compactionTrigger combines a decision signal with a deterministic budget.
// A decision alone never compacts a transcript: the budget decides whether
// the transcript is over its trigger, and the decision decides whether the
// content is worth summarizing.
func compactionTrigger(model lebro.DecisionModel, state string, budget lebro.ContextBudget) (bool, error) {
	usable := budget.ContextWindowTokens - budget.OutputReserveTokens - budget.FutureToolReserveTokens
	if usable <= 0 || budget.TriggerFraction <= 0 {
		// An unusable budget is a misconfiguration, and it must be visible
		// rather than silently disabling compaction.
		return false, errors.New("the context budget has no usable capacity")
	}
	// A rough character estimate stands in for a tokenizer. A real
	// application counts the tokens it will actually send.
	usedTokens := int64(len(state) / 4)
	if usedTokens <= int64(float64(usable)*budget.TriggerFraction) {
		// The transcript is still inside its budget, so there is nothing to
		// compact, whatever the decision says.
		return false, nil
	}
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
	gate := lebro.DecisionGate{
		SelectProbability:   0.7,
		EscalateProbability: 0.3,
		MinimumConfidence:   &minimumConfidence,
		// A confidence threshold is only comparable with a threshold
		// written against the same measure.
		ConfidenceMetric: lebro.DecisionConfidenceDistanceFromHalf,
	}
	return gate.Noul(answer) == lebro.DecisionOutcomeSelect, nil
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
		if ctx.Err() != nil {
			// A cancelled or expired call is not a denial. The run reports
			// the cancellation instead of a policy decision.
			return lebro.ToolApprovalResolution{}, err
		}
		// The reason is fixed: a provider error can carry request content
		// or provider detail, and it does not belong in an approval
		// record. The typed error stays available for classification.
		return lebro.ToolApprovalResolution{
			Outcome: lebro.ToolApprovalDeny,
			Reason:  "the decision provider failed",
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
		ConfidenceMetric:    lebro.DecisionConfidenceDistanceFromHalf,
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
	if t.id == "files.delete" {
		return lebro.ToolDefinition{
			ID:          t.id,
			Description: "Delete a file",
			InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`),
		}
	}
	return lebro.ToolDefinition{
		ID:          t.id,
		Description: "A tool that moves customer funds",
		InputSchema: json.RawMessage(`{"type":"object","required":["amount","recipient"],"properties":{"amount":{"type":"number"},"recipient":{"type":"string"}}}`),
	}
}

func (t *countingTool) Execute(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
	t.counter.calls[string(t.id)]++
	if t.id == "files.delete" {
		return json.RawMessage(`{"status":"deleted","reviewed_arguments":` + string(input) + `}`), nil
	}
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
}

// confidence is the metric the stand-in reports: the distance of the
// probability from one half, doubled so that it runs from zero to one.
//
// The stand-in computes it rather than carrying a number of its own, so the
// metric it names is one its numbers really mean.
func (m guardDecisionModel) confidence() float64 {
	return math.Abs(2*m.probability - 1)
}

func (guardDecisionModel) ProviderID() lebro.ProviderID { return "example" }

func (guardDecisionModel) Capabilities() lebro.DecisionCapabilities {
	return lebro.DecisionCapabilities{
		Noul: true,
		NoulUncertainty: lebro.DecisionUncertainty{
			Distribution: lebro.DecisionAvailabilityUnavailable,
			Confidence:   lebro.DecisionAvailabilityRequired,
			// The metric is the one confidence() computes, so the gate
			// binds its threshold to a measure the numbers mean.
			ConfidenceMetric: lebro.DecisionConfidenceDistanceFromHalf,
		},
	}
}

// CapabilitiesFor reports the capabilities of the stand-in's only model.
func (m guardDecisionModel) CapabilitiesFor(model string) (lebro.DecisionCapabilities, bool) {
	if model != "" && model != "example-model" {
		return lebro.DecisionCapabilities{}, false
	}
	return m.Capabilities(), true
}

func (m guardDecisionModel) Decide(_ context.Context, request lebro.DecisionRequest) (lebro.DecisionResponse, error) {
	if _, ok := m.CapabilitiesFor(request.Model); !ok {
		return lebro.DecisionResponse{}, &lebro.DecisionError{
			Kind:     lebro.DecisionErrorUnsupportedCapability,
			Provider: string(m.ProviderID()),
			Message:  "the example provider has no profile for that model",
		}
	}
	confidence := m.confidence()
	answers := make(map[lebro.DecisionQuestionID]lebro.DecisionAnswer, len(request.Questions))
	for id := range request.Questions {
		answers[id] = lebro.NoulAnswer{
			ProbabilityYes:   m.probability,
			Confidence:       &confidence,
			ConfidenceMetric: lebro.DecisionConfidenceDistanceFromHalf,
		}
	}
	return lebro.DecisionResponse{
		Provider: m.ProviderID(),
		Model:    "example-model",
		Answers:  answers,
		Attempts: 1,
	}, nil
}

// timeoutDecisionModel always times out, the way a provider that does not
// answer in time does.
type timeoutDecisionModel struct{}

func (timeoutDecisionModel) ProviderID() lebro.ProviderID { return "example" }

func (timeoutDecisionModel) Capabilities() lebro.DecisionCapabilities {
	return lebro.DecisionCapabilities{Noul: true}
}

// CapabilitiesFor reports the capabilities of the stand-in's only model.
func (m timeoutDecisionModel) CapabilitiesFor(model string) (lebro.DecisionCapabilities, bool) {
	if model != "" && model != "example-model" {
		return lebro.DecisionCapabilities{}, false
	}
	return m.Capabilities(), true
}

func (timeoutDecisionModel) Decide(_ context.Context, _ lebro.DecisionRequest) (lebro.DecisionResponse, error) {
	return lebro.DecisionResponse{}, &lebro.DecisionError{
		Kind:     lebro.DecisionErrorTimeout,
		Provider: "example",
		Message:  "the decision provider did not answer in time",
	}
}

// failingDecisionModel always fails, the way an unreachable provider does.
type failingDecisionModel struct{}

func (failingDecisionModel) ProviderID() lebro.ProviderID { return "example" }

func (failingDecisionModel) Capabilities() lebro.DecisionCapabilities {
	return lebro.DecisionCapabilities{Noul: true}
}

// CapabilitiesFor reports the capabilities of the stand-in's only model.
func (m failingDecisionModel) CapabilitiesFor(model string) (lebro.DecisionCapabilities, bool) {
	if model != "" && model != "example-model" {
		return lebro.DecisionCapabilities{}, false
	}
	return m.Capabilities(), true
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
