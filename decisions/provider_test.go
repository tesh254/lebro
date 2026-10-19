package decisions

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tesh254/lebro"
)

// genericProviderConfig is the contract of a System One provider this
// package ships no named adapter for.
func genericProviderConfig(baseURL string) SystemOneConfig {
	return SystemOneConfig{
		APIKey:     "generic-key",
		BaseURL:    baseURL,
		AuthHeader: "X-Generic-Key",
		ProviderID: lebro.ProviderID("generic"),
		Model:      "generic-model",
		Score:      SystemOneScoreFields{Weighted: "expected_level", Winner: "score"},
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
			RetryableStatuses:      []int{http.StatusTooManyRequests},
		},
		ImageStyle:       SystemOneImagesInline,
		MaxBodyBytes:     1 << 20,
		RequestIDHeaders: []string{"x-generic-request-id"},
	}
}

// TestGenericProviderByConfiguration builds a provider from configuration
// alone and checks that the whole contract still holds: the credential
// placement, the model, the score field names, the answers, and the request
// identity.
func TestGenericProviderByConfiguration(t *testing.T) {
	var body map[string]any
	var credential string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		credential = r.Header.Get("X-Generic-Key")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-generic-request-id", "generic-1")
		_, _ = w.Write([]byte(`{
			"model": "generic-model-2026",
			"answers": {
				"eligible": {"type": "noul", "noul": 0.81, "confidence": 0.62},
				"team": {"type": "choice", "choice": "billing", "probabilities": {"billing": 0.8, "technical": 0.2}, "confidence": 0.6},
				"urgency": {
					"type": "score",
					"score": 1,
					"expected_level": 1.4,
					"confidence": 0.7,
					"probabilities": {"0": 0.2, "1": 0.2, "2": 0.6},
					"legend": {"0": "low", "1": "medium", "2": "high"}
				}
			},
			"usage": {"input_tokens": 12, "output_tokens": 3}
		}`))
	}))
	defer server.Close()

	model, err := NewSystemOne(genericProviderConfig(server.URL))
	if err != nil {
		t.Fatalf("NewSystemOne() error = %v", err)
	}
	if model.ProviderID() != "generic" {
		t.Fatalf("ProviderID() = %q, want %q", model.ProviderID(), "generic")
	}
	response, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State: "a customer asked for a refund",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"eligible": lebro.NoulQuestion{Instructions: "Is it eligible?"},
			"team": lebro.ChoiceQuestion{
				Instructions: "Which team?",
				Criteria:     map[lebro.DecisionOptionID]lebro.JSONValue{"billing": "money", "technical": "bugs"},
			},
			"urgency": lebro.ScoreQuestion{
				Instructions: "How urgent?",
				Criteria:     []lebro.JSONValue{"low", "medium", "high"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("Decide() response error = %v", err)
	}
	if credential != "generic-key" {
		t.Fatalf("the credential header = %q, want %q", credential, "generic-key")
	}
	if body["model"] != "generic-model" {
		t.Fatalf("the request model = %#v, want %q", body["model"], "generic-model")
	}
	if response.Model != "generic-model-2026" {
		t.Fatalf("Decide() model = %q, want %q", response.Model, "generic-model-2026")
	}
	if response.RequestID != "generic-1" {
		t.Fatalf("Decide() request ID = %q, want %q", response.RequestID, "generic-1")
	}
	// The configured score fields decide the meaning: the winner comes from
	// "score" and the weighted value from "expected_level".
	score, ok := response.Answers["urgency"].(lebro.ScoreAnswer)
	if !ok {
		t.Fatalf("Decide() score answer = %#v, want a score answer", response.Answers["urgency"])
	}
	if score.SelectedLevel == nil || *score.SelectedLevel != 1 {
		t.Fatalf("Decide() selected level = %#v, want 1", score.SelectedLevel)
	}
	if score.ExpectedLevel == nil || *score.ExpectedLevel != 1.4 {
		t.Fatalf("Decide() expected level = %#v, want 1.4", score.ExpectedLevel)
	}
	noul, ok := response.Answers["eligible"].(lebro.NoulAnswer)
	if !ok {
		t.Fatalf("Decide() noul answer = %#v, want a noul answer", response.Answers["eligible"])
	}
	if noul.ProbabilityYes != 0.81 {
		t.Fatalf("Decide() probability = %v, want 0.81", noul.ProbabilityYes)
	}
	if noul.Confidence == nil || *noul.Confidence != 0.62 {
		t.Fatalf("Decide() confidence = %#v, want 0.62", noul.Confidence)
	}
}

// TestGenericProviderScoreFieldNames pins that the score field names decide
// the meaning of the answer, and that a provider reporting no winning level
// leaves the selected level unset.
func TestGenericProviderScoreFieldNames(t *testing.T) {
	cases := []struct {
		name       string
		score      SystemOneScoreFields
		body       string
		wantLevel  *int
		wantExpect float64
	}{
		{
			name:       "a weighted value and no winner",
			score:      SystemOneScoreFields{Weighted: "score"},
			body:       `{"model":"m","answers":{"q":{"type":"score","score":1.4,"probabilities":{"0":0.5,"1":0.5}}}}`,
			wantLevel:  nil,
			wantExpect: 1.4,
		},
		{
			name:       "a winner and a weighted value",
			score:      SystemOneScoreFields{Weighted: "expected_level", Winner: "score"},
			body:       `{"model":"m","answers":{"q":{"type":"score","score":1,"expected_level":1.4,"probabilities":{"0":0.5,"1":0.5}}}}`,
			wantLevel:  intPointer(1),
			wantExpect: 1.4,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(testCase.body))
			}))
			defer server.Close()

			config := genericProviderConfig(server.URL)
			config.Score = testCase.score
			model, err := NewSystemOne(config)
			if err != nil {
				t.Fatalf("NewSystemOne() error = %v", err)
			}
			response, err := model.Decide(context.Background(), lebro.DecisionRequest{
				State: "state",
				Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
					"q": lebro.ScoreQuestion{Instructions: "How much?", Criteria: []lebro.JSONValue{"low", "high"}},
				},
			})
			if err != nil {
				t.Fatalf("Decide() error = %v", err)
			}
			answer, ok := response.Answers["q"].(lebro.ScoreAnswer)
			if !ok {
				t.Fatalf("Decide() score answer = %#v, want a score answer", response.Answers["q"])
			}
			if answer.ExpectedLevel == nil || *answer.ExpectedLevel != testCase.wantExpect {
				t.Fatalf("Decide() expected level = %#v, want %v", answer.ExpectedLevel, testCase.wantExpect)
			}
			switch {
			case testCase.wantLevel == nil && answer.SelectedLevel != nil:
				t.Fatalf("Decide() selected level = %d, want none", *answer.SelectedLevel)
			case testCase.wantLevel != nil && (answer.SelectedLevel == nil || *answer.SelectedLevel != *testCase.wantLevel):
				t.Fatalf("Decide() selected level = %#v, want %d", answer.SelectedLevel, *testCase.wantLevel)
			}
		})
	}
}

// TestGenericProviderRejectsAScoreFieldMismatch pins that a configuration
// whose weighted and winner fields are the same is rejected before the
// adapter is built.
func TestGenericProviderRejectsAScoreFieldMismatch(t *testing.T) {
	config := genericProviderConfig("https://example.com")
	config.Score = SystemOneScoreFields{Weighted: "score", Winner: "score"}
	if _, err := NewSystemOne(config); err == nil {
		t.Fatal("NewSystemOne() error = nil, want the mismatched score fields to be rejected")
	}
}

// TestGenericProviderTextOnlyInstructions pins that a provider which does
// not accept structured instructions refuses them before the request is sent.
func TestGenericProviderTextOnlyInstructions(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","answers":{}}`))
	}))
	defer server.Close()

	config := genericProviderConfig(server.URL)
	config.Capabilities.StructuredInstructions = false
	model, err := NewSystemOne(config)
	if err != nil {
		t.Fatalf("NewSystemOne() error = %v", err)
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

// TestGenericProviderImages pins that the image style selects the wire
// shape, and that a provider whose style this package does not implement has
// image inputs disabled.
func TestGenericProviderImages(t *testing.T) {
	image := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	t.Run("inline style sends the images", func(t *testing.T) {
		var body map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode request body: %v", err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"m","answers":{"q":{"type":"noul","noul":0.5}}}`))
		}))
		defer server.Close()

		config := genericProviderConfig(server.URL)
		config.Capabilities.Images = lebro.DecisionImageCapabilities{
			Enabled:    true,
			MediaTypes: []string{"image/png"},
			MaxImages:  2,
		}
		model, err := NewSystemOne(config)
		if err != nil {
			t.Fatalf("NewSystemOne() error = %v", err)
		}
		if !model.Capabilities().Images.Enabled {
			t.Fatal("the adapter should advertise image support")
		}
		if _, err := model.Decide(context.Background(), lebro.DecisionRequest{
			State:  "state",
			Images: []lebro.DecisionImage{{MediaType: "image/png", Bytes: image}},
			Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
				"q": lebro.NoulQuestion{Instructions: "Is it?"},
			},
		}); err != nil {
			t.Fatalf("Decide() error = %v", err)
		}
		images, ok := body["images"].([]any)
		if !ok || len(images) != 1 {
			t.Fatalf("request images = %#v, want one image", body["images"])
		}
	})
	t.Run("an unimplemented style disables images", func(t *testing.T) {
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"m","answers":{}}`))
		}))
		defer server.Close()

		config := genericProviderConfig(server.URL)
		config.ImageStyle = SystemOneImageStyle("assets")
		config.Capabilities.Images = lebro.DecisionImageCapabilities{
			Enabled:    true,
			MediaTypes: []string{"image/png"},
		}
		model, err := NewSystemOne(config)
		if err != nil {
			t.Fatalf("NewSystemOne() error = %v", err)
		}
		if model.Capabilities().Images.Enabled {
			t.Fatal("the adapter must not advertise an image style it cannot send")
		}
		_, err = model.Decide(context.Background(), lebro.DecisionRequest{
			State:  "state",
			Images: []lebro.DecisionImage{{MediaType: "image/png", Bytes: image}},
			Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
				"q": lebro.NoulQuestion{Instructions: "Is it?"},
			},
		})
		if !errors.Is(err, lebro.ErrDecisionUnsupportedCapability) {
			t.Fatalf("Decide() error = %v, want an unsupported capability error", err)
		}
		if requests != 0 {
			t.Fatalf("the provider received %d requests, want none", requests)
		}
	})
}

// TestGenericProviderStorageOptOut pins that the storage opt-out reaches
// the provider and that it is off by default.
func TestGenericProviderStorageOptOut(t *testing.T) {
	cases := []struct {
		name           string
		disableStorage bool
		wantStoreField bool
	}{
		{name: "off by default", disableStorage: false, wantStoreField: false},
		{name: "requested", disableStorage: true, wantStoreField: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request body: %v", err)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"model":"m","answers":{"q":{"type":"noul","noul":0.5}}}`))
			}))
			defer server.Close()

			config := genericProviderConfig(server.URL)
			config.DisableStorage = testCase.disableStorage
			model, err := NewSystemOne(config)
			if err != nil {
				t.Fatalf("NewSystemOne() error = %v", err)
			}
			if _, err := model.Decide(context.Background(), lebro.DecisionRequest{
				State:     "state",
				Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
			}); err != nil {
				t.Fatalf("Decide() error = %v", err)
			}
			_, present := body["store"]
			if present != testCase.wantStoreField {
				t.Fatalf("the request store field is present = %v, want %v", present, testCase.wantStoreField)
			}
			if present && body["store"] != false {
				t.Fatalf("the request store = %#v, want false", body["store"])
			}
		})
	}
}

// TestGenericProviderNeedsACredentialAndEndpoint pins the constructor's
// requirements.
func TestGenericProviderNeedsACredentialAndEndpoint(t *testing.T) {
	config := genericProviderConfig("https://example.com")
	config.APIKey = ""
	if _, err := NewSystemOne(config); err == nil {
		t.Fatal("NewSystemOne() error = nil, want a missing credential to be rejected")
	}
	config = genericProviderConfig("")
	if _, err := NewSystemOne(config); err == nil {
		t.Fatal("NewSystemOne() error = nil, want a missing base URL to be rejected")
	}
}

// TestGenericProviderHonorsTheRetryPolicy pins that a configured retryable
// status is retried and an unlisted one is not.
func TestGenericProviderHonorsTheRetryPolicy(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		wantAttempts int
	}{
		{name: "a listed status is retried", status: http.StatusTooManyRequests, wantAttempts: 3},
		{name: "an unlisted status is not retried", status: http.StatusServiceUnavailable, wantAttempts: 1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			attempts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts++
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(testCase.status)
				_, _ = w.Write([]byte(`{"detail":{"error_type":"unavailable","message":"try later"}}`))
			}))
			defer server.Close()

			config := genericProviderConfig(server.URL)
			config.MaxRetries = intPointer(2)
			model, err := NewSystemOne(config)
			if err != nil {
				t.Fatalf("NewSystemOne() error = %v", err)
			}
			_, err = model.Decide(context.Background(), lebro.DecisionRequest{
				State:     "state",
				Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
			})
			if err == nil {
				t.Fatal("Decide() error = nil, want a provider failure")
			}
			if attempts != testCase.wantAttempts {
				t.Fatalf("the provider received %d attempts, want %d", attempts, testCase.wantAttempts)
			}
		})
	}
}

// TestGenericProviderCapabilitiesAreCopied pins that a caller cannot change
// the capabilities the adapter reports for everyone else.
func TestGenericProviderCapabilitiesAreCopied(t *testing.T) {
	config := genericProviderConfig("https://example.com")
	config.Capabilities.Images = lebro.DecisionImageCapabilities{
		Enabled:    true,
		MediaTypes: []string{"image/png", "image/jpeg"},
	}
	model, err := NewSystemOne(config)
	if err != nil {
		t.Fatalf("NewSystemOne() error = %v", err)
	}
	capabilities := model.Capabilities()
	capabilities.RetryableStatuses[0] = 999
	if model.Capabilities().RetryableStatuses[0] == 999 {
		t.Fatal("the retryable statuses are shared between calls")
	}
	capabilities.Images.MediaTypes[0] = "image/gif"
	if model.Capabilities().Images.MediaTypes[0] != "image/png" {
		t.Fatal("the image media types are shared between calls")
	}
	// The configuration the caller kept must not change the adapter either.
	config.Capabilities.Images.MediaTypes[0] = "image/avif"
	if model.Capabilities().Images.MediaTypes[0] != "image/png" {
		t.Fatal("the adapter shares the configured media types")
	}
}

// TestGenericProviderEnforcesTheStateLimit pins that an advertised state
// limit is checked before the request is sent.
func TestGenericProviderEnforcesTheStateLimit(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","answers":{"q":{"type":"noul","noul":0.5}}}`))
	}))
	defer server.Close()

	config := genericProviderConfig(server.URL)
	config.Capabilities.MaxStateBytes = 16
	model, err := NewSystemOne(config)
	if err != nil {
		t.Fatalf("NewSystemOne() error = %v", err)
	}
	_, err = model.Decide(context.Background(), lebro.DecisionRequest{
		State:     "a state that is comfortably larger than sixteen bytes",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
	})
	if !errors.Is(err, lebro.ErrDecisionInvalidRequest) {
		t.Fatalf("Decide() error = %v, want an invalid request error", err)
	}
	if requests != 0 {
		t.Fatalf("the provider received %d requests, want none", requests)
	}
}

// TestConfidenceIsLeftNilWhenTheProviderOmitsIt pins that an adapter does
// not invent a confidence for a provider that reports none.
func TestConfidenceIsLeftNilWhenTheProviderOmitsIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "m",
			"answers": {
				"eligible": {"type": "noul", "noul": 0.7},
				"team": {"type": "choice", "choice": "billing", "probabilities": {"billing": 0.7, "other": 0.3}},
				"urgency": {"type": "score", "score": 1.2, "probabilities": {"0": 0.5, "1": 0.5}}
			}
		}`))
	}))
	defer server.Close()

	config := genericProviderConfig(server.URL)
	config.Score = SystemOneScoreFields{Weighted: "score"}
	model, err := NewSystemOne(config)
	if err != nil {
		t.Fatalf("NewSystemOne() error = %v", err)
	}
	response, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State: "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"eligible": lebro.NoulQuestion{Instructions: "Is it?"},
			"team": lebro.ChoiceQuestion{
				Instructions: "Which one?",
				Criteria:     map[lebro.DecisionOptionID]lebro.JSONValue{"billing": "money", "other": "something else"},
			},
			"urgency": lebro.ScoreQuestion{Instructions: "How much?", Criteria: []lebro.JSONValue{"low", "high"}},
		},
	})
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	noul, ok := response.Answers["eligible"].(lebro.NoulAnswer)
	if !ok {
		t.Fatalf("Decide() noul answer = %#v, want a noul answer", response.Answers["eligible"])
	}
	if noul.Confidence != nil {
		t.Fatal("the noul answer invented a confidence")
	}
	choice, ok := response.Answers["team"].(lebro.ChoiceAnswer)
	if !ok {
		t.Fatalf("Decide() choice answer = %#v, want a choice answer", response.Answers["team"])
	}
	if choice.Confidence != nil {
		t.Fatal("the choice answer invented a confidence")
	}
	score, ok := response.Answers["urgency"].(lebro.ScoreAnswer)
	if !ok {
		t.Fatalf("Decide() score answer = %#v, want a score answer", response.Answers["urgency"])
	}
	if score.Confidence != nil {
		t.Fatal("the score answer invented a confidence")
	}
}
