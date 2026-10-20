package decisions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
)

// openAIFactory builds the shared contract suite's harness for the OpenAI
// Decisions API.
//
// The suite compares the request the provider received with the request the
// caller sent. The OpenAI protocol does not carry every part of the caller's
// request in its own field, so the harness reconstructs the neutral request
// from the wire: the state is parsed back out of the documented rendering,
// and the criteria of a binary judgment are parsed back out of the
// documented rendering of the instructions.
func openAIFactory() testkit.DecisionFactory {
	return func(t *testing.T, contractCase testkit.DecisionCase, cancel context.CancelFunc) testkit.DecisionHarness {
		t.Helper()
		var mu sync.Mutex
		var observed lebro.DecisionRequest
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read request body: %v", err)
				return
			}
			decoded, err := decodeObservedOpenAIRequest(body)
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
				_, _ = w.Write([]byte(`{"error":{"message":"the provider is busy","type":"server_error"}}`))
			case testkit.DecisionCancellation:
				// Cancel once the request has reached the provider, so the
				// suite covers the in-flight path.
				cancel()
				<-r.Context().Done()
			default:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(renderOpenAIWire(contractCase.Request))
			}
		}))
		t.Cleanup(server.Close)

		model, err := NewOpenAI(OpenAIConfig{APIKey: "test-key", BaseURL: server.URL})
		if err != nil {
			t.Fatalf("build the openai adapter: %v", err)
		}
		facts := computeDecisionFacts(contractCase.Request)
		return testkit.DecisionHarness{
			Model:    model,
			Expected: expectedOpenAIResponse(contractCase.Request, facts),
			ObservedRequest: func() lebro.DecisionRequest {
				mu.Lock()
				defer mu.Unlock()
				return observed
			},
		}
	}
}

// TestOpenAIContract runs the shared decision suite against the OpenAI
// Decisions API.
func TestOpenAIContract(t *testing.T) {
	testkit.RunDecisionContract(t, openAIFactory())
}

// renderOpenAIWire renders the OpenAI Decisions response body for a
// request.
func renderOpenAIWire(request lebro.DecisionRequest) []byte {
	facts := computeDecisionFacts(request)
	answers := make([]any, 0, len(facts))
	for id, fact := range facts {
		switch fact.kind {
		case "noul":
			answers = append(answers, map[string]any{
				"type":        "predicate",
				"name":        string(id),
				"probability": fact.noul,
			})
		case "choice":
			probabilities := make([]any, 0, len(fact.probabilities))
			for option, probability := range fact.probabilities {
				probabilities = append(probabilities, map[string]any{
					"value":       option,
					"probability": probability,
				})
			}
			answers = append(answers, map[string]any{
				"type":          "choice",
				"name":          string(id),
				"choice":        fact.choice,
				"confidence":    *fact.confidence,
				"probabilities": probabilities,
			})
		case "score":
			probabilities := make([]any, 0, len(fact.probabilities))
			for level, probability := range fact.probabilities {
				index, err := strconv.Atoi(level)
				if err != nil {
					panic(err)
				}
				probabilities = append(probabilities, map[string]any{
					"value":       index,
					"label":       level,
					"probability": probability,
				})
			}
			answers = append(answers, map[string]any{
				"type":          "score",
				"name":          string(id),
				"score":         fact.score,
				"confidence":    *fact.confidence,
				"probabilities": probabilities,
			})
		}
	}
	body := map[string]any{
		"answers": answers,
		"model":   openAIModel,
		"usage":   map[string]any{"input_tokens": 89, "output_tokens": 4, "total_tokens": 93},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return encoded
}

// expectedOpenAIResponse renders the neutral response the OpenAI adapter must
// produce for a request.
func expectedOpenAIResponse(request lebro.DecisionRequest, facts map[lebro.DecisionQuestionID]decisionFacts) lebro.DecisionResponse {
	inputTokens := int64(89)
	outputTokens := int64(4)
	totalTokens := int64(93)
	answers := make(map[lebro.DecisionQuestionID]lebro.DecisionAnswer, len(facts))
	for id, fact := range facts {
		switch fact.kind {
		case "noul":
			// A predicate answer carries a probability and nothing else.
			answers[id] = lebro.NoulAnswer{ProbabilityYes: fact.noul}
		case "choice":
			answer := lebro.ChoiceAnswer{
				Choice:           lebro.DecisionOptionID(fact.choice),
				Probabilities:    make(map[lebro.DecisionOptionID]float64, len(fact.probabilities)),
				Confidence:       floatPointer(*fact.confidence),
				ConfidenceMetric: lebro.DecisionConfidenceUnknown,
			}
			for option, probability := range fact.probabilities {
				answer.Probabilities[lebro.DecisionOptionID(option)] = probability
			}
			answers[id] = answer
		case "score":
			answer := lebro.ScoreAnswer{
				Probabilities:    make(map[int]float64, len(fact.probabilities)),
				Confidence:       floatPointer(*fact.confidence),
				ConfidenceMetric: lebro.DecisionConfidenceUnknown,
				Provenance:       lebro.ScoreNative,
			}
			for level, probability := range fact.probabilities {
				index, err := strconv.Atoi(level)
				if err != nil {
					panic(err)
				}
				answer.Probabilities[index] = probability
			}
			expected := fact.score
			answer.ExpectedLevel = &expected
			answers[id] = answer
		}
	}
	return lebro.DecisionResponse{
		Provider:       lebro.ProviderID(openAIProvider),
		RequestedModel: openAIModel,
		Model:          openAIModel,
		Answers:        answers,
		Usage: &lebro.DecisionUsage{
			InputTokens:  &inputTokens,
			OutputTokens: &outputTokens,
			TotalTokens:  &totalTokens,
		},
	}
}

// decodeObservedOpenAIRequest rebuilds the neutral request from the wire
// body the provider received.
func decodeObservedOpenAIRequest(body []byte) (lebro.DecisionRequest, error) {
	var wire struct {
		Model     string             `json:"model"`
		Input     json.RawMessage    `json:"input"`
		Questions []openAIWireChoice `json:"questions"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return lebro.DecisionRequest{}, err
	}
	request := lebro.DecisionRequest{Model: wire.Model}
	state, images, err := decodeOpenAIInput(wire.Input)
	if err != nil {
		return lebro.DecisionRequest{}, err
	}
	request.State = state
	request.Images = images
	request.Questions = make(map[lebro.DecisionQuestionID]lebro.DecisionQuestion, len(wire.Questions))
	for _, question := range wire.Questions {
		id := lebro.DecisionQuestionID(question.Name)
		switch question.Type {
		case "predicate":
			instructions, criteria, err := parseOpenAICriteria(question.Instructions)
			if err != nil {
				return lebro.DecisionRequest{}, err
			}
			request.Questions[id] = lebro.NoulQuestion{Instructions: instructions, Criteria: criteria}
		case "choice":
			criteria := make(map[lebro.DecisionOptionID]lebro.JSONValue, len(question.Choices))
			for _, choice := range question.Choices {
				criteria[lebro.DecisionOptionID(choice.Value)] = choice.Description
			}
			request.Questions[id] = lebro.ChoiceQuestion{Instructions: question.Instructions, Criteria: criteria}
		case "score":
			criteria := make([]lebro.JSONValue, 0, len(question.Levels))
			for _, level := range question.Levels {
				criteria = append(criteria, level.Description)
			}
			request.Questions[id] = lebro.ScoreQuestion{Instructions: question.Instructions, Criteria: criteria}
		default:
			return lebro.DecisionRequest{}, fmt.Errorf("lebro: unexpected question type %q", question.Type)
		}
	}
	return request, nil
}

// openAIWireChoice is one question of the OpenAI Decisions request.
type openAIWireChoice struct {
	Type         string             `json:"type"`
	Name         string             `json:"name"`
	Instructions string             `json:"instructions"`
	Choices      []openAIWireOption `json:"choices"`
	Levels       []openAIWireLevel  `json:"levels"`
}

// openAIWireOption is one option of a choice question.
type openAIWireOption struct {
	Value       string `json:"value"`
	Description string `json:"description"`
}

// openAIWireLevel is one ordered level of a score question.
type openAIWireLevel struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// decodeOpenAIInput reads the state and the images out of the request
// input.
func decodeOpenAIInput(raw json.RawMessage) (lebro.JSONValue, []lebro.DecisionImage, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		state, err := parseOpenAIState(text)
		return state, nil, err
	}
	var messages []struct {
		Role    string `json:"role"`
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL string `json:"image_url"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &messages); err != nil {
		return nil, nil, fmt.Errorf("lebro: decode the request input: %w", err)
	}
	var state lebro.JSONValue
	var images []lebro.DecisionImage
	for _, message := range messages {
		for _, part := range message.Content {
			switch part.Type {
			case "input_text":
				decoded, err := parseOpenAIState(part.Text)
				if err != nil {
					return nil, nil, err
				}
				state = decoded
			case "input_image":
				mediaType, _, ok := strings.Cut(part.ImageURL, ";base64,")
				if !ok {
					return nil, nil, errors.New("lebro: the request carries an image that is not a base64 data URL")
				}
				images = append(images, lebro.DecisionImage{MediaType: strings.TrimPrefix(mediaType, "data:")})
			}
		}
	}
	return state, images, nil
}

// parseOpenAIState reads the state back out of the documented rendering.
func parseOpenAIState(text string) (lebro.JSONValue, error) {
	prefix := openAIStateLabel + "\n"
	if !strings.HasPrefix(text, prefix) {
		return text, nil
	}
	var decoded lebro.JSONValue
	if err := json.Unmarshal([]byte(strings.TrimPrefix(text, prefix)), &decoded); err != nil {
		return nil, fmt.Errorf("lebro: decode the rendered state: %w", err)
	}
	return decoded, nil
}

// parseOpenAICriteria reads the criteria of a binary judgment back out of
// the documented rendering of the instructions.
func parseOpenAICriteria(instructions string) (string, *lebro.NoulCriteria, error) {
	marker := "\n\n" + openAICriteriaLabel
	index := strings.LastIndex(instructions, marker)
	if index < 0 {
		return instructions, nil, nil
	}
	var criteria struct {
		Yes string `json:"yes"`
		No  string `json:"no"`
	}
	if err := json.Unmarshal([]byte(instructions[index+len(marker):]), &criteria); err != nil {
		return "", nil, fmt.Errorf("lebro: decode the rendered criteria: %w", err)
	}
	return instructions[:index], &lebro.NoulCriteria{True: criteria.Yes, False: criteria.No}, nil
}

// TestOpenAIRequestShape pins the wire shape of the request: the questions
// are an array, the input carries the state, and the options and the levels
// carry the caller's identifiers.
func TestOpenAIRequestShape(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"answers": [
				{"type": "predicate", "name": "is_refund", "probability": 0.8},
				{"type": "choice", "name": "department", "choice": "billing", "confidence": 0.5,
					"probabilities": [{"value": "billing", "probability": 0.6}, {"value": "technical", "probability": 0.4}]},
				{"type": "score", "name": "severity", "score": 1, "confidence": 0.4,
					"probabilities": [{"value": 0, "probability": 0.5}, {"value": 1, "probability": 0.3}, {"value": 2, "probability": 0.2}]}
			],
			"model": "gpt-6-luna"
		}`))
	}))
	defer server.Close()

	model, err := NewOpenAI(OpenAIConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewOpenAI() error = %v", err)
	}
	if _, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State: "My card was charged twice.",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"department": lebro.ChoiceQuestion{
				Instructions: "Which team?",
				Criteria: map[lebro.DecisionOptionID]lebro.JSONValue{
					"billing":   "Payments and refunds",
					"technical": "Product faults",
				},
			},
			"is_refund": lebro.NoulQuestion{
				Instructions: "Does this qualify?",
				Criteria:     &lebro.NoulCriteria{True: "Qualifies", False: "Does not qualify"},
			},
			"severity": lebro.ScoreQuestion{
				Instructions: "How severe?",
				Criteria:     []lebro.JSONValue{"Cosmetic", "Workaround", "Blocked"},
			},
		},
	}); err != nil {
		t.Fatalf("Decide() error = %v", err)
	}

	if body["model"] != openAIModel {
		t.Fatalf("the request model = %#v, want %q", body["model"], openAIModel)
	}
	if body["input"] != "My card was charged twice." {
		t.Fatalf("the request input = %#v, want the state text", body["input"])
	}
	questions, ok := body["questions"].([]any)
	if !ok || len(questions) != 3 {
		t.Fatalf("the request questions = %#v, want three questions", body["questions"])
	}
	byName := make(map[string]map[string]any, len(questions))
	for _, entry := range questions {
		question, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("the request question = %#v, want an object", entry)
		}
		name, _ := question["name"].(string)
		byName[name] = question
	}
	// The questions go out in a stable order, so the identifiers decide it.
	if byName["department"]["type"] != "choice" {
		t.Fatalf("the choice question = %#v, want a choice question", byName["department"])
	}
	if byName["is_refund"]["type"] != "predicate" {
		t.Fatalf("the binary question = %#v, want a predicate question", byName["is_refund"])
	}
	if byName["severity"]["type"] != "score" {
		t.Fatalf("the score question = %#v, want a score question", byName["severity"])
	}
	choices, ok := byName["department"]["choices"].([]any)
	if !ok || len(choices) != 2 {
		t.Fatalf("the choice options = %#v, want two options", byName["department"]["choices"])
	}
	first, ok := choices[0].(map[string]any)
	if !ok {
		t.Fatalf("the choice option = %#v, want an object", choices[0])
	}
	if first["value"] != "billing" {
		t.Fatalf("the choice option value = %#v, want %q", first["value"], "billing")
	}
	if first["description"] != "Payments and refunds" {
		t.Fatalf("the choice option description = %#v, want the caller's description", first["description"])
	}
	levels, ok := byName["severity"]["levels"].([]any)
	if !ok || len(levels) != 3 {
		t.Fatalf("the score levels = %#v, want three levels", byName["severity"]["levels"])
	}
	firstLevel, ok := levels[0].(map[string]any)
	if !ok {
		t.Fatalf("the score level = %#v, want an object", levels[0])
	}
	if firstLevel["label"] != "0" {
		t.Fatalf("the score level label = %#v, want the level index", firstLevel["label"])
	}
	if firstLevel["description"] != "Cosmetic" {
		t.Fatalf("the score level description = %#v, want the caller's description", firstLevel["description"])
	}
	// The criteria have no field of their own, so they are rendered into the
	// instructions.
	instructions, _ := byName["is_refund"]["instructions"].(string)
	if !strings.Contains(instructions, "Qualifies") || !strings.Contains(instructions, "Does not qualify") {
		t.Fatalf("the binary question instructions = %q, want the criteria rendered into them", instructions)
	}
}

// TestOpenAIRendersAStructuredState pins the documented rendering of a
// state that is not text.
func TestOpenAIRendersAStructuredState(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":[{"type":"predicate","name":"is_refund","probability":0.8}],"model":"gpt-6-luna"}`))
	}))
	defer server.Close()

	model, err := NewOpenAI(OpenAIConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewOpenAI() error = %v", err)
	}
	if _, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State: map[string]any{"order_id": "A-104", "amount": 42.5},
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"is_refund": lebro.NoulQuestion{Instructions: "Does this qualify?"},
		},
	}); err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	input, ok := body["input"].(string)
	if !ok {
		t.Fatalf("the request input = %#v, want the rendered state text", body["input"])
	}
	if !strings.HasPrefix(input, openAIStateLabel+"\n") {
		t.Fatalf("the request input = %q, want the documented state label", input)
	}
	if !strings.Contains(input, `"order_id": "A-104"`) {
		t.Fatalf("the request input = %q, want the state's content", input)
	}
}

// TestOpenAIRejectsStructuredInstructions pins that a value the OpenAI
// Decisions schema cannot carry is rejected before the request is sent.
func TestOpenAIRejectsStructuredInstructions(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":[{"type":"predicate","name":"q","probability":0.5}],"model":"gpt-6-luna"}`))
	}))
	defer server.Close()

	model, err := NewOpenAI(OpenAIConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewOpenAI() error = %v", err)
	}
	_, err = model.Decide(context.Background(), lebro.DecisionRequest{
		State: "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"q": lebro.NoulQuestion{Instructions: map[string]any{"question": "Is it?"}},
		},
	})
	if !errors.Is(err, lebro.ErrDecisionInvalidRequest) {
		t.Fatalf("Decide() error = %v, want an invalid request error", err)
	}
	if requests != 0 {
		t.Fatalf("the provider received %d requests, want none", requests)
	}
}

// TestOpenAIEncodesImagesAsDataURLs pins that the image bytes reach the
// provider as an inline base64 data URL and nowhere else.
func TestOpenAIEncodesImagesAsDataURLs(t *testing.T) {
	payload := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":[{"type":"predicate","name":"q","probability":0.8}],"model":"gpt-6-luna"}`))
	}))
	defer server.Close()

	model, err := NewOpenAI(OpenAIConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewOpenAI() error = %v", err)
	}
	if _, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State:  "A photo of a damaged product.",
		Images: []lebro.DecisionImage{{MediaType: "image/png", Bytes: payload}},
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"q": lebro.NoulQuestion{Instructions: "Is it damaged?"},
		},
	}); err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	messages, ok := body["input"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("the request input = %#v, want one message", body["input"])
	}
	message, ok := messages[0].(map[string]any)
	if !ok {
		t.Fatalf("the request message = %#v, want an object", messages[0])
	}
	parts, ok := message["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("the message content = %#v, want a text part and an image part", message["content"])
	}
	image, ok := parts[1].(map[string]any)
	if !ok {
		t.Fatalf("the image part = %#v, want an object", parts[1])
	}
	if image["type"] != "input_image" {
		t.Fatalf("the image part type = %#v, want %q", image["type"], "input_image")
	}
	url, _ := image["image_url"].(string)
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(payload)
	if url != want {
		t.Fatalf("the image URL = %q, want %q", url, want)
	}
}

// TestOpenAIRefusal pins that a refusal is a first-class answer and that it
// leaves its sibling answers alone.
func TestOpenAIRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"answers": [
				{"type": "predicate", "name": "is_refund", "probability": 0.88},
				{"type": "refusal", "name": "department"}
			],
			"model": "gpt-6-luna"
		}`))
	}))
	defer server.Close()

	model, err := NewOpenAI(OpenAIConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewOpenAI() error = %v", err)
	}
	response, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State: "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"department": lebro.ChoiceQuestion{
				Instructions: "Which team?",
				Criteria:     map[lebro.DecisionOptionID]lebro.JSONValue{"billing": "Payments", "technical": "Faults"},
			},
			"is_refund": lebro.NoulQuestion{Instructions: "Does this qualify?"},
		},
	})
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	refusal, ok := response.Answers["department"].(lebro.RefusalAnswer)
	if !ok {
		t.Fatalf("the refused answer = %#v, want a refusal answer", response.Answers["department"])
	}
	if refusal.Reason != "" {
		t.Fatalf("the refusal reason = %q, want none", refusal.Reason)
	}
	noul, ok := response.Answers["is_refund"].(lebro.NoulAnswer)
	if !ok {
		t.Fatalf("the sibling answer = %#v, want a noul answer", response.Answers["is_refund"])
	}
	if noul.ProbabilityYes != 0.88 {
		t.Fatalf("the sibling probability = %v, want 0.88", noul.ProbabilityYes)
	}
}

// TestOpenAIRejectsAnUnknownAnswerName pins that an answer the caller never
// asked for is a response the adapter cannot place.
func TestOpenAIRejectsAnUnknownAnswerName(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "an unknown question",
			body: `{"answers":[{"type":"predicate","name":"other","probability":0.5}],"model":"gpt-6-luna"}`,
		},
		{
			name: "a duplicate question",
			body: `{"answers":[{"type":"predicate","name":"q","probability":0.5},{"type":"predicate","name":"q","probability":0.6}],"model":"gpt-6-luna"}`,
		},
		{
			name: "a missing question",
			body: `{"answers":[],"model":"gpt-6-luna"}`,
		},
		{
			name: "an answer with no name",
			body: `{"answers":[{"type":"predicate","probability":0.5}],"model":"gpt-6-luna"}`,
		},
		{
			name: "the wrong answer kind",
			body: `{"answers":[{"type":"choice","name":"q","choice":"a"}],"model":"gpt-6-luna"}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(testCase.body))
			}))
			defer server.Close()

			model, err := NewOpenAI(OpenAIConfig{APIKey: "test-key", BaseURL: server.URL})
			if err != nil {
				t.Fatalf("NewOpenAI() error = %v", err)
			}
			_, err = model.Decide(context.Background(), lebro.DecisionRequest{
				State:     "state",
				Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
			})
			if !errors.Is(err, lebro.ErrDecisionMalformedResponse) {
				t.Fatalf("Decide() error = %v, want a malformed response failure", err)
			}
		})
	}
}

// TestOpenAIRejectsADistributionThatDoesNotAddUp pins that a reported
// distribution has to be a distribution.
func TestOpenAIRejectsADistributionThatDoesNotAddUp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":[{"type":"choice","name":"q","choice":"a","confidence":0.5,"probabilities":[{"value":"a","probability":0.9},{"value":"b","probability":0.4}]}],"model":"gpt-6-luna"}`))
	}))
	defer server.Close()

	model, err := NewOpenAI(OpenAIConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewOpenAI() error = %v", err)
	}
	_, err = model.Decide(context.Background(), lebro.DecisionRequest{
		State: "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"q": lebro.ChoiceQuestion{
				Instructions: "Which one?",
				Criteria:     map[lebro.DecisionOptionID]lebro.JSONValue{"a": "first", "b": "second"},
			},
		},
	})
	if !errors.Is(err, lebro.ErrDecisionMalformedResponse) {
		t.Fatalf("Decide() error = %v, want a malformed response failure", err)
	}
}

// TestOpenAIRejectsAnUnknownModel pins that a request naming a model the
// Decisions API does not serve is rejected.
func TestOpenAIRejectsAnUnknownModel(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":[{"type":"predicate","name":"q","probability":0.5}],"model":"gpt-6-luna"}`))
	}))
	defer server.Close()

	model, err := NewOpenAI(OpenAIConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewOpenAI() error = %v", err)
	}
	_, err = model.Decide(context.Background(), lebro.DecisionRequest{
		Model:     "gpt-6-some-other",
		State:     "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
	})
	if !errors.Is(err, lebro.ErrDecisionUnsupportedCapability) {
		t.Fatalf("Decide() error = %v, want an unsupported capability failure", err)
	}
	if requests != 0 {
		t.Fatalf("the provider received %d requests, want none", requests)
	}
}

// TestOpenAICredentialsStayOutOfErrors pins that a credential never reaches
// an error message.
func TestOpenAICredentialsStayOutOfErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key","type":"invalid_request_error"}}`))
	}))
	defer server.Close()

	model, err := NewOpenAI(OpenAIConfig{APIKey: "super-secret-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewOpenAI() error = %v", err)
	}
	_, err = model.Decide(context.Background(), lebro.DecisionRequest{
		State:     "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
	})
	if err == nil {
		t.Fatal("Decide() error = nil, want an authentication failure")
	}
	if !errors.Is(err, lebro.ErrDecisionAuthentication) {
		t.Fatalf("Decide() error = %v, want an authentication failure", err)
	}
	if strings.Contains(err.Error(), "super-secret-key") {
		t.Fatalf("Decide() error = %v, want the credential kept out of the message", err)
	}
}

// TestOpenAIRequestContentStaysOutOfErrors pins that a provider which
// echoes the request back does not carry the caller's content into the
// error message.
//
// The echoed content is the rendered form, so the rendering itself has to be
// redacted: the criteria are written into the instructions as JSON, and a
// criterion that JSON escapes would not match the caller's own value.
func TestOpenAIRequestContentStaysOutOfErrors(t *testing.T) {
	cases := []struct {
		name    string
		state   lebro.JSONValue
		secrets []string
	}{
		{
			name:  "a structured state",
			state: map[string]any{"order_id": "A-104-secret-order"},
			secrets: []string{
				"A-104-secret-order",
			},
		},
		{
			name:  "a criterion the rendering escapes",
			state: "A customer asked for a refund.",
			secrets: []string{
				`qualifies for a "refund"`,
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var body []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				read, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read request body: %v", err)
					return
				}
				body = read
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				// The provider puts the request it read into its own
				// description.
				rejection, err := json.Marshal(map[string]any{
					"error": map[string]any{
						"message": "the request was rejected: " + string(read),
						"type":    "invalid_request_error",
					},
				})
				if err != nil {
					t.Errorf("encode the rejection: %v", err)
					return
				}
				_, _ = w.Write(rejection)
			}))
			defer server.Close()

			model, err := NewOpenAI(OpenAIConfig{APIKey: "super-secret-key", BaseURL: server.URL})
			if err != nil {
				t.Fatalf("NewOpenAI() error = %v", err)
			}
			_, err = model.Decide(context.Background(), lebro.DecisionRequest{
				State: testCase.state,
				Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
					"is_refund": lebro.NoulQuestion{
						Instructions: "Does this qualify for a refund?",
						Criteria: &lebro.NoulCriteria{
							True:  `qualifies for a "refund"`,
							False: "does not qualify",
						},
					},
				},
			})
			if err == nil {
				t.Fatal("Decide() error = nil, want a provider failure")
			}
			for _, secret := range append(testCase.secrets, "super-secret-key") {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("Decide() error = %v, want %q kept out of the message", err, secret)
				}
			}
			// The provider saw the content; the error does not carry it.
			if !strings.Contains(string(body), "is_refund") {
				t.Fatalf("the request body = %s, want the question", body)
			}
		})
	}
}

// TestOpenAIRejectsAConfidenceOnAPredicate pins that a predicate answer
// carries a probability and nothing else: the probability already is the
// uncertainty.
func TestOpenAIRejectsAConfidenceOnAPredicate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":[{"type":"predicate","name":"q","probability":0.5,"confidence":0.9}],"model":"gpt-6-luna"}`))
	}))
	defer server.Close()

	model, err := NewOpenAI(OpenAIConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewOpenAI() error = %v", err)
	}
	_, err = model.Decide(context.Background(), lebro.DecisionRequest{
		State:     "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
	})
	if !errors.Is(err, lebro.ErrDecisionMalformedResponse) {
		t.Fatalf("Decide() error = %v, want a malformed response failure", err)
	}
}

// TestOpenAIMalformedDistributions pins that a malformed distribution is a
// malformed response: an entry without a probability, a repeated option, and
// a null level are all rejected.
func TestOpenAIMalformedDistributions(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "an entry with no probability",
			body: `{"answers":[{"type":"choice","name":"q","choice":"a","confidence":0.5,"probabilities":[{"value":"a"},{"value":"b","probability":1}]}],"model":"gpt-6-luna"}`,
			want: "distribution entry with no probability",
		},
		{
			name: "a repeated option",
			body: `{"answers":[{"type":"choice","name":"q","choice":"a","confidence":0.5,"probabilities":[{"value":"a","probability":0.5},{"value":"a","probability":0.5},{"value":"b","probability":0}]}],"model":"gpt-6-luna"}`,
			want: "two probabilities for option",
		},
		{
			name: "a null option",
			body: `{"answers":[{"type":"choice","name":"q","choice":"a","confidence":0.5,"probabilities":[{"value":null,"probability":1},{"value":"b","probability":0}]}],"model":"gpt-6-luna"}`,
			want: "not an option",
		},
		{
			name: "a null level",
			body: `{"answers":[{"type":"score","name":"q","score":0,"confidence":0.5,"probabilities":[{"value":null,"probability":1},{"value":1,"probability":0}]}],"model":"gpt-6-luna"}`,
			want: "not a level index",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(testCase.body))
			}))
			defer server.Close()

			model, err := NewOpenAI(OpenAIConfig{APIKey: "test-key", BaseURL: server.URL})
			if err != nil {
				t.Fatalf("NewOpenAI() error = %v", err)
			}
			var question lebro.DecisionQuestion = lebro.ChoiceQuestion{
				Instructions: "Which one?",
				Criteria:     map[lebro.DecisionOptionID]lebro.JSONValue{"a": "first", "b": "second"},
			}
			if testCase.name == "a null level" {
				question = lebro.ScoreQuestion{
					Instructions: "How much?",
					Criteria:     []lebro.JSONValue{"low", "high"},
				}
			}
			_, err = model.Decide(context.Background(), lebro.DecisionRequest{
				State:     "state",
				Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": question},
			})
			if !errors.Is(err, lebro.ErrDecisionMalformedResponse) {
				t.Fatalf("Decide() error = %v, want a malformed response failure", err)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Decide() error = %v, want it to mention %q", err, testCase.want)
			}
		})
	}
}

// TestOpenAIRejectsAnInvalidEndpoint pins that a base URL the adapter
// cannot use is rejected when the adapter is built.
func TestOpenAIRejectsAnInvalidEndpoint(t *testing.T) {
	for _, baseURL := range []string{"://not a url", "/relative", "ftp://example.com", "https://"} {
		if _, err := NewOpenAI(OpenAIConfig{APIKey: "test-key", BaseURL: baseURL}); err == nil {
			t.Fatalf("NewOpenAI() error = nil, want the base URL %q to be rejected", baseURL)
		}
	}
}

// TestOpenAICanRefuseAnyQuestion pins that the profile says a question of
// any kind can be refused, because the endpoint says so.
func TestOpenAICanRefuseAnyQuestion(t *testing.T) {
	model, err := NewOpenAI(OpenAIConfig{APIKey: "test-key"})
	if err != nil {
		t.Fatalf("NewOpenAI() error = %v", err)
	}
	capabilities := model.Capabilities()
	if !capabilities.NoulUncertainty.Refusal ||
		!capabilities.ChoiceUncertainty.Refusal ||
		!capabilities.ScoreUncertainty.Refusal {
		t.Fatalf("the refusal capability = %#v, want every kind to allow a refusal", capabilities)
	}
}
