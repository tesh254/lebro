package decisions

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
	err := checkSupportedKinds(profile, request)
	if err == nil {
		t.Fatal("checkSupportedKinds() error = nil, want a failure")
	}
	if !strings.Contains(err.Error(), "does not support") {
		t.Fatalf("checkSupportedKinds() error = %v, want it to mention the missing support", err)
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
	if err := checkQuestionLimits(profile, twoQuestions); err == nil {
		t.Fatal("checkQuestionLimits() error = nil, want a question count failure")
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
	if err := checkQuestionLimits(profile, tooManyOptions); err == nil {
		t.Fatal("checkQuestionLimits() error = nil, want an option count failure")
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
	if err := checkQuestionLimits(profile, tooManyLevels); err == nil {
		t.Fatal("checkQuestionLimits() error = nil, want a level count failure")
	}
	withinLimits := lebro.DecisionRequest{
		State: "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"q": lebro.NoulQuestion{Instructions: "Is it?"},
		},
	}
	if err := checkQuestionLimits(profile, withinLimits); err != nil {
		t.Fatalf("checkQuestionLimits() error = %v, want nil", err)
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

// TestAdapterUsesTheRequestModel pins that a request may select a model.
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
		_, _ = w.Write([]byte(`{"model":"subq-snap-preview","answers":{"q":{"type":"noul","noul":0.5}}}`))
	}))
	defer server.Close()

	model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: server.URL, Model: "subq-snap-preview"})
	if err != nil {
		t.Fatalf("NewSubQ() error = %v", err)
	}
	if _, err := model.Decide(context.Background(), lebro.DecisionRequest{
		Model:     "subq-snap-preview",
		State:     "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
	}); err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	if body.Model != "subq-snap-preview" {
		t.Fatalf("request model = %q, want %q", body.Model, "subq-snap-preview")
	}
}

// decodeBody decodes a request body into target.
func decodeBody(r *http.Request, target any) error {
	return json.NewDecoder(r.Body).Decode(target)
}
