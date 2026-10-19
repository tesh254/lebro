package decisions

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/tesh254/lebro"
)

// TestCheckSupportedKindsRejectsUnsupportedQuestions covers the capability
// gate for a provider that does not support a question kind. No shipped
// provider disables a kind, so the gate is exercised directly.
func TestCheckSupportedKindsRejectsUnsupportedQuestions(t *testing.T) {
	profile := systemOneProfile{
		provider: "example",
		capabilities: lebro.DecisionCapabilities{
			Noul:   true,
			Choice: false,
			Score:  false,
		},
	}
	request := lebro.DecisionRequest{
		State: "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"q": lebro.ChoiceQuestion{
				Instructions: "Which one?",
				Criteria:     map[lebro.DecisionOptionID]lebro.JSONValue{"a": "first", "b": "second"},
			},
		},
	}
	err := checkDecisionKinds(profile.capabilities, profile.provider, request)
	if err == nil {
		t.Fatal("checkDecisionKinds() error = nil, want a failure")
	}
	if !strings.Contains(err.Error(), "does not support") {
		t.Fatalf("checkDecisionKinds() error = %v, want it to mention the missing support", err)
	}
}

// TestCheckQuestionLimits covers the documented limits that the shipped
// providers share.
func TestCheckQuestionLimits(t *testing.T) {
	profile := systemOneProfile{
		provider: "example",
		capabilities: lebro.DecisionCapabilities{
			Noul:              true,
			Choice:            true,
			Score:             true,
			MultipleQuestions: false,
			MaxQuestions:      1,
			MaxChoiceOptions:  2,
			MaxScoreLevels:    2,
		},
	}
	twoQuestions := lebro.DecisionRequest{
		State: "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"a": lebro.NoulQuestion{Instructions: "Is it?"},
			"b": lebro.NoulQuestion{Instructions: "Is it?"},
		},
	}
	if err := checkDecisionLimits(profile.capabilities, profile.provider, twoQuestions); err == nil {
		t.Fatal("checkDecisionLimits() error = nil, want a question count failure")
	}
	tooManyOptions := lebro.DecisionRequest{
		State: "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"q": lebro.ChoiceQuestion{
				Instructions: "Which one?",
				Criteria:     map[lebro.DecisionOptionID]lebro.JSONValue{"a": "one", "b": "two", "c": "three"},
			},
		},
	}
	if err := checkDecisionLimits(profile.capabilities, profile.provider, tooManyOptions); err == nil {
		t.Fatal("checkDecisionLimits() error = nil, want an option count failure")
	}
	tooManyLevels := lebro.DecisionRequest{
		State: "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"q": lebro.ScoreQuestion{
				Instructions: "How much?",
				Criteria:     []lebro.JSONValue{"one", "two", "three"},
			},
		},
	}
	if err := checkDecisionLimits(profile.capabilities, profile.provider, tooManyLevels); err == nil {
		t.Fatal("checkDecisionLimits() error = nil, want a level count failure")
	}
	withinLimits := lebro.DecisionRequest{
		State: "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"q": lebro.NoulQuestion{Instructions: "Is it?"},
		},
	}
	if err := checkDecisionLimits(profile.capabilities, profile.provider, withinLimits); err != nil {
		t.Fatalf("checkDecisionLimits() error = %v, want nil", err)
	}
}

// TestSubQRejectsTooManyOptions pins the documented choice option limit.
func TestSubQRejectsTooManyOptions(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"subq-snap-preview","answers":{}}`))
	}))
	defer server.Close()

	model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewSubQ() error = %v", err)
	}
	criteria := make(map[lebro.DecisionOptionID]lebro.JSONValue, 256)
	for i := 0; i < 256; i++ {
		criteria[lebro.DecisionOptionID("option-"+strconv.Itoa(i))] = "description"
	}
	_, err = model.Decide(context.Background(), lebro.DecisionRequest{
		State: "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"q": lebro.ChoiceQuestion{Instructions: "Which one?", Criteria: criteria},
		},
	})
	if !errors.Is(err, lebro.ErrDecisionInvalidRequest) {
		t.Fatalf("Decide() error = %v, want an invalid request error", err)
	}
	if requests != 0 {
		t.Fatalf("the provider received %d requests, want none", requests)
	}
}

// TestTypeSafeRejectsTooManyLevels pins the documented score level limit.
func TestTypeSafeRejectsTooManyLevels(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{}}`))
	}))
	defer server.Close()

	model, err := NewTypeSafe(TypeSafeConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewTypeSafe() error = %v", err)
	}
	levels := make([]lebro.JSONValue, 11)
	for i := range levels {
		levels[i] = "level description"
	}
	_, err = model.Decide(context.Background(), lebro.DecisionRequest{
		State: "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"q": lebro.ScoreQuestion{Instructions: "How much?", Criteria: levels},
		},
	})
	if !errors.Is(err, lebro.ErrDecisionInvalidRequest) {
		t.Fatalf("Decide() error = %v, want an invalid request error", err)
	}
	if requests != 0 {
		t.Fatalf("the provider received %d requests, want none", requests)
	}
}

// TestSubQRejectsOversizedRequestBody pins the documented request body limit.
func TestSubQRejectsOversizedRequestBody(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"subq-snap-preview","answers":{}}`))
	}))
	defer server.Close()

	model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewSubQ() error = %v", err)
	}
	oversized := strings.Repeat("x", subQMaxBodyBytes+1)
	_, err = model.Decide(context.Background(), lebro.DecisionRequest{
		State: oversized,
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"q": lebro.NoulQuestion{Instructions: "Is it?"},
		},
	})
	if !errors.Is(err, lebro.ErrDecisionInvalidRequest) {
		t.Fatalf("Decide() error = %v, want an invalid request error", err)
	}
	if requests != 0 {
		t.Fatalf("the provider received %d requests, want none", requests)
	}
}

// TestFailureCarriesRetryAfter pins that the provider's requested delay
// reaches the caller.
func TestFailureCarriesRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"detail":{"error_type":"rate_limit","message":"slow down"}}`))
	}))
	defer server.Close()

	model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: server.URL, MaxRetries: intPointer(0)})
	if err != nil {
		t.Fatalf("NewSubQ() error = %v", err)
	}
	_, err = model.Decide(context.Background(), lebro.DecisionRequest{
		State:     "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
	})
	if err == nil {
		t.Fatal("Decide() error = nil, want a rate limited failure")
	}
	var decisionErr *lebro.DecisionError
	if !errors.As(err, &decisionErr) {
		t.Fatalf("Decide() error = %v, want a *lebro.DecisionError", err)
	}
	if decisionErr.RetryAfter.Seconds() != 30 {
		t.Fatalf("Decide() retry after = %v, want 30s", decisionErr.RetryAfter)
	}
}

// TestAdapterSendsUserAgent pins the adapter identity on the wire.
func TestAdapterSendsUserAgent(t *testing.T) {
	var userAgent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userAgent = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"subq-snap-preview","answers":{"q":{"type":"noul","noul":0.5}}}`))
	}))
	defer server.Close()

	model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewSubQ() error = %v", err)
	}
	if _, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State:     "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
	}); err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	if userAgent != defaultUserAgent {
		t.Fatalf("User-Agent = %q, want %q", userAgent, defaultUserAgent)
	}
}

// TestAdapterSendsTheSystemOnePath pins the endpoint path.
func TestAdapterSendsTheSystemOnePath(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"subq-snap-preview","answers":{"q":{"type":"noul","noul":0.5}}}`))
	}))
	defer server.Close()

	model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewSubQ() error = %v", err)
	}
	if _, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State:     "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
	}); err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	if path != systemOnePath {
		t.Fatalf("request path = %q, want %q", path, systemOnePath)
	}
}

// TestAdapterRejectsUnparseableBaseURL pins the constructor's URL check.
func TestAdapterRejectsUnparseableBaseURL(t *testing.T) {
	if _, err := NewSubQ(SubQConfig{APIKey: "k", BaseURL: "://not a url"}); err == nil {
		t.Fatal("NewSubQ() error = nil, want an invalid base URL to be rejected")
	}
}

// TestAdapterUsesTheRequestModel pins that a request may select a model
// the adapter has a verified profile for, and that the profile's rules are
// the ones that apply to it.
func TestAdapterUsesTheRequestModel(t *testing.T) {
	var body struct {
		Model string `json:"model"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := decodeBody(r, &body); err != nil {
			t.Errorf("decode request body: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"requested-model-2026","answers":{"q":{"type":"noul","noul":0.5}}}`))
	}))
	defer server.Close()

	config := genericProviderConfig(server.URL)
	config.Model = "configured-default"
	config.Profiles = []SystemOneModelProfile{
		{
			Model: "requested-model",
			Capabilities: lebro.DecisionCapabilities{
				Noul: true,
				NoulUncertainty: lebro.DecisionUncertainty{
					Distribution: lebro.DecisionAvailabilityUnavailable,
					Confidence:   lebro.DecisionAvailabilityUnavailable,
				},
			},
		},
	}
	model, err := NewSystemOne(config)
	if err != nil {
		t.Fatalf("NewSystemOne() error = %v", err)
	}
	response, err := model.Decide(context.Background(), lebro.DecisionRequest{
		Model:     "requested-model",
		State:     "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
	})
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	if body.Model != "requested-model" {
		t.Fatalf("request model = %q, want %q", body.Model, "requested-model")
	}
	if response.RequestedModel != "requested-model" {
		t.Fatalf("Decide() requested model = %q, want %q", response.RequestedModel, "requested-model")
	}
	if response.Model != "requested-model-2026" {
		t.Fatalf("Decide() model = %q, want %q", response.Model, "requested-model-2026")
	}
	capabilities, ok := model.CapabilitiesFor("requested-model")
	if !ok {
		t.Fatal("CapabilitiesFor() = false, want the model to have a profile")
	}
	if capabilities.NoulUncertainty.Confidence != lebro.DecisionAvailabilityUnavailable {
		t.Fatalf("the profile's noul confidence = %q, want it unavailable", capabilities.NoulUncertainty.Confidence)
	}
}

// TestAdapterRejectsAModelWithoutAProfile pins that a request naming a
// model the adapter has no verified profile for is rejected. Answering it
// under the default model's rules would advertise capabilities the named
// model does not have.
func TestAdapterRejectsAModelWithoutAProfile(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","answers":{"q":{"type":"noul","noul":0.5}}}`))
	}))
	defer server.Close()

	model, err := NewSystemOne(genericProviderConfig(server.URL))
	if err != nil {
		t.Fatalf("NewSystemOne() error = %v", err)
	}
	_, err = model.Decide(context.Background(), lebro.DecisionRequest{
		Model:     "unverified-model",
		State:     "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
	})
	if !errors.Is(err, lebro.ErrDecisionUnsupportedCapability) {
		t.Fatalf("Decide() error = %v, want an unsupported capability failure", err)
	}
	if requests != 0 {
		t.Fatalf("the provider received %d requests, want none", requests)
	}
	if _, ok := model.CapabilitiesFor("unverified-model"); ok {
		t.Fatal("CapabilitiesFor() = true, want false for a model without a profile")
	}
}

// decodeBody decodes a request body into target.
func decodeBody(r *http.Request, target any) error {
	return json.NewDecoder(r.Body).Decode(target)
}

// TestRequestBodyCarriesOnlyDocumentedKeys pins that a request never carries
// an unexpected top-level key. Fastino rejects one, and the other providers
// document the same minimal body.
func TestRequestBodyCarriesOnlyDocumentedKeys(t *testing.T) {
	cases := []struct {
		name  string
		build func(baseURL string) (lebro.DecisionModel, error)
		want  []string
		// answer is the response the provider sends. It follows each
		// provider's documented answer shape.
		answer string
	}{
		{
			name: "typesafe",
			build: func(baseURL string) (lebro.DecisionModel, error) {
				return NewTypeSafe(TypeSafeConfig{APIKey: "test-key", BaseURL: baseURL})
			},
			want:   []string{"model", "questions", "state"},
			answer: `{"model":"model","answers":{"q":{"type":"noul","noul":0.5}}}`,
		},
		{
			name: "fastino",
			build: func(baseURL string) (lebro.DecisionModel, error) {
				return NewFastino(FastinoConfig{APIKey: "test-key", BaseURL: baseURL})
			},
			want:   []string{"model", "questions", "state"},
			answer: `{"model":"model","answers":{"q":{"type":"noul","noul":0.5,"confidence":0}}}`,
		},
		{
			name: "subq",
			build: func(baseURL string) (lebro.DecisionModel, error) {
				return NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: baseURL})
			},
			want:   []string{"model", "questions", "state"},
			answer: `{"model":"model","answers":{"q":{"type":"noul","noul":0.5}}}`,
		},
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
				_, _ = w.Write([]byte(testCase.answer))
			}))
			defer server.Close()

			model, err := testCase.build(server.URL)
			if err != nil {
				t.Fatalf("build the %s adapter: %v", testCase.name, err)
			}
			if _, err := model.Decide(context.Background(), lebro.DecisionRequest{
				State:     "state",
				Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
			}); err != nil {
				t.Fatalf("Decide() error = %v", err)
			}
			keys := make([]string, 0, len(body))
			for key := range body {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			if strings.Join(keys, ",") != strings.Join(testCase.want, ",") {
				t.Fatalf("request keys = %v, want %v", keys, testCase.want)
			}
		})
	}
}

// TestFastinoSendsTheStorageOptOutOnlyWhenAsked pins that the documented
// storage flag is the only extra top-level key, and only when the caller
// asked for it.
func TestFastinoSendsTheStorageOptOutOnlyWhenAsked(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"glide","answers":{"q":{"type":"noul","noul":0.5,"confidence":0}}}`))
	}))
	defer server.Close()

	model, err := NewFastino(FastinoConfig{APIKey: "test-key", BaseURL: server.URL, DisableStorage: true})
	if err != nil {
		t.Fatalf("NewFastino() error = %v", err)
	}
	if _, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State:     "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
	}); err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	keys := make([]string, 0, len(body))
	for key := range body {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "model,questions,state,store" {
		t.Fatalf("request keys = %v, want the storage opt-out", keys)
	}
	if store, ok := body["store"].(bool); !ok || store {
		t.Fatalf("request store = %#v, want false", body["store"])
	}
}
