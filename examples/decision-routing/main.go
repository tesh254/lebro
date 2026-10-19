// The decision-routing example routes a support request with a decision
// model, gates an automated action on a confidence threshold, and combines
// score answers with weights the application owns.
//
// The decision model is an in-file stand-in for a hosted provider, so the
// example runs without a network call or an API key. A real integration
// swaps in decisions.NewTypeSafe, decisions.NewFastino, or decisions.NewSubQ
// and changes configuration only.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/tesh254/lebro"
)

func main() {
	must(run(os.Stdout))
}

func run(output io.Writer) error {
	model := ticketDecisionModel{}
	state := "My card was charged twice for order A-104 and I need a refund today."

	// A choice answer selects a route. The provider returns the option and
	// the distribution over every requested option.
	route, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State: state,
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"department": lebro.ChoiceQuestion{
				Instructions: "Which team should handle this request?",
				Criteria: map[lebro.DecisionOptionID]lebro.JSONValue{
					"billing":   "Payment, invoice, and refund questions",
					"technical": "Product faults and integrations",
					"shipping":  "Delivery and tracking",
				},
			},
		},
	})
	if err != nil {
		return err
	}
	choice, ok := route.Answers["department"].(lebro.ChoiceAnswer)
	if !ok {
		return fmt.Errorf("expected a choice answer, got %T", route.Answers["department"])
	}
	fmt.Fprintf(output, "route: %s\n", choice.Choice)
	for _, option := range sortedOptions(choice.Probabilities) {
		fmt.Fprintf(output, "  %-10s %.3f\n", option, choice.Probabilities[option])
	}

	// A noul answer is a probability, not an action. The application owns the
	// thresholds, and the gate turns the answer into a recommendation.
	eligibility, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State: state,
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"is_refund": lebro.NoulQuestion{
				Instructions: "Does this request qualify for a refund?",
				Criteria:     &lebro.NoulCriteria{True: "Qualifies", False: "Does not qualify"},
			},
		},
	})
	if err != nil {
		return err
	}
	noul, ok := eligibility.Answers["is_refund"].(lebro.NoulAnswer)
	if !ok {
		return fmt.Errorf("expected a noul answer, got %T", eligibility.Answers["is_refund"])
	}
	minimumConfidence := 0.7
	gate := lebro.DecisionGate{
		SelectProbability:   0.85,
		EscalateProbability: 0.30,
		MinimumConfidence:   &minimumConfidence,
		ConfidenceMetric:    lebro.DecisionConfidenceUnknown,
	}
	if err := gate.Validate(); err != nil {
		return err
	}
	outcome := gate.Noul(noul)
	fmt.Fprintf(output, "eligibility: probability=%.3f outcome=%s\n", noul.ProbabilityYes, outcome)
	if outcome != lebro.DecisionOutcomeSelect {
		fmt.Fprint(output, "recommendation: route to a human review queue\n")
	} else {
		// The gate recommends; it does not authorize. The refund policy
		// still decides, and the approval path still runs.
		fmt.Fprint(output, "recommendation: refund eligible, route to the refund approval policy\n")
	}

	// Composite scoring combines two independent score answers with weights
	// the application owns. The model supplies the levels; the application
	// supplies the weights and the threshold.
	scores, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State: state,
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"urgency": lebro.ScoreQuestion{
				Instructions: "How urgent is this request?",
				Criteria:     []lebro.JSONValue{"Can wait", "Handle today", "Immediate"},
			},
			"impact": lebro.ScoreQuestion{
				Instructions: "How much does this affect the customer?",
				Criteria:     []lebro.JSONValue{"Minor", "Noticeable", "Blocking"},
			},
		},
	})
	if err != nil {
		return err
	}
	urgency, ok := scores.Answers["urgency"].(lebro.ScoreAnswer)
	if !ok {
		return fmt.Errorf("expected a score answer, got %T", scores.Answers["urgency"])
	}
	impact, ok := scores.Answers["impact"].(lebro.ScoreAnswer)
	if !ok {
		return fmt.Errorf("expected a score answer, got %T", scores.Answers["impact"])
	}
	combined, ok := compositeScore(urgency, impact, 0.6, 0.4)
	if !ok {
		return fmt.Errorf("expected both score answers to report a weighted level")
	}
	fmt.Fprintf(output, "composite: urgency=%.2f impact=%.2f combined=%.2f\n",
		*urgency.ExpectedLevel, *impact.ExpectedLevel, combined)
	// A decision can also select the handler. The option identifiers are the
	// application's own, so switching from a deterministic reply to a
	// specialist agent to a human is a configuration change, not a rewrite.
	handling, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State: state,
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"handler": lebro.ChoiceQuestion{
				Instructions: "Who should handle this request?",
				Criteria: map[lebro.DecisionOptionID]lebro.JSONValue{
					"auto_reply":       "A deterministic reply handles it",
					"specialist_agent": "A specialist agent handles it",
					"human_review":     "A person reviews it",
				},
			},
		},
	})
	if err != nil {
		return err
	}
	handler, ok := handling.Answers["handler"].(lebro.ChoiceAnswer)
	if !ok {
		return fmt.Errorf("expected a choice answer, got %T", handling.Answers["handler"])
	}
	// The application owns the effect. A high composite score sends the
	// request to a person, so the handler is a human review even though the
	// decision model would pick a specialist.
	selected := handler.Choice
	if combined >= 1.5 {
		selected = "human_review"
		fmt.Fprint(output, "action: escalate to a human, the combined score is high\n")
	} else {
		fmt.Fprint(output, "action: handle on the normal queue\n")
	}
	switch selected {
	case "auto_reply":
		fmt.Fprint(output, "handler: deterministic reply\n")
	case "specialist_agent":
		fmt.Fprint(output, "handler: specialist agent\n")
	default:
		fmt.Fprint(output, "handler: human review\n")
	}
	return nil
}

// compositeScore combines two score answers with weights the application
// owns. The weighted value stays a number the caller interprets; the model
// never decides what it means.
func compositeScore(primary, secondary lebro.ScoreAnswer, primaryWeight, secondaryWeight float64) (float64, bool) {
	if primary.ExpectedLevel == nil || secondary.ExpectedLevel == nil {
		return 0, false
	}
	total := primaryWeight + secondaryWeight
	if total == 0 {
		return 0, false
	}
	return (primaryWeight*(*primary.ExpectedLevel) + secondaryWeight*(*secondary.ExpectedLevel)) / total, true
}

// ticketDecisionModel is an in-file stand-in for a hosted decision provider.
type ticketDecisionModel struct{}

func (ticketDecisionModel) ProviderID() lebro.ProviderID { return "example" }

func (ticketDecisionModel) Capabilities() lebro.DecisionCapabilities {
	return lebro.DecisionCapabilities{
		Noul:                   true,
		Choice:                 true,
		Score:                  true,
		MultipleQuestions:      true,
		StructuredState:        true,
		StructuredInstructions: true,
		MaxChoiceOptions:       255,
		MaxScoreLevels:         10,
		NoulUncertainty: lebro.DecisionUncertainty{
			Distribution: lebro.DecisionAvailabilityUnavailable,
			Confidence:   lebro.DecisionAvailabilityRequired,
			// The stand-in reports a number without documenting what it
			// means, so the metric says exactly that.
			ConfidenceMetric: lebro.DecisionConfidenceUnknown,
		},
		ChoiceUncertainty: lebro.DecisionUncertainty{
			Distribution:     lebro.DecisionAvailabilityRequired,
			Confidence:       lebro.DecisionAvailabilityRequired,
			ConfidenceMetric: lebro.DecisionConfidenceUnknown,
		},
		ScoreUncertainty: lebro.DecisionUncertainty{
			Distribution:     lebro.DecisionAvailabilityRequired,
			Confidence:       lebro.DecisionAvailabilityRequired,
			ConfidenceMetric: lebro.DecisionConfidenceUnknown,
		},
	}
}

// CapabilitiesFor reports the capabilities of the stand-in's only model.
func (m ticketDecisionModel) CapabilitiesFor(model string) (lebro.DecisionCapabilities, bool) {
	if model != "" && model != "example-model" {
		return lebro.DecisionCapabilities{}, false
	}
	return m.Capabilities(), true
}

func (m ticketDecisionModel) Decide(_ context.Context, request lebro.DecisionRequest) (lebro.DecisionResponse, error) {
	if err := request.Validate(); err != nil {
		return lebro.DecisionResponse{}, &lebro.DecisionError{Kind: lebro.DecisionErrorInvalidRequest, Message: err.Error(), Err: err}
	}
	// The fixture enforces the limits it advertises, so the example never
	// claims a capability it does not check.
	capabilities := m.Capabilities()
	if capabilities.MaxQuestions > 0 && len(request.Questions) > capabilities.MaxQuestions {
		return lebro.DecisionResponse{}, &lebro.DecisionError{
			Kind:     lebro.DecisionErrorInvalidRequest,
			Provider: string(m.ProviderID()),
			Message:  fmt.Sprintf("the fixture answers at most %d questions per request", capabilities.MaxQuestions),
		}
	}
	answers := make(map[lebro.DecisionQuestionID]lebro.DecisionAnswer, len(request.Questions))
	for id, question := range request.Questions {
		switch typed := question.(type) {
		case lebro.NoulQuestion:
			confidence := 0.91
			answers[id] = lebro.NoulAnswer{
				ProbabilityYes:   0.93,
				Confidence:       &confidence,
				ConfidenceMetric: lebro.DecisionConfidenceUnknown,
			}
		case lebro.ChoiceQuestion:
			if capabilities.MaxChoiceOptions > 0 && len(typed.Criteria) > capabilities.MaxChoiceOptions {
				return lebro.DecisionResponse{}, &lebro.DecisionError{
					Kind:     lebro.DecisionErrorInvalidRequest,
					Provider: string(m.ProviderID()),
					Message:  fmt.Sprintf("the fixture answers at most %d options per choice", capabilities.MaxChoiceOptions),
				}
			}
			options := sortedOptionIDs(typed.Criteria)
			selected := options[0]
			// The fixture routes to a specialist whenever the caller offers
			// one, and to the first option in a stable order otherwise.
			if _, offered := typed.Criteria["specialist_agent"]; offered {
				selected = "specialist_agent"
			}
			probabilities := make(map[lebro.DecisionOptionID]float64, len(options))
			rest := 0.3 / float64(len(options)-1)
			for _, option := range options {
				if option == selected {
					continue
				}
				probabilities[option] = rest
			}
			probabilities[selected] = 0.7
			confidence := 0.62
			answers[id] = lebro.ChoiceAnswer{
				Choice:           selected,
				Probabilities:    probabilities,
				Confidence:       &confidence,
				ConfidenceMetric: lebro.DecisionConfidenceUnknown,
			}
		case *lebro.NoulQuestion, *lebro.ChoiceQuestion, *lebro.ScoreQuestion:
			return lebro.DecisionResponse{}, fmt.Errorf("the fixture answers value question forms only")
		case lebro.ScoreQuestion:
			if capabilities.MaxScoreLevels > 0 && len(typed.Criteria) > capabilities.MaxScoreLevels {
				return lebro.DecisionResponse{}, &lebro.DecisionError{
					Kind:     lebro.DecisionErrorInvalidRequest,
					Provider: string(m.ProviderID()),
					Message:  fmt.Sprintf("the fixture answers at most %d levels per score", capabilities.MaxScoreLevels),
				}
			}
			levels := len(typed.Criteria)
			probabilities := make(map[int]float64, levels)
			legend := make(map[int]lebro.JSONValue, levels)
			rest := 0.2 / float64(levels-1)
			for level := range typed.Criteria {
				probabilities[level] = rest
				legend[level] = typed.Criteria[level]
			}
			probabilities[levels-1] = 0.8
			expected := 0.0
			for level, probability := range probabilities {
				expected += float64(level) * probability
			}
			selected := levels - 1
			confidence := 0.74
			answers[id] = lebro.ScoreAnswer{
				SelectedLevel:    &selected,
				ExpectedLevel:    &expected,
				Probabilities:    probabilities,
				Legend:           legend,
				Confidence:       &confidence,
				ConfidenceMetric: lebro.DecisionConfidenceUnknown,
				Provenance:       lebro.ScoreNative,
			}
		}
	}
	return lebro.DecisionResponse{
		Provider: m.ProviderID(),
		Model:    "example-model",
		Answers:  answers,
		Attempts: 1,
	}, nil
}

// sortedOptionIDs returns the option identifiers in a stable order.
func sortedOptionIDs(criteria map[lebro.DecisionOptionID]lebro.JSONValue) []lebro.DecisionOptionID {
	options := make([]lebro.DecisionOptionID, 0, len(criteria))
	for option := range criteria {
		options = append(options, option)
	}
	sort.Slice(options, func(i, j int) bool { return options[i] < options[j] })
	return options
}

// sortedOptions returns the option identifiers in a stable order.
func sortedOptions(probabilities map[lebro.DecisionOptionID]float64) []lebro.DecisionOptionID {
	options := make([]lebro.DecisionOptionID, 0, len(probabilities))
	for option := range probabilities {
		options = append(options, option)
	}
	sort.Slice(options, func(i, j int) bool { return options[i] < options[j] })
	return options
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
