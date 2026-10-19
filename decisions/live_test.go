package decisions

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/tesh254/lebro"
)

// Live decision tests run against the real providers. They are skipped unless
// the matching credential is present, so the default suite stays offline.
//
// The credential variables are:
//
//	LEBRO_TYPESAFE_API_KEY
//	LEBRO_FASTINO_API_KEY
//	LEBRO_SUBQ_API_KEY
//
// A run reports the provider, the model it echoed, the latency, the usage, the
// modality it exercised, and the uncertainty it observed. Those observations
// are the evidence a live run produces; the fixture suite is the regression
// gate.
func TestLiveTypeSafe(t *testing.T) {
	apiKey := os.Getenv("LEBRO_TYPESAFE_API_KEY")
	if apiKey == "" {
		t.Skip("skipping the live TypeSafe decision test: set LEBRO_TYPESAFE_API_KEY to run it")
	}
	model, err := NewTypeSafe(TypeSafeConfig{APIKey: apiKey})
	if err != nil {
		t.Fatalf("NewTypeSafe() error = %v", err)
	}
	runLiveDecision(t, model, "text")
}

func TestLiveFastino(t *testing.T) {
	apiKey := os.Getenv("LEBRO_FASTINO_API_KEY")
	if apiKey == "" {
		t.Skip("skipping the live Fastino decision test: set LEBRO_FASTINO_API_KEY to run it")
	}
	model, err := NewFastino(FastinoConfig{APIKey: apiKey})
	if err != nil {
		t.Fatalf("NewFastino() error = %v", err)
	}
	runLiveDecision(t, model, "text")
}

func TestLiveSubQ(t *testing.T) {
	apiKey := os.Getenv("LEBRO_SUBQ_API_KEY")
	if apiKey == "" {
		t.Skip("skipping the live SubQ decision test: set LEBRO_SUBQ_API_KEY to run it")
	}
	model, err := NewSubQ(SubQConfig{APIKey: apiKey})
	if err != nil {
		t.Fatalf("NewSubQ() error = %v", err)
	}
	runLiveDecision(t, model, "text")
}

// TestLiveTypeSafeViaOpenRouter runs the same adapter against a gateway that
// fronts TypeSafe. Only the endpoint, the credential, and the model alias
// change, which is the point: the contract and the application code do not.
func TestLiveTypeSafeViaOpenRouter(t *testing.T) {
	apiKey := os.Getenv("LEBRO_OPENROUTER_API_KEY")
	if apiKey == "" {
		t.Skip("skipping the live OpenRouter decision test: set LEBRO_OPENROUTER_API_KEY to run it")
	}
	model, err := NewTypeSafe(TypeSafeConfig{
		APIKey:  apiKey,
		BaseURL: "https://openrouter.ai/api",
		Model:   "~typesafe/jev-latest",
	})
	if err != nil {
		t.Fatalf("NewTypeSafe() error = %v", err)
	}
	runLiveDecision(t, model, "text")
}

// runLiveDecision asks one question of each kind and reports what the
// provider returned.
func runLiveDecision(t *testing.T, model lebro.DecisionModel, modality string) {
	t.Helper()
	// A live provider answers over HTTP/2, and the pooled connection keeps a
	// reader goroutine alive after the response. Releasing it keeps the
	// goroutine check honest instead of hiding a real leak.
	t.Cleanup(func() { closeIdleConnections() })
	started := time.Now()
	response, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State: "My card was charged twice for order A-104 and I need a refund today.",
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
					"shipping":  "Delivery and tracking",
				},
			},
			"urgency": lebro.ScoreQuestion{
				Instructions: "How urgent is this request?",
				Criteria:     []lebro.JSONValue{"Can wait a week", "Should be handled today", "Immediate"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("Decide() response error = %v", err)
	}
	noul, ok := response.Answers["is_refund"].(lebro.NoulAnswer)
	if !ok {
		t.Fatalf("the noul answer = %#v, want a noul answer", response.Answers["is_refund"])
	}
	choice, ok := response.Answers["department"].(lebro.ChoiceAnswer)
	if !ok {
		t.Fatalf("the choice answer = %#v, want a choice answer", response.Answers["department"])
	}
	score, ok := response.Answers["urgency"].(lebro.ScoreAnswer)
	if !ok {
		t.Fatalf("the score answer = %#v, want a score answer", response.Answers["urgency"])
	}
	t.Logf("provider=%s upstream=%s requested_model=%s model=%s modality=%s latency=%s attempts=%d",
		response.Provider, formatLiveUpstream(response.UpstreamProvider), response.RequestedModel,
		response.Model, modality, time.Since(started), response.Attempts)
	t.Logf("usage=%s", formatLiveUsage(response.Usage))
	t.Logf("noul probability=%.4f confidence=%s metric=%s",
		noul.ProbabilityYes, formatLiveConfidence(noul.Confidence), formatLiveMetric(noul.ConfidenceMetric))
	t.Logf("choice=%s confidence=%s metric=%s",
		choice.Choice, formatLiveConfidence(choice.Confidence), formatLiveMetric(choice.ConfidenceMetric))
	t.Logf("score expected=%s selected=%s confidence=%s metric=%s",
		formatLiveLevel(score.ExpectedLevel), formatLiveSelected(score.SelectedLevel),
		formatLiveConfidence(score.Confidence), formatLiveMetric(score.ConfidenceMetric))
}

// formatLiveUpstream renders the upstream provider.
func formatLiveUpstream(upstream lebro.ProviderID) string {
	if upstream == "" {
		return "none"
	}
	return string(upstream)
}

// formatLiveMetric renders the metric a reported confidence means.
func formatLiveMetric(metric lebro.DecisionConfidenceMetric) string {
	if metric == "" {
		return "none"
	}
	return string(metric)
}

// closeIdleConnections releases the pooled provider connections.
func closeIdleConnections() {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
}

// formatLiveUsage renders the reported usage.
func formatLiveUsage(usage *lebro.DecisionUsage) string {
	if usage == nil {
		return "none"
	}
	input, output := "none", "none"
	if usage.InputTokens != nil {
		input = formatLiveInt(*usage.InputTokens)
	}
	if usage.OutputTokens != nil {
		output = formatLiveInt(*usage.OutputTokens)
	}
	return "input=" + input + " output=" + output
}

// formatLiveConfidence renders an optional confidence.
func formatLiveConfidence(confidence *float64) string {
	if confidence == nil {
		return "none"
	}
	return formatLiveFloat(*confidence)
}

// formatLiveLevel renders an optional weighted level.
func formatLiveLevel(level *float64) string {
	if level == nil {
		return "none"
	}
	return formatLiveFloat(*level)
}

// formatLiveSelected renders an optional winning level.
func formatLiveSelected(level *int) string {
	if level == nil {
		return "none"
	}
	return formatLiveInt(int64(*level))
}

func formatLiveFloat(value float64) string {
	return strconv.FormatFloat(value, 'f', 4, 64)
}

func formatLiveInt(value int64) string {
	return strconv.FormatInt(value, 10)
}
