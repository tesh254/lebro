package decisions

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
)

// fastinoFixture describes Fastino for the shared contract suite.
func fastinoFixture() decisionFixture {
	return decisionFixture{
		name: "fastino",
		newModel: func(baseURL string) (lebro.DecisionModel, error) {
			return NewFastino(FastinoConfig{APIKey: "test-key", BaseURL: baseURL, Model: "fastino/GLiDE"})
		},
		scoreWeightedField:     "expected_level",
		scoreWinnerField:       "score",
		noulConfidence:         true,
		noulConfidenceMetric:   lebro.DecisionConfidenceDistanceFromHalf,
		choiceConfidenceMetric: lebro.DecisionConfidenceTopTwoMargin,
		scoreConfidenceMetric:  lebro.DecisionConfidenceTopTwoMargin,
		model:                  "glide",
		requestedModel:         "fastino/GLiDE",
	}
}

// TestFastinoContract runs the shared decision suite against Fastino.
func TestFastinoContract(t *testing.T) {
	testkit.RunDecisionContract(t, decisionContractFactory(fastinoFixture()))
}

// TestFastinoKeepsWinnerAndExpectationApart pins that GLiDE's winning level
// and its probability-weighted value stay distinguishable. Mapping the
// weighted value onto the winning level would lose the provider's meaning.
func TestFastinoKeepsWinnerAndExpectationApart(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "glide",
			"answers": {"urgency": {
				"type": "score",
				"score": 2,
				"expected_level": 1.99,
				"confidence": 0.9925,
				"probabilities": {"0": 0.0035, "1": 0.0004, "2": 0.996},
				"legend": {"0": "low urgency", "1": "medium urgency", "2": "high urgency"}
			}},
			"usage": {"input_tokens": 743, "output_tokens": 252},
			"token_usage": 995
		}`))
	}))
	defer server.Close()

	model, err := NewFastino(FastinoConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewFastino() error = %v", err)
	}
	response, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State: "A refund request is waiting.",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"urgency": lebro.ScoreQuestion{
				Instructions: "How urgent is this request?",
				Criteria:     []lebro.JSONValue{"low urgency", "medium urgency", "high urgency"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	answer, ok := response.Answers["urgency"].(lebro.ScoreAnswer)
	if !ok {
		t.Fatalf("Decide() answer = %#v, want a score answer", response.Answers["urgency"])
	}
	if answer.SelectedLevel == nil || *answer.SelectedLevel != 2 {
		t.Fatalf("Decide() selected level = %#v, want 2", answer.SelectedLevel)
	}
	if answer.ExpectedLevel == nil || *answer.ExpectedLevel != 1.99 {
		t.Fatalf("Decide() expected level = %#v, want 1.99", answer.ExpectedLevel)
	}
	if answer.SelectedLevel != nil && answer.ExpectedLevel != nil && *answer.SelectedLevel == int(*answer.ExpectedLevel) {
		t.Fatal("Decide() conflated the winning level with the weighted value")
	}
	if answer.Provenance != lebro.ScoreNative {
		t.Fatalf("Decide() provenance = %q, want %q", answer.Provenance, lebro.ScoreNative)
	}
	if answer.Probabilities[2] != 0.996 {
		t.Fatalf("Decide() probabilities = %#v, want 0.996 for the winning level", answer.Probabilities)
	}
	if answer.Confidence == nil || *answer.Confidence != 0.9925 {
		t.Fatalf("Decide() confidence = %#v, want 0.9925", answer.Confidence)
	}
}

// TestFastinoReportsNoulConfidence pins that GLiDE's noul confidence is
// reported rather than dropped.
func TestFastinoReportsNoulConfidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "glide",
			"answers": {"refund_allowed": {"type": "noul", "noul": 0.99767683794902, "confidence": 0.9953536758980399}}
		}`))
	}))
	defer server.Close()

	model, err := NewFastino(FastinoConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewFastino() error = %v", err)
	}
	response, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State: "Refund request with a receipt attached.",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"refund_allowed": lebro.NoulQuestion{Instructions: "Does this request qualify for a refund?"},
		},
	})
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	answer, ok := response.Answers["refund_allowed"].(lebro.NoulAnswer)
	if !ok {
		t.Fatalf("Decide() answer = %#v, want a noul answer", response.Answers["refund_allowed"])
	}
	if answer.Confidence == nil || *answer.Confidence != 0.9953536758980399 {
		t.Fatalf("Decide() confidence = %#v, want the provider confidence", answer.Confidence)
	}
}

// TestFastinoRejectsStructuredInstructions pins that a request GLiDE cannot
// represent fails before HTTP.
func TestFastinoRejectsStructuredInstructions(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"glide","answers":{}}`))
	}))
	defer server.Close()

	model, err := NewFastino(FastinoConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewFastino() error = %v", err)
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

// TestFastinoSendsAPIKeyHeader pins the credential placement.
func TestFastinoSendsAPIKeyHeader(t *testing.T) {
	var apiKey, authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey = r.Header.Get("X-API-Key")
		authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"glide","answers":{"q":{"type":"noul","noul":0.5,"confidence":0}}}`))
	}))
	defer server.Close()

	model, err := NewFastino(FastinoConfig{APIKey: "secret-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewFastino() error = %v", err)
	}
	if _, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State:     "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
	}); err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	if apiKey != "secret-key" {
		t.Fatalf("X-API-Key header = %q, want %q", apiKey, "secret-key")
	}
	if authorization != "" {
		t.Fatalf("Authorization header = %q, want it empty", authorization)
	}
}

// TestFastinoDisableStorage pins the documented privacy opt-out.
func TestFastinoDisableStorage(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		body = string(raw)
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
	if !strings.Contains(body, `"store":false`) {
		t.Fatalf("request body = %s, want the storage opt-out", body)
	}
}

// TestFastinoCredentialsStayOutOfErrors pins that a credential never
// reaches an error message.
func TestFastinoCredentialsStayOutOfErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"invalid api key"}`))
	}))
	defer server.Close()

	model, err := NewFastino(FastinoConfig{APIKey: "super-secret-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewFastino() error = %v", err)
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
