package lebro

import (
	"context"
	"errors"
	"testing"
	"time"
)

var _ DecisionModel = contractDecisionModel{}

// contractDecisionModel is a decision adapter written against the public
// façade. It proves that the exported decision contract is usable from
// outside the runtime package.
type contractDecisionModel struct{}

func (contractDecisionModel) ProviderID() ProviderID { return "contract" }

func (contractDecisionModel) Capabilities() DecisionCapabilities {
	return DecisionCapabilities{
		Noul:                   true,
		Choice:                 true,
		Score:                  true,
		MultipleQuestions:      true,
		StructuredState:        true,
		StructuredInstructions: true,
		Distributions:          true,
		Confidence:             true,
		MaxChoiceOptions:       255,
		MaxScoreLevels:         10,
	}
}

func (contractDecisionModel) Decide(_ context.Context, request DecisionRequest) (DecisionResponse, error) {
	if err := request.Validate(); err != nil {
		return DecisionResponse{}, &DecisionError{Kind: DecisionErrorInvalidRequest, Message: err.Error(), Err: err}
	}
	confidence := 0.9
	selected := 0
	expected := 1.25
	answers := make(map[DecisionQuestionID]DecisionAnswer, len(request.Questions))
	for id, question := range request.Questions {
		switch typed := question.(type) {
		case NoulQuestion:
			answers[id] = NoulAnswer{ProbabilityYes: 0.8, Confidence: &confidence}
		case ChoiceQuestion:
			options := make([]DecisionOptionID, 0, len(typed.Criteria))
			for option := range typed.Criteria {
				options = append(options, option)
			}
			probabilities := make(map[DecisionOptionID]float64, len(options))
			for index, option := range options {
				probabilities[option] = float64(index+1) / float64(len(options)+1)
			}
			answers[id] = ChoiceAnswer{Choice: options[0], Probabilities: probabilities, Confidence: &confidence}
		case ScoreQuestion:
			probabilities := make(map[int]float64, len(typed.Criteria))
			legend := make(map[int]JSONValue, len(typed.Criteria))
			for level := range typed.Criteria {
				probabilities[level] = 1 / float64(len(typed.Criteria))
				legend[level] = typed.Criteria[level]
			}
			answers[id] = ScoreAnswer{
				SelectedLevel: &selected,
				ExpectedLevel: &expected,
				Probabilities: probabilities,
				Legend:        legend,
				Confidence:    &confidence,
				Provenance:    ScoreNative,
			}
		}
	}
	usage := DecisionUsage{InputTokens: int64Pointer(12), OutputTokens: int64Pointer(3)}
	return DecisionResponse{
		Provider: "contract",
		Model:    "contract-model",
		Answers:  answers,
		Usage:    &usage,
		Duration: time.Millisecond,
		Attempts: 1,
	}, nil
}

func TestDecisionContract(t *testing.T) {
	model := contractDecisionModel{}
	request := DecisionRequest{
		State: "A customer asked for a refund three days after the purchase.",
		Questions: map[DecisionQuestionID]DecisionQuestion{
			"refund_eligible": NoulQuestion{
				Instructions: "Does this request qualify for a refund?",
				Criteria:     &NoulCriteria{True: "Qualifies", False: "Does not qualify"},
			},
			"department": ChoiceQuestion{
				Instructions: "Which team should handle this request?",
				Criteria: map[DecisionOptionID]JSONValue{
					"billing":   "Payment and refund questions",
					"technical": "Product faults and integrations",
				},
			},
			"urgency": ScoreQuestion{
				Instructions: "How urgent is this request?",
				Criteria:     []JSONValue{"Can wait", "Should be handled today"},
			},
		},
	}
	response, err := model.Decide(context.Background(), request)
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("Decide() response error = %v", err)
	}
	if response.Provider != model.ProviderID() {
		t.Fatalf("Decide() provider = %q, want %q", response.Provider, model.ProviderID())
	}
	if len(response.Answers) != len(request.Questions) {
		t.Fatalf("Decide() answered %d questions, want %d", len(response.Answers), len(request.Questions))
	}
	if _, ok := response.Answers["refund_eligible"].(NoulAnswer); !ok {
		t.Fatalf("Decide() noul answer = %#v, want a noul answer", response.Answers["refund_eligible"])
	}
	if _, ok := response.Answers["department"].(ChoiceAnswer); !ok {
		t.Fatalf("Decide() choice answer = %#v, want a choice answer", response.Answers["department"])
	}
	score, ok := response.Answers["urgency"].(ScoreAnswer)
	if !ok {
		t.Fatalf("Decide() score answer = %#v, want a score answer", response.Answers["urgency"])
	}
	if score.SelectedLevel == nil || score.ExpectedLevel == nil {
		t.Fatal("Decide() score answer lost the selected or expected level")
	}
	if score.Provenance != ScoreNative {
		t.Fatalf("Decide() provenance = %q, want %q", score.Provenance, ScoreNative)
	}
	if response.Usage == nil || response.Usage.InputTokens == nil || *response.Usage.InputTokens != 12 {
		t.Fatalf("Decide() usage = %#v, want 12 input tokens", response.Usage)
	}
}

func TestDecisionContractRejectsInvalidRequest(t *testing.T) {
	model := contractDecisionModel{}
	_, err := model.Decide(context.Background(), DecisionRequest{
		State:     "state",
		Questions: map[DecisionQuestionID]DecisionQuestion{},
	})
	if !errors.Is(err, ErrDecisionInvalidRequest) {
		t.Fatalf("Decide() error = %v, want an invalid request failure", err)
	}
}

func TestValidateDecisionImagesFacade(t *testing.T) {
	caps := DecisionImageCapabilities{
		Enabled:    true,
		MediaTypes: []string{"image/png"},
		MaxImages:  1,
	}
	images := []DecisionImage{{MediaType: "image/gif", Bytes: []byte("data")}}
	if err := ValidateDecisionImages(caps, images); !errors.Is(err, ErrDecisionUnsupportedCapability) {
		t.Fatalf("ValidateDecisionImages() error = %v, want an unsupported capability failure", err)
	}
}

func TestDecisionGateFacade(t *testing.T) {
	minimum := 0.8
	gate := DecisionGate{SelectProbability: 0.9, EscalateProbability: 0.1, MinimumConfidence: &minimum}
	if err := gate.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
	if got := gate.Noul(NoulAnswer{ProbabilityYes: 0.95, Confidence: float64Pointer(0.85)}); got != DecisionOutcomeSelect {
		t.Fatalf("Noul() = %q, want %q", got, DecisionOutcomeSelect)
	}
	if got := gate.Noul(NoulAnswer{ProbabilityYes: 0.5, Confidence: float64Pointer(0.85)}); got != DecisionOutcomeReview {
		t.Fatalf("Noul() = %q, want %q", got, DecisionOutcomeReview)
	}
	if got := gate.Noul(NoulAnswer{ProbabilityYes: 0.05, Confidence: float64Pointer(0.85)}); got != DecisionOutcomeEscalate {
		t.Fatalf("Noul() = %q, want %q", got, DecisionOutcomeEscalate)
	}
}

// float64Pointer returns a pointer to a copy of value.
func float64Pointer(value float64) *float64 { return &value }

// int64Pointer returns a pointer to a copy of value.
func int64Pointer(value int64) *int64 { return &value }
