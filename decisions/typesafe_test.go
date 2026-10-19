package decisions

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
)

// typeSafeFixture describes TypeSafe for the shared contract suite.
func typeSafeFixture() decisionFixture {
	return decisionFixture{
		name: "typesafe",
		newModel: func(baseURL string) (lebro.DecisionModel, error) {
			return NewTypeSafe(TypeSafeConfig{APIKey: "test-key", BaseURL: baseURL, Model: "jev-latest"})
		},
		scoreWeightedField: "score",
		scoreWinnerField:   "",
		noulConfidence:     false,
		model:              "jev-1.13.0",
	}
}

// TestTypeSafeContract runs the shared decision suite against TypeSafe.
func TestTypeSafeContract(t *testing.T) {
	testkit.RunDecisionContract(t, decisionContractFactory(typeSafeFixture()))
}

// TestTypeSafeNoulHasNoConfidence pins the provider difference that Jev
// reports no confidence for a noul answer. A confidence derived from the
// probability would be a fabricated value.
func TestTypeSafeNoulHasNoConfidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(typeSafeRequestIDHeader, "req-42")
		_, _ = w.Write([]byte(`{
			"model": "jev-1.13.0",
			"answers": {"urgent": {"type": "noul", "noul": 0.95}},
			"usage": {"input_tokens": 296, "output_tokens": 20}
		}`))
	}))
	defer server.Close()

	model, err := NewTypeSafe(TypeSafeConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewTypeSafe() error = %v", err)
	}
	response, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State: "My payouts have been failing for three days.",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"urgent": lebro.NoulQuestion{Instructions: "Does this convey urgency?"},
		},
	})
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	answer, ok := response.Answers["urgent"].(lebro.NoulAnswer)
	if !ok {
		t.Fatalf("Decide() answer = %#v, want a noul answer", response.Answers["urgent"])
	}
	if answer.ProbabilityYes != 0.95 {
		t.Fatalf("Decide() probability = %v, want 0.95", answer.ProbabilityYes)
	}
	if answer.Confidence != nil {
		t.Fatalf("Decide() confidence = %v, want no confidence", *answer.Confidence)
	}
	if response.Model != "jev-1.13.0" {
		t.Fatalf("Decide() model = %q, want %q", response.Model, "jev-1.13.0")
	}
	if response.RequestID != "req-42" {
		t.Fatalf("Decide() request ID = %q, want %q", response.RequestID, "req-42")
	}
	if response.Usage == nil || response.Usage.InputTokens == nil || *response.Usage.InputTokens != 296 {
		t.Fatalf("Decide() usage = %#v, want 296 input tokens", response.Usage)
	}
	if response.Usage.OutputTokens == nil || *response.Usage.OutputTokens != 20 {
		t.Fatalf("Decide() output tokens = %#v, want 20", response.Usage.OutputTokens)
	}
	if response.Attempts != 1 {
		t.Fatalf("Decide() attempts = %d, want 1", response.Attempts)
	}
}

// TestTypeSafeScoreHasNoNativeWinner pins that Jev reports a weighted score
// and no winning level. Reporting an argmax would invent a winner.
func TestTypeSafeScoreHasNoNativeWinner(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "jev-1.13.0",
			"answers": {"frustration": {
				"type": "score",
				"score": 1.05,
				"legend": {"0": "Calm", "1": "Frustrated", "2": "Very angry"},
				"probabilities": {"0": 0.0, "1": 0.95, "2": 0.05},
				"confidence": 0.92
			}},
			"usage": {"input_tokens": 304, "output_tokens": 18}
		}`))
	}))
	defer server.Close()

	model, err := NewTypeSafe(TypeSafeConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewTypeSafe() error = %v", err)
	}
	response, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State: "Support requests are piling up.",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"frustration": lebro.ScoreQuestion{
				Instructions: "How frustrated is the customer?",
				Criteria:     []lebro.JSONValue{"Calm", "Frustrated", "Very angry"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	answer, ok := response.Answers["frustration"].(lebro.ScoreAnswer)
	if !ok {
		t.Fatalf("Decide() answer = %#v, want a score answer", response.Answers["frustration"])
	}
	if answer.SelectedLevel != nil {
		t.Fatalf("Decide() selected level = %d, want no native winner", *answer.SelectedLevel)
	}
	if answer.ExpectedLevel == nil || *answer.ExpectedLevel != 1.05 {
		t.Fatalf("Decide() expected level = %#v, want 1.05", answer.ExpectedLevel)
	}
	if answer.Provenance != lebro.ScoreNative {
		t.Fatalf("Decide() provenance = %q, want %q", answer.Provenance, lebro.ScoreNative)
	}
	if len(answer.Probabilities) != 3 || answer.Probabilities[1] != 0.95 {
		t.Fatalf("Decide() probabilities = %#v, want three levels", answer.Probabilities)
	}
	if len(answer.Legend) != 3 || answer.Legend[2] != "Very angry" {
		t.Fatalf("Decide() legend = %#v, want three levels", answer.Legend)
	}
	if answer.Confidence == nil || *answer.Confidence != 0.92 {
		t.Fatalf("Decide() confidence = %#v, want 0.92", answer.Confidence)
	}
}

// TestTypeSafeSendsBearerToken pins the credential placement.
func TestTypeSafeSendsBearerToken(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"q":{"type":"noul","noul":0.5}}}`))
	}))
	defer server.Close()

	model, err := NewTypeSafe(TypeSafeConfig{APIKey: "secret-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewTypeSafe() error = %v", err)
	}
	if _, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State:     "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
	}); err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	if authorization != "Bearer secret-key" {
		t.Fatalf("Authorization header = %q, want %q", authorization, "Bearer secret-key")
	}
}

// TestTypeSafeRejectsImages pins that an image request fails before HTTP
// when the adapter does not advertise image support.
func TestTypeSafeRejectsImages(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"q":{"type":"noul","noul":0.5}}}`))
	}))
	defer server.Close()

	model, err := NewTypeSafe(TypeSafeConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewTypeSafe() error = %v", err)
	}
	payload := "not-an-image-payload-3f9a2b"
	_, err = model.Decide(context.Background(), lebro.DecisionRequest{
		State:  "state",
		Images: []lebro.DecisionImage{{MediaType: "image/png", Bytes: []byte(payload)}},
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
	if strings.Contains(err.Error(), payload) {
		t.Fatalf("Decide() error = %v, want the image payload kept out of the message", err)
	}
}

// TestTypeSafeCredentialsStayOutOfErrors pins that a credential never
// reaches an error message.
func TestTypeSafeCredentialsStayOutOfErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key","code":"bad_key"}}`))
	}))
	defer server.Close()

	model, err := NewTypeSafe(TypeSafeConfig{APIKey: "super-secret-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewTypeSafe() error = %v", err)
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
	var decisionErr *lebro.DecisionError
	if !errors.As(err, &decisionErr) {
		t.Fatalf("Decide() error = %v, want a *lebro.DecisionError", err)
	}
	if decisionErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Decide() status = %d, want %d", decisionErr.StatusCode, http.StatusUnauthorized)
	}
	if decisionErr.Code != "bad_key" {
		t.Fatalf("Decide() code = %q, want %q", decisionErr.Code, "bad_key")
	}
}
