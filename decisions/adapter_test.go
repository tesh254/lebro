package decisions

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"testing"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
)

// errUnknownQuestionType marks a wire question the decoder does not know.
var errUnknownQuestionType = errors.New("decisions: unknown question type")

// decisionFixture describes one provider for the shared contract suite.
type decisionFixture struct {
	// name is the provider name.
	name string
	// newModel builds the adapter against a test server.
	newModel func(baseURL string) (lebro.DecisionModel, error)
	// scoreWeightedField and scoreWinnerField name the wire fields the
	// provider uses for a score answer.
	scoreWeightedField string
	scoreWinnerField   string
	// noulConfidence reports whether the provider reports a confidence for a
	// noul answer.
	noulConfidence bool
	// model is the model identity the provider reports.
	model string
}

// decisionFacts is the computed answer content for one question.
type decisionFacts struct {
	kind          string
	noul          float64
	confidence    *float64
	choice        string
	probabilities map[string]float64
	score         float64
	winner        *int
	legend        map[string]string
}

// decisionContractFactory builds a factory that serves one provider's wire
// body for a contract case.
func decisionContractFactory(fixture decisionFixture) testkit.DecisionFactory {
	return func(t *testing.T, contractCase testkit.DecisionCase) testkit.DecisionHarness {
		t.Helper()
		var mu sync.Mutex
		var observed lebro.DecisionRequest
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read request body: %v", err)
				return
			}
			decoded, err := decodeObservedDecisionRequest(body)
			if err != nil {
				t.Errorf("decode observed request: %v", err)
				return
			}
			mu.Lock()
			observed = decoded
			mu.Unlock()

			switch contractCase.Mode {
			case testkit.DecisionFailure:
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"detail":{"error_type":"unavailable","message":"the provider is busy"}}`))
			case testkit.DecisionCancellation:
				<-r.Context().Done()
			default:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(renderDecisionWire(contractCase.Request, fixture))
			}
		}))
		t.Cleanup(server.Close)

		model, err := fixture.newModel(server.URL)
		if err != nil {
			t.Fatalf("build the %s adapter: %v", fixture.name, err)
		}
		facts := computeDecisionFacts(contractCase.Request)
		return testkit.DecisionHarness{
			Model:    model,
			Expected: expectedDecisionResponse(contractCase.Request, facts, fixture),
			ObservedRequest: func() lebro.DecisionRequest {
				mu.Lock()
				defer mu.Unlock()
				return observed
			},
		}
	}
}

// computeDecisionFacts derives the answer content the providers report for a
// request. The values differ along the axis each answer measures, so a
// mapping bug cannot produce the same result as a correct one.
func computeDecisionFacts(request lebro.DecisionRequest) map[lebro.DecisionQuestionID]decisionFacts {
	facts := make(map[lebro.DecisionQuestionID]decisionFacts, len(request.Questions))
	for id, question := range request.Questions {
		switch typed := question.(type) {
		case lebro.NoulQuestion:
			facts[id] = decisionFacts{kind: "noul", noul: 0.92, confidence: floatPointer(0.84)}
		case lebro.ChoiceQuestion:
			options := sortedOptions(typed.Criteria)
			distribution := decisionDistribution(len(options))
			probabilities := make(map[string]float64, len(options))
			for index, option := range options {
				probabilities[option] = distribution[strconv.Itoa(index)]
			}
			// The selected option carries the peak, so it is the last option
			// in a stable order and not the first.
			facts[id] = decisionFacts{
				kind:          "choice",
				choice:        options[len(options)-1],
				probabilities: probabilities,
				confidence:    floatPointer(0.6),
			}
		case lebro.ScoreQuestion:
			levels := len(typed.Criteria)
			probabilities := decisionDistribution(levels)
			weighted := 0.0
			winner := 0
			best := -1.0
			for level := 0; level < levels; level++ {
				probability := probabilities[strconv.Itoa(level)]
				weighted += float64(level) * probability
				if probability > best {
					best = probability
					winner = level
				}
			}
			legend := make(map[string]string, levels)
			for level, description := range typed.Criteria {
				legend[strconv.Itoa(level)] = description.(string)
			}
			facts[id] = decisionFacts{
				kind:          "score",
				probabilities: probabilities,
				score:         weighted,
				winner:        &winner,
				legend:        legend,
				confidence:    floatPointer(0.55),
			}
		}
	}
	return facts
}

// decisionDistribution spreads a probability mass over count entries.
//
// The peak sits on the last index, not the first, so a fixture that always
// reports index zero as the winner cannot pass, and neither can one that
// always picks the first option.
func decisionDistribution(count int) map[string]float64 {
	probabilities := make(map[string]float64, count)
	if count == 1 {
		probabilities["0"] = 1
		return probabilities
	}
	rest := 0.4 / float64(count-1)
	for i := 0; i < count; i++ {
		if i == count-1 {
			probabilities[strconv.Itoa(i)] = 0.6
			continue
		}
		probabilities[strconv.Itoa(i)] = rest
	}
	return probabilities
}

// sortedOptions returns the option identifiers in a stable order.
func sortedOptions(criteria map[lebro.DecisionOptionID]lebro.JSONValue) []string {
	options := make([]string, 0, len(criteria))
	for option := range criteria {
		options = append(options, string(option))
	}
	sort.Strings(options)
	return options
}

// renderDecisionWire renders the provider response body for a request.
func renderDecisionWire(request lebro.DecisionRequest, fixture decisionFixture) []byte {
	facts := computeDecisionFacts(request)
	answers := make(map[string]any, len(facts))
	for id, fact := range facts {
		switch fact.kind {
		case "noul":
			answer := map[string]any{"type": "noul", "noul": fact.noul}
			if fixture.noulConfidence {
				answer["confidence"] = *fact.confidence
			}
			answers[string(id)] = answer
		case "choice":
			answer := map[string]any{
				"type":          "choice",
				"choice":        fact.choice,
				"probabilities": fact.probabilities,
			}
			if fact.confidence != nil {
				answer["confidence"] = *fact.confidence
			}
			answers[string(id)] = answer
		case "score":
			answer := map[string]any{
				"type":          "score",
				"probabilities": fact.probabilities,
				"legend":        fact.legend,
			}
			if fact.confidence != nil {
				answer["confidence"] = *fact.confidence
			}
			if fixture.scoreWinnerField != "" {
				answer[fixture.scoreWinnerField] = *fact.winner
			}
			answer[fixture.scoreWeightedField] = fact.score
			answers[string(id)] = answer
		}
	}
	body := map[string]any{
		"model":   fixture.model,
		"answers": answers,
		"usage":   map[string]any{"input_tokens": 89, "output_tokens": 4},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return encoded
}

// expectedDecisionResponse renders the neutral response the adapter must
// produce for a request.
func expectedDecisionResponse(request lebro.DecisionRequest, facts map[lebro.DecisionQuestionID]decisionFacts, fixture decisionFixture) lebro.DecisionResponse {
	inputTokens := int64(89)
	outputTokens := int64(4)
	answers := make(map[lebro.DecisionQuestionID]lebro.DecisionAnswer, len(facts))
	for id, fact := range facts {
		switch fact.kind {
		case "noul":
			answer := lebro.NoulAnswer{ProbabilityYes: fact.noul}
			if fixture.noulConfidence {
				answer.Confidence = floatPointer(*fact.confidence)
			}
			answers[id] = answer
		case "choice":
			answer := lebro.ChoiceAnswer{
				Choice:        lebro.DecisionOptionID(fact.choice),
				Probabilities: make(map[lebro.DecisionOptionID]float64, len(fact.probabilities)),
			}
			for option, probability := range fact.probabilities {
				answer.Probabilities[lebro.DecisionOptionID(option)] = probability
			}
			answer.Confidence = floatPointer(*fact.confidence)
			answers[id] = answer
		case "score":
			answer := lebro.ScoreAnswer{
				Probabilities: make(map[int]float64, len(fact.probabilities)),
				Legend:        make(map[int]lebro.JSONValue, len(fact.legend)),
				Provenance:    lebro.ScoreNative,
			}
			for level, probability := range fact.probabilities {
				index, err := strconv.Atoi(level)
				if err != nil {
					panic(err)
				}
				answer.Probabilities[index] = probability
			}
			for level, description := range fact.legend {
				index, err := strconv.Atoi(level)
				if err != nil {
					panic(err)
				}
				answer.Legend[index] = description
			}
			if fixture.scoreWinnerField != "" {
				winner := *fact.winner
				answer.SelectedLevel = &winner
			}
			expected := fact.score
			answer.ExpectedLevel = &expected
			answer.Confidence = floatPointer(*fact.confidence)
			answers[id] = answer
		}
	}
	return lebro.DecisionResponse{
		Provider: lebro.ProviderID(fixture.name),
		Model:    fixture.model,
		Answers:  answers,
		Usage: &lebro.DecisionUsage{
			InputTokens:  &inputTokens,
			OutputTokens: &outputTokens,
		},
	}
}

// decodeObservedDecisionRequest rebuilds the neutral request from the wire
// body the provider received.
func decodeObservedDecisionRequest(body []byte) (lebro.DecisionRequest, error) {
	var wire struct {
		Model     string                     `json:"model"`
		State     json.RawMessage            `json:"state"`
		Questions map[string]json.RawMessage `json:"questions"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return lebro.DecisionRequest{}, err
	}
	request := lebro.DecisionRequest{Model: wire.Model}
	if len(wire.State) > 0 {
		if err := json.Unmarshal(wire.State, &request.State); err != nil {
			return lebro.DecisionRequest{}, err
		}
	}
	request.Questions = make(map[lebro.DecisionQuestionID]lebro.DecisionQuestion, len(wire.Questions))
	for id, raw := range wire.Questions {
		var question struct {
			Type         string          `json:"type"`
			Instructions json.RawMessage `json:"instructions"`
			Criteria     json.RawMessage `json:"criteria"`
		}
		if err := json.Unmarshal(raw, &question); err != nil {
			return lebro.DecisionRequest{}, err
		}
		switch question.Type {
		case "noul":
			built := lebro.NoulQuestion{}
			if err := json.Unmarshal(question.Instructions, &built.Instructions); err != nil {
				return lebro.DecisionRequest{}, err
			}
			if len(question.Criteria) > 0 {
				var criteria struct {
					True  json.RawMessage `json:"true"`
					False json.RawMessage `json:"false"`
				}
				if err := json.Unmarshal(question.Criteria, &criteria); err != nil {
					return lebro.DecisionRequest{}, err
				}
				built.Criteria = &lebro.NoulCriteria{}
				if len(criteria.True) > 0 {
					if err := json.Unmarshal(criteria.True, &built.Criteria.True); err != nil {
						return lebro.DecisionRequest{}, err
					}
				}
				if len(criteria.False) > 0 {
					if err := json.Unmarshal(criteria.False, &built.Criteria.False); err != nil {
						return lebro.DecisionRequest{}, err
					}
				}
			}
			request.Questions[lebro.DecisionQuestionID(id)] = built
		case "choice":
			built := lebro.ChoiceQuestion{Criteria: map[lebro.DecisionOptionID]lebro.JSONValue{}}
			if err := json.Unmarshal(question.Instructions, &built.Instructions); err != nil {
				return lebro.DecisionRequest{}, err
			}
			var criteria map[string]json.RawMessage
			if err := json.Unmarshal(question.Criteria, &criteria); err != nil {
				return lebro.DecisionRequest{}, err
			}
			for option, description := range criteria {
				var decoded lebro.JSONValue
				if err := json.Unmarshal(description, &decoded); err != nil {
					return lebro.DecisionRequest{}, err
				}
				built.Criteria[lebro.DecisionOptionID(option)] = decoded
			}
			request.Questions[lebro.DecisionQuestionID(id)] = built
		case "score":
			built := lebro.ScoreQuestion{}
			if err := json.Unmarshal(question.Instructions, &built.Instructions); err != nil {
				return lebro.DecisionRequest{}, err
			}
			var criteria []json.RawMessage
			if err := json.Unmarshal(question.Criteria, &criteria); err != nil {
				return lebro.DecisionRequest{}, err
			}
			for _, description := range criteria {
				var decoded lebro.JSONValue
				if err := json.Unmarshal(description, &decoded); err != nil {
					return lebro.DecisionRequest{}, err
				}
				built.Criteria = append(built.Criteria, decoded)
			}
			request.Questions[lebro.DecisionQuestionID(id)] = built
		default:
			return lebro.DecisionRequest{}, errUnknownQuestionType
		}
	}
	return request, nil
}

// floatPointer returns a pointer to a copy of value.
func floatPointer(value float64) *float64 { return &value }
