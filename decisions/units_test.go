package decisions

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tesh254/lebro"
)

// TestExtractProviderError pins the error body shapes the adapters read. Only
// documented message fields are read, so a validation error cannot echo
// request content back into an error message.
func TestExtractProviderError(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantMessage string
		wantCode    string
	}{
		{
			name:        "detail object",
			body:        `{"detail":{"error_type":"validation_error","message":"questions/queue: must have required property 'criteria'"}}`,
			wantMessage: "questions/queue: must have required property 'criteria'",
			wantCode:    "validation_error",
		},
		{
			name:        "detail string",
			body:        `{"detail":"invalid api key"}`,
			wantMessage: "invalid api key",
		},
		{
			name:        "detail list",
			body:        `{"detail":[{"loc":["body","model"],"msg":"field required","type":"missing"}]}`,
			wantMessage: "field required",
		},
		{
			name:        "detail list with several problems",
			body:        `{"detail":[{"msg":"field required"},{"msg":"extra inputs are not permitted"}]}`,
			wantMessage: "field required; extra inputs are not permitted",
		},
		{
			name:        "error object",
			body:        `{"error":{"message":"invalid key","type":"invalid_request_error","code":"bad_key"}}`,
			wantMessage: "invalid key",
			wantCode:    "bad_key",
		},
		{
			name:        "error object without a code",
			body:        `{"error":{"message":"invalid key","type":"invalid_request_error"}}`,
			wantMessage: "invalid key",
			wantCode:    "invalid_request_error",
		},
		{
			name:        "error string",
			body:        `{"error":"something went wrong"}`,
			wantMessage: "something went wrong",
		},
		{
			name:        "message and code",
			body:        `{"message":"quota exceeded","code":"rate_limit"}`,
			wantMessage: "quota exceeded",
			wantCode:    "rate_limit",
		},
		{
			name:        "not json",
			body:        `plain text failure`,
			wantMessage: "",
			wantCode:    "",
		},
		{
			name:        "unknown shape",
			body:        `{"unexpected":{"nested":"value"}}`,
			wantMessage: "",
			wantCode:    "",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			message, code := extractProviderError([]byte(testCase.body))
			if message != testCase.wantMessage {
				t.Fatalf("message = %q, want %q", message, testCase.wantMessage)
			}
			if code != testCase.wantCode {
				t.Fatalf("code = %q, want %q", code, testCase.wantCode)
			}
		})
	}
}

// TestExtractProviderErrorKeepsRequestContentOut pins that a provider error
// body is never echoed verbatim, so an echoed payload cannot reach an error
// message.
func TestExtractProviderErrorKeepsRequestContentOut(t *testing.T) {
	body := `{"detail":{"error_type":"validation_error","message":"images/0: bad image","input":"iVBORw0KGgoAAAANSUhEUg=="}}`
	message, _ := extractProviderError([]byte(body))
	if strings.Contains(message, "iVBORw0KGgo") {
		t.Fatalf("message = %q, want the request payload kept out", message)
	}
}

// TestTruncateErrorText pins that a long provider description is bounded.
func TestTruncateErrorText(t *testing.T) {
	long := strings.Repeat("x", maxErrorMessageLength*2)
	truncated := truncateErrorText(long)
	if len(truncated) > maxErrorMessageLength+len("…") {
		t.Fatalf("the truncated length = %d, want at most %d plus the ellipsis", len(truncated), maxErrorMessageLength)
	}
	if len(truncated) <= maxErrorMessageLength {
		t.Fatalf("the truncated length = %d, want the bound to be applied", len(truncated))
	}
	if !strings.HasSuffix(truncated, "…") {
		t.Fatalf("the truncated text = %q, want an ellipsis", truncated)
	}
	if got := truncateErrorText("  short  "); got != "short" {
		t.Fatalf("truncateErrorText() = %q, want %q", got, "short")
	}
}

// TestCheckSupportedKindsRejectsEveryKind pins the capability gate for each
// question kind.
func TestCheckSupportedKindsRejectsEveryKind(t *testing.T) {
	cases := []struct {
		name     string
		caps     lebro.DecisionCapabilities
		question lebro.DecisionQuestion
	}{
		{
			name:     "noul",
			caps:     lebro.DecisionCapabilities{Choice: true, Score: true},
			question: lebro.NoulQuestion{Instructions: "Is it?"},
		},
		{
			name:     "choice",
			caps:     lebro.DecisionCapabilities{Noul: true, Score: true},
			question: lebro.ChoiceQuestion{Instructions: "Which?", Criteria: map[lebro.DecisionOptionID]lebro.JSONValue{"a": "one", "b": "two"}},
		},
		{
			name:     "score",
			caps:     lebro.DecisionCapabilities{Noul: true, Choice: true},
			question: lebro.ScoreQuestion{Instructions: "How much?", Criteria: []lebro.JSONValue{"low", "high"}},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			profile := systemOneProfile{provider: "example", capabilities: testCase.caps}
			err := checkDecisionKinds(profile.capabilities, profile.provider, lebro.DecisionRequest{
				State:     "state",
				Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": testCase.question},
			})
			if err == nil {
				t.Fatal("checkDecisionKinds() error = nil, want a failure")
			}
			if !strings.Contains(err.Error(), "does not support") {
				t.Fatalf("checkDecisionKinds() error = %v, want it to mention the missing support", err)
			}
		})
	}
}

// TestMapSystemOneLegendRejectsBadLegends pins the legend validation.
func TestMapSystemOneLegendRejectsBadLegends(t *testing.T) {
	cases := []struct {
		name   string
		legend map[string]json.RawMessage
		want   string
	}{
		{
			name:   "key is not a level index",
			legend: map[string]json.RawMessage{"low": json.RawMessage(`"low"`)},
			want:   "not a level index",
		},
		{
			name:   "level out of range",
			legend: map[string]json.RawMessage{"5": json.RawMessage(`"five"`)},
			want:   "outside the requested range",
		},
		{
			name:   "entry is not valid json",
			legend: map[string]json.RawMessage{"0": json.RawMessage(`{`)},
			want:   "not valid JSON",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := mapSystemOneLegend("q", 2, testCase.legend)
			if err == nil {
				t.Fatal("mapSystemOneLegend() error = nil, want a failure")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("mapSystemOneLegend() error = %v, want it to mention %q", err, testCase.want)
			}
		})
	}
}

// TestTextOnlyQuestionsRejectsStructuredValues pins that a provider that
// accepts text only refuses structured instructions and criteria.
func TestTextOnlyQuestionsRejectsStructuredValues(t *testing.T) {
	cases := []struct {
		name      string
		questions map[lebro.DecisionQuestionID]lebro.DecisionQuestion
		want      string
	}{
		{
			name: "structured instructions",
			questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
				"q": lebro.NoulQuestion{Instructions: map[string]any{"question": "Is it?"}},
			},
			want: "requires text instructions",
		},
		{
			name: "structured noul criteria",
			questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
				"q": lebro.NoulQuestion{
					Instructions: "Is it?",
					Criteria:     &lebro.NoulCriteria{True: map[string]any{"yes": "means yes"}},
				},
			},
			want: "requires text criteria",
		},
		{
			name: "structured choice criteria",
			questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
				"q": lebro.ChoiceQuestion{
					Instructions: "Which one?",
					Criteria:     map[lebro.DecisionOptionID]lebro.JSONValue{"a": []any{"one"}},
				},
			},
			want: "requires text criteria",
		},
		{
			name: "structured score criteria",
			questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
				"q": lebro.ScoreQuestion{
					Instructions: "How much?",
					Criteria:     []lebro.JSONValue{map[string]any{"level": "low"}, "high"},
				},
			},
			want: "requires text criteria",
		},
		{
			name: "empty text criteria",
			questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
				"q": lebro.ScoreQuestion{
					Instructions: "How much?",
					Criteria:     []lebro.JSONValue{"  ", "high"},
				},
			},
			want: "requires non-empty criteria",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := buildTextOnlyQuestions(testCase.questions)
			if err == nil {
				t.Fatal("buildTextOnlyQuestions() error = nil, want a failure")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("buildTextOnlyQuestions() error = %v, want it to mention %q", err, testCase.want)
			}
		})
	}
}

// TestTextOnlyQuestionsAcceptsTextCriteria pins that a text-only provider
// still renders every question kind.
func TestTextOnlyQuestionsAcceptsTextCriteria(t *testing.T) {
	rendered, err := buildTextOnlyQuestions(map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
		"noul": lebro.NoulQuestion{
			Instructions: "Is it?",
			Criteria:     &lebro.NoulCriteria{True: "yes means yes", False: "no means no"},
		},
		"choice": lebro.ChoiceQuestion{
			Instructions: "Which one?",
			Criteria:     map[lebro.DecisionOptionID]lebro.JSONValue{"a": "one", "b": "two"},
		},
		"score": lebro.ScoreQuestion{
			Instructions: "How much?",
			Criteria:     []lebro.JSONValue{"low", "high"},
		},
	})
	if err != nil {
		t.Fatalf("buildTextOnlyQuestions() error = %v, want nil", err)
	}
	if len(rendered) != 3 {
		t.Fatalf("rendered %d questions, want 3", len(rendered))
	}
}

// TestTransportFailureIsRetried pins that a transport failure is retried
// inside the bounded retry policy.
func TestTransportFailureIsRetried(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			// Drop the connection so the client sees a transport failure
			// rather than a response.
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("the test server does not support hijacking")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack the connection: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"subq-snap-preview","answers":{"q":{"type":"noul","noul":0.5}}}`))
	}))
	defer server.Close()

	model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: server.URL, RetryBackoff: time.Millisecond})
	if err != nil {
		t.Fatalf("NewSubQ() error = %v", err)
	}
	response, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State:     "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
	})
	if err != nil {
		t.Fatalf("Decide() error = %v, want a successful retry", err)
	}
	if attempts != 2 {
		t.Fatalf("the provider received %d attempts, want 2", attempts)
	}
	if response.Attempts != 2 {
		t.Fatalf("Decide() attempts = %d, want 2", response.Attempts)
	}
}

// TestCancellationDuringTheBackoffStops pins that a cancelled caller stops
// the retry loop instead of waiting out the provider's delay.
func TestCancellationDuringTheBackoffStops(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Retry-After", "30")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"detail":{"error_type":"rate_limit","message":"slow down"}}`))
	}))
	defer server.Close()

	model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: server.URL, RetryBackoff: time.Millisecond})
	if err != nil {
		t.Fatalf("NewSubQ() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	defer cancel()
	started := time.Now()
	_, err = model.Decide(ctx, lebro.DecisionRequest{
		State:     "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
	})
	if err == nil {
		t.Fatal("Decide() error = nil, want a cancellation failure")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Decide() error = %v, want the context cancellation preserved", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("Decide() waited %v, want it to stop at the cancellation", elapsed)
	}
	if attempts != 1 {
		t.Fatalf("the provider received %d attempts, want 1", attempts)
	}
}

// TestNoulCriteriaOnTheWire pins how the noul criteria reach the provider:
// both descriptions, one description, or none at all.
func TestNoulCriteriaOnTheWire(t *testing.T) {
	cases := []struct {
		name     string
		criteria *lebro.NoulCriteria
		want     map[string]any
	}{
		{
			name:     "both descriptions",
			criteria: &lebro.NoulCriteria{True: "yes means yes", False: "no means no"},
			want:     map[string]any{"true": "yes means yes", "false": "no means no"},
		},
		{
			name:     "only the true description",
			criteria: &lebro.NoulCriteria{True: "yes means yes"},
			want:     map[string]any{"true": "yes means yes"},
		},
		{
			name:     "only the false description",
			criteria: &lebro.NoulCriteria{False: "no means no"},
			want:     map[string]any{"false": "no means no"},
		},
		{
			name:     "no criteria",
			criteria: nil,
			want:     nil,
		},
		{
			name:     "an empty criteria pair",
			criteria: &lebro.NoulCriteria{},
			want:     nil,
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
				_, _ = w.Write([]byte(`{"model":"subq-snap-preview","answers":{"q":{"type":"noul","noul":0.5}}}`))
			}))
			defer server.Close()

			model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: server.URL})
			if err != nil {
				t.Fatalf("NewSubQ() error = %v", err)
			}
			if _, err := model.Decide(context.Background(), lebro.DecisionRequest{
				State: "state",
				Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
					"q": lebro.NoulQuestion{Instructions: "Is it?", Criteria: testCase.criteria},
				},
			}); err != nil {
				t.Fatalf("Decide() error = %v", err)
			}
			questions, ok := body["questions"].(map[string]any)
			if !ok {
				t.Fatalf("request questions = %#v, want an object", body["questions"])
			}
			question, ok := questions["q"].(map[string]any)
			if !ok {
				t.Fatalf("request question = %#v, want an object", questions["q"])
			}
			if question["type"] != "noul" {
				t.Fatalf("request question type = %#v, want %q", question["type"], "noul")
			}
			criteria, present := question["criteria"]
			if testCase.want == nil {
				if present {
					t.Fatalf("request criteria = %#v, want none", criteria)
				}
				return
			}
			if !present {
				t.Fatalf("request question = %#v, want criteria", question)
			}
			got, ok := criteria.(map[string]any)
			if !ok {
				t.Fatalf("request criteria = %#v, want an object", criteria)
			}
			if len(got) != len(testCase.want) {
				t.Fatalf("request criteria = %#v, want %#v", got, testCase.want)
			}
			for key, value := range testCase.want {
				if got[key] != value {
					t.Fatalf("request criteria = %#v, want %#v", got, testCase.want)
				}
			}
		})
	}
}
