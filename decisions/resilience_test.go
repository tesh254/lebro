package decisions

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tesh254/lebro"
)

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "empty", value: ""},
		{name: "seconds", value: "120", want: 2 * time.Minute},
		{name: "zero seconds", value: "0"},
		{name: "negative seconds", value: "-5"},
		{name: "not a number", value: "soon"},
		{name: "past date", value: "Mon, 02 Jan 2006 15:04:05 GMT"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := parseRetryAfter(testCase.value); got != testCase.want {
				t.Fatalf("parseRetryAfter(%q) = %v, want %v", testCase.value, got, testCase.want)
			}
		})
	}
}

func TestRetriesRateLimitAndHonorsRetryAfter(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"detail":{"error_type":"rate_limit","message":"slow down"}}`))
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
	started := time.Now()
	response, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State:     "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
	})
	if err != nil {
		t.Fatalf("Decide() error = %v, want a successful retry", err)
	}
	elapsed := time.Since(started)
	if attempts != 2 {
		t.Fatalf("the provider received %d attempts, want 2", attempts)
	}
	if response.Attempts != 2 {
		t.Fatalf("Decide() attempts = %d, want 2", response.Attempts)
	}
	if elapsed < time.Second {
		t.Fatalf("the retry waited %v, want the Retry-After delay of at least 1s", elapsed)
	}
}

func TestRetriesAreBounded(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"detail":{"error_type":"rate_limit","message":"slow down"}}`))
	}))
	defer server.Close()

	model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: server.URL, MaxRetries: intPointer(2), RetryBackoff: time.Millisecond})
	if err != nil {
		t.Fatalf("NewSubQ() error = %v", err)
	}
	_, err = model.Decide(context.Background(), lebro.DecisionRequest{
		State:     "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
	})
	if !errors.Is(err, lebro.ErrDecisionRateLimited) {
		t.Fatalf("Decide() error = %v, want a rate limited failure", err)
	}
	if attempts != 3 {
		t.Fatalf("the provider received %d attempts, want 3", attempts)
	}
}

func TestDoesNotRetryInvalidRequests(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"detail":{"error_type":"validation_error","message":"bad request"}}`))
	}))
	defer server.Close()

	model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: server.URL, MaxRetries: intPointer(3), RetryBackoff: time.Millisecond})
	if err != nil {
		t.Fatalf("NewSubQ() error = %v", err)
	}
	_, err = model.Decide(context.Background(), lebro.DecisionRequest{
		State:     "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
	})
	if !errors.Is(err, lebro.ErrDecisionInvalidRequest) {
		t.Fatalf("Decide() error = %v, want an invalid request failure", err)
	}
	if attempts != 1 {
		t.Fatalf("the provider received %d attempts, want 1", attempts)
	}
}

func TestMalformedResponses(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "not json",
			body: `not json at all`,
			want: "decode the decision response",
		},
		{
			name: "no answers",
			body: `{"model":"subq-snap-preview"}`,
			want: "carries no answers",
		},
		{
			name: "missing question",
			body: `{"model":"subq-snap-preview","answers":{"other":{"type":"noul","noul":0.5}}}`,
			want: `returned no answer for question "q"`,
		},
		{
			name: "wrong answer type",
			body: `{"model":"subq-snap-preview","answers":{"q":{"type":"choice","choice":"a"}}}`,
			want: "expected a noul answer",
		},
		{
			name: "missing probability",
			body: `{"model":"subq-snap-preview","answers":{"q":{"type":"noul"}}}`,
			want: "returned no noul probability",
		},
		{
			name: "probability out of range",
			body: `{"model":"subq-snap-preview","answers":{"q":{"type":"noul","noul":1.4}}}`,
			want: "outside the range 0 to 1",
		},
		{
			name: "a confidence the provider does not report",
			body: `{"model":"subq-snap-preview","answers":{"q":{"type":"noul","noul":0.5,"confidence":0.2}}}`,
			want: "returned a confidence the provider does not report",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(testCase.body))
			}))
			defer server.Close()

			model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: server.URL})
			if err != nil {
				t.Fatalf("NewSubQ() error = %v", err)
			}
			_, err = model.Decide(context.Background(), lebro.DecisionRequest{
				State:     "state",
				Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
			})
			if !errors.Is(err, lebro.ErrDecisionMalformedResponse) {
				t.Fatalf("Decide() error = %v, want a malformed response failure", err)
			}
			if !contains(err.Error(), testCase.want) {
				t.Fatalf("Decide() error = %v, want it to mention %q", err, testCase.want)
			}
		})
	}
}

func TestMalformedChoiceAndScoreResponses(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unrequested option",
			body: `{"model":"subq-snap-preview","answers":{"q":{"type":"choice","choice":"other","probabilities":{"other":1,"a":0}}}}`,
			want: "which was not requested",
		},
		{
			name: "missing option probability",
			body: `{"model":"subq-snap-preview","answers":{"q":{"type":"choice","choice":"a","probabilities":{"a":1}}}}`,
			want: `returned no probability for option "b"`,
		},
		{
			name: "level out of range",
			body: `{"model":"subq-snap-preview","answers":{"q":{"type":"score","score":1.0,"probabilities":{"0":0.5,"1":0.5,"2":0}}}}`,
			want: "outside the requested range",
		},
		{
			name: "missing level probability",
			body: `{"model":"subq-snap-preview","answers":{"q":{"type":"score","score":0.5,"probabilities":{"0":1}}}}`,
			want: "returned no probability for level 1",
		},
		{
			name: "confidence out of range",
			body: `{"model":"subq-snap-preview","answers":{"q":{"type":"choice","choice":"a","confidence":1.4,"probabilities":{"a":1,"b":0}}}}`,
			want: "outside the range 0 to 1",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(testCase.body))
			}))
			defer server.Close()

			model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: server.URL})
			if err != nil {
				t.Fatalf("NewSubQ() error = %v", err)
			}
			var question lebro.DecisionQuestion = lebro.ChoiceQuestion{
				Instructions: "Which one?",
				Criteria:     map[lebro.DecisionOptionID]lebro.JSONValue{"a": "first", "b": "second"},
			}
			if testCase.name == "level out of range" || testCase.name == "missing level probability" {
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
			if !contains(err.Error(), testCase.want) {
				t.Fatalf("Decide() error = %v, want it to mention %q", err, testCase.want)
			}
		})
	}
}

func TestCancellationPreservesContextError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(250 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"subq-snap-preview","answers":{"q":{"type":"noul","noul":0.5}}}`))
	}))
	defer server.Close()

	model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewSubQ() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
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
	if !errors.Is(err, lebro.ErrDecisionCancelled) {
		t.Fatalf("Decide() error = %v, want a cancelled decision failure", err)
	}
}

func TestCallerDeadlineIsReportedAsTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"subq-snap-preview","answers":{"q":{"type":"noul","noul":0.5}}}`))
	}))
	defer server.Close()

	model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: server.URL, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("NewSubQ() error = %v", err)
	}
	// The caller sets the deadline, not the adapter.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = model.Decide(ctx, lebro.DecisionRequest{
		State:     "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
	})
	if err == nil {
		t.Fatal("Decide() error = nil, want a timeout failure")
	}
	if !errors.Is(err, lebro.ErrDecisionTimeout) {
		t.Fatalf("Decide() error = %v, want a timeout failure", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Decide() error = %v, want the caller's deadline preserved", err)
	}
}

func TestDeadlineIsReportedAsTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The handler outlives the client deadline on purpose, so the
		// adapter has to give up on its own.
		time.Sleep(250 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"subq-snap-preview","answers":{"q":{"type":"noul","noul":0.5}}}`))
	}))
	defer server.Close()

	model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: server.URL, Timeout: 50 * time.Millisecond, MaxRetries: intPointer(0)})
	if err != nil {
		t.Fatalf("NewSubQ() error = %v", err)
	}
	// The caller has no deadline, so the adapter's own timeout is the only
	// deadline in play.
	_, err = model.Decide(context.Background(), lebro.DecisionRequest{
		State:     "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
	})
	if err == nil {
		t.Fatal("Decide() error = nil, want a timeout failure")
	}
	if !errors.Is(err, lebro.ErrDecisionTimeout) {
		t.Fatalf("Decide() error = %v, want a timeout failure", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Decide() error = %v, want the deadline preserved", err)
	}
}

func TestAdapterConstructorsRequireCredentials(t *testing.T) {
	if _, err := NewTypeSafe(TypeSafeConfig{}); err == nil {
		t.Fatal("NewTypeSafe() error = nil, want a missing credential failure")
	}
	if _, err := NewFastino(FastinoConfig{}); err == nil {
		t.Fatal("NewFastino() error = nil, want a missing credential failure")
	}
	if _, err := NewSubQ(SubQConfig{}); err == nil {
		t.Fatal("NewSubQ() error = nil, want a missing credential failure")
	}
}

func TestAdapterConstructorsRejectRelativeBaseURL(t *testing.T) {
	if _, err := NewTypeSafe(TypeSafeConfig{APIKey: "k", BaseURL: "/v1"}); err == nil {
		t.Fatal("NewTypeSafe() error = nil, want a relative base URL to be rejected")
	}
}

func TestProvidersAdvertiseDistinctIdentities(t *testing.T) {
	typeSafeModel, err := NewTypeSafe(TypeSafeConfig{APIKey: "k"})
	if err != nil {
		t.Fatalf("NewTypeSafe() error = %v", err)
	}
	fastinoModel, err := NewFastino(FastinoConfig{APIKey: "k"})
	if err != nil {
		t.Fatalf("NewFastino() error = %v", err)
	}
	subQModel, err := NewSubQ(SubQConfig{APIKey: "k"})
	if err != nil {
		t.Fatalf("NewSubQ() error = %v", err)
	}
	if typeSafeModel.ProviderID() != "typesafe" {
		t.Fatalf("the TypeSafe provider ID = %q, want %q", typeSafeModel.ProviderID(), "typesafe")
	}
	if fastinoModel.ProviderID() != "fastino" {
		t.Fatalf("the Fastino provider ID = %q, want %q", fastinoModel.ProviderID(), "fastino")
	}
	if subQModel.ProviderID() != "subq" {
		t.Fatalf("the SubQ provider ID = %q, want %q", subQModel.ProviderID(), "subq")
	}
	if !subQModel.Capabilities().Images.Enabled {
		t.Fatal("the SubQ adapter should advertise image support")
	}
	if typeSafeModel.Capabilities().Images.Enabled {
		t.Fatal("the TypeSafe adapter must not advertise image support")
	}
	if fastinoModel.Capabilities().Images.Enabled {
		t.Fatal("the Fastino adapter must not advertise image support")
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

// intPointer returns a pointer to a copy of value.
func intPointer(value int) *int { return &value }
