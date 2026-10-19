// The decision-provider example builds a decision provider from
// configuration alone, the way an integration would for a System One
// provider that has no named adapter. The provider is pointed at a local
// endpoint, so the example runs without a network call or an API key.
//
// The named adapters in the decisions package are the same adapter with a
// different configuration: only the wire contract changes, never the
// application code.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/decisions"
)

func main() {
	must(run(os.Stdout))
}

func run(output io.Writer) error {
	server := newFakeProvider()
	defer server.Close()

	// The provider's wire contract, and nothing else. The endpoint, the
	// credential placement, the model, the score field names, the
	// capabilities, and the retryable statuses are all configuration.
	model, err := decisions.NewSystemOne(decisions.SystemOneConfig{
		APIKey:     "example-key",
		BaseURL:    server.URL,
		AuthHeader: "X-Example-Key",
		Model:      "example-model",
		Score:      decisions.SystemOneScoreFields{Weighted: "expected_level", Winner: "score"},
		Capabilities: lebro.DecisionCapabilities{
			Noul:                   true,
			Choice:                 true,
			Score:                  true,
			MultipleQuestions:      true,
			StructuredState:        true,
			StructuredInstructions: true,
			Distributions:          true,
			Confidence:             true,
			MaxQuestions:           8,
			MaxChoiceOptions:       16,
			MaxScoreLevels:         5,
		},
		RetryableStatuses: []int{http.StatusTooManyRequests, http.StatusServiceUnavailable},
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(output, "provider: %s\n", model.ProviderID())

	response, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State: "My card was charged twice for order A-104.",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"is_refund": lebro.NoulQuestion{
				Instructions: "Does this request qualify for a refund?",
				Criteria:     &lebro.NoulCriteria{True: "Qualifies", False: "Does not qualify"},
			},
			"department": lebro.ChoiceQuestion{
				Instructions: "Which team should handle this request?",
				Criteria: map[lebro.DecisionOptionID]lebro.JSONValue{
					"billing":   "Payment and refund questions",
					"technical": "Product faults and integrations",
				},
			},
			"urgency": lebro.ScoreQuestion{
				Instructions: "How urgent is this request?",
				Criteria:     []lebro.JSONValue{"Can wait", "Handle today", "Immediate"},
			},
		},
	})
	if err != nil {
		return err
	}

	noul, ok := response.Answers["is_refund"].(lebro.NoulAnswer)
	if !ok {
		return fmt.Errorf("expected a noul answer, got %T", response.Answers["is_refund"])
	}
	choice, ok := response.Answers["department"].(lebro.ChoiceAnswer)
	if !ok {
		return fmt.Errorf("expected a choice answer, got %T", response.Answers["department"])
	}
	score, ok := response.Answers["urgency"].(lebro.ScoreAnswer)
	if !ok {
		return fmt.Errorf("expected a score answer, got %T", response.Answers["urgency"])
	}

	fmt.Fprintf(output, "refund: probability=%.2f confidence=%s\n",
		noul.ProbabilityYes, formatConfidence(noul.Confidence))
	fmt.Fprintf(output, "department: %s\n", choice.Choice)
	fmt.Fprintf(output, "urgency: expected=%.2f selected=%s\n",
		*score.ExpectedLevel, formatLevel(score.SelectedLevel))
	return nil
}

// newFakeProvider serves the answers the configuration above describes. It
// reports a winning score level and a separate weighted value, the way a
// provider that names both score fields does.
func newFakeProvider() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "example-model-2026",
			"answers": {
				"is_refund": {"type": "noul", "noul": 0.93, "confidence": 0.86},
				"department": {
					"type": "choice",
					"choice": "billing",
					"probabilities": {"billing": 0.82, "technical": 0.18},
					"confidence": 0.64
				},
				"urgency": {
					"type": "score",
					"score": 1,
					"expected_level": 1.4,
					"confidence": 0.71,
					"probabilities": {"0": 0.2, "1": 0.2, "2": 0.6},
					"legend": {"0": "Can wait", "1": "Handle today", "2": "Immediate"}
				}
			},
			"usage": {"input_tokens": 128, "output_tokens": 12}
		}`))
	}))
}

// formatConfidence renders an optional confidence.
func formatConfidence(confidence *float64) string {
	if confidence == nil {
		return "none"
	}
	return fmt.Sprintf("%.2f", *confidence)
}

// formatLevel renders an optional winning level.
func formatLevel(level *int) string {
	if level == nil {
		return "none"
	}
	return fmt.Sprintf("%d", *level)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
