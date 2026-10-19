package decisions

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tesh254/lebro"
)

// TestProviderTextIsRedacted pins that a provider which echoes the request
// back in an error cannot put the request content, the image payload, or the
// credential into an error message.
func TestProviderTextIsRedacted(t *testing.T) {
	const credential = "sk-super-secret-credential-value"
	const state = "the customer account 9911-2233-4455 was charged twice"
	image := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x01, 0x02, 0x03}
	encoded := base64.StdEncoding.EncodeToString(image)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"detail":{"error_type":"validation_error","message":"state ` + state + ` with key ` + credential + ` and image ` + encoded + ` is invalid"}}`))
	}))
	defer server.Close()

	model, err := NewSubQ(SubQConfig{APIKey: credential, BaseURL: server.URL, MaxRetries: intPointer(0)})
	if err != nil {
		t.Fatalf("NewSubQ() error = %v", err)
	}
	_, err = model.Decide(context.Background(), lebro.DecisionRequest{
		State:  state,
		Images: []lebro.DecisionImage{{MediaType: "image/png", Bytes: image}},
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"q": lebro.NoulQuestion{Instructions: "Does this request qualify for a refund?"},
		},
	})
	if err == nil {
		t.Fatal("Decide() error = nil, want a provider failure")
	}
	message := err.Error()
	for _, secret := range []string{state, credential, encoded} {
		if strings.Contains(message, secret) {
			t.Fatalf("Decide() error = %v, want %q kept out of the message", err, secret)
		}
	}
	if !strings.Contains(message, "[redacted]") {
		t.Fatalf("Decide() error = %v, want the request content marked as redacted", err)
	}
}

// TestRedirectsAreRefused pins that the adapter does not follow a redirect,
// because a redirected request would carry the credential to another host.
func TestRedirectsAreRefused(t *testing.T) {
	target := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/systemone" {
			// The redirect target is the same server, so the counter below
			// observes a followed redirect instead of an unreachable host.
			http.Redirect(w, r, server.URL+"/elsewhere", http.StatusTemporaryRedirect)
			return
		}
		target++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"subq-snap-preview","answers":{"q":{"type":"noul","noul":0.5}}}`))
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
		t.Fatal("Decide() error = nil, want a redirect refusal")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("Decide() error = %v, want a redirect refusal", err)
	}
	if target != 0 {
		t.Fatalf("the redirect target received %d requests, want none", target)
	}
}

// TestLongRetryAfterIsNotWaitedOut pins that a provider delay longer than
// the adapter timeout is not waited out: the failure comes back with the
// delay so the caller can schedule the retry.
func TestLongRetryAfterIsNotWaitedOut(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Retry-After", "3600")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"detail":{"error_type":"rate_limit","message":"slow down"}}`))
	}))
	defer server.Close()

	model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: server.URL, Timeout: time.Second, MaxRetries: intPointer(3)})
	if err != nil {
		t.Fatalf("NewSubQ() error = %v", err)
	}
	started := time.Now()
	_, err = model.Decide(context.Background(), lebro.DecisionRequest{
		State:     "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
	})
	if err == nil {
		t.Fatal("Decide() error = nil, want a rate limited failure")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("Decide() waited %v, want it to return without the hour-long delay", elapsed)
	}
	if attempts != 1 {
		t.Fatalf("the provider received %d attempts, want 1", attempts)
	}
	var decisionErr *lebro.DecisionError
	if !errors.As(err, &decisionErr) {
		t.Fatalf("Decide() error = %v, want a *lebro.DecisionError", err)
	}
	if decisionErr.RetryAfter != time.Hour {
		t.Fatalf("Decide() retry after = %v, want 1h", decisionErr.RetryAfter)
	}
}

// TestMalformedProviderFields pins the response fields the contract requires.
func TestMalformedProviderFields(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "no model identity",
			body: `{"answers":{"q":{"type":"noul","noul":0.5}}}`,
			want: "no model identity",
		},
		{
			name: "negative input tokens",
			body: `{"model":"subq-snap-preview","answers":{"q":{"type":"noul","noul":0.5}},"usage":{"input_tokens":-1}}`,
			want: "negative input token count",
		},
		{
			name: "negative output tokens",
			body: `{"model":"subq-snap-preview","answers":{"q":{"type":"noul","noul":0.5}},"usage":{"output_tokens":-5}}`,
			want: "negative output token count",
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
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Decide() error = %v, want it to mention %q", err, testCase.want)
			}
		})
	}
}

// TestScoreRequiresTheProviderFields pins that a score answer without the
// fields the provider is required to report is a malformed response.
func TestScoreRequiresTheProviderFields(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "weighted provider without a score",
			body: `{"model":"subq-snap-preview","answers":{"q":{"type":"score","probabilities":{"0":0.5,"1":0.5}}}}`,
			want: `returned no "score" score field`,
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
				State: "state",
				Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
					"q": lebro.ScoreQuestion{Instructions: "How much?", Criteria: []lebro.JSONValue{"low", "high"}},
				},
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

// TestFastinoRequiresTheWinningLevel pins the same rule for the provider
// that reports a winning level and a weighted value.
func TestFastinoRequiresTheWinningLevel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"glide","answers":{"q":{"type":"score","expected_level":1.5,"probabilities":{"0":0.5,"1":0.5}}}}`))
	}))
	defer server.Close()

	model, err := NewFastino(FastinoConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewFastino() error = %v", err)
	}
	_, err = model.Decide(context.Background(), lebro.DecisionRequest{
		State: "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"q": lebro.ScoreQuestion{Instructions: "How much?", Criteria: []lebro.JSONValue{"low", "high"}},
		},
	})
	if !errors.Is(err, lebro.ErrDecisionMalformedResponse) {
		t.Fatalf("Decide() error = %v, want a malformed response failure", err)
	}
	if !strings.Contains(err.Error(), `no "score" winning level`) {
		t.Fatalf("Decide() error = %v, want it to mention the missing level", err)
	}
}

// TestPointerQuestionsAreAccepted pins that the pointer forms of the closed
// question set work like the value forms.
func TestPointerQuestionsAreAccepted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"subq-snap-preview","answers":{"q":{"type":"noul","noul":0.5}}}`))
	}))
	defer server.Close()

	model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewSubQ() error = %v", err)
	}
	question := lebro.NoulQuestion{Instructions: "Is it?"}
	response, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State:     "state",
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": &question},
	})
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	if _, ok := response.Answers["q"].(lebro.NoulAnswer); !ok {
		t.Fatalf("Decide() answer = %#v, want a noul answer", response.Answers["q"])
	}
}

// TestConfiguredTimeoutAppliesToACallerClient pins that a caller-supplied
// HTTP client with no timeout of its own still gets the adapter's configured
// deadline.
func TestConfiguredTimeoutAppliesToACallerClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"subq-snap-preview","answers":{"q":{"type":"noul","noul":0.5}}}`))
	}))
	defer server.Close()

	model, err := NewSubQ(SubQConfig{
		APIKey:     "test-key",
		BaseURL:    server.URL,
		HTTPClient: &http.Client{},
		Timeout:    50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewSubQ() error = %v", err)
	}
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
}

// TestDefaultTimeoutAppliesToACallerClient pins that a caller-supplied HTTP
// client with no timeout of its own still gets the adapter's default
// deadline rather than hanging until the caller's context ends.
func TestDefaultTimeoutAppliesToACallerClient(t *testing.T) {
	if defaultTimeout != 60*time.Second {
		t.Fatalf("the default timeout = %v, want 60s", defaultTimeout)
	}
	client, err := newDecisionClient(
		subQProvider,
		"https://api.subq.ai",
		systemOnePath,
		"Authorization",
		"Bearer ",
		"test-key",
		subQModel,
		httpClientConfig{client: &http.Client{}},
	)
	if err != nil {
		t.Fatalf("newDecisionClient() error = %v", err)
	}
	if client.timeout != defaultTimeout {
		t.Fatalf("the adapter timeout = %v, want the default %v", client.timeout, defaultTimeout)
	}
}
