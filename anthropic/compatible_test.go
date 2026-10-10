package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	claude "github.com/anthropics/anthropic-sdk-go"
	"github.com/tesh254/lebro"
)

const compatibleMessageResponse = `{"id":"msg_compat","type":"message","role":"assistant","model":"glm-4.6","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`

func compatibleRequest() lebro.ModelRequest {
	return lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}}
}

func TestCompatibleEndpointCredentialsAndHeaders(t *testing.T) {
	// Environment credentials and base URL must never leak into a request.
	t.Setenv("ANTHROPIC_API_KEY", "env-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "env-token")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1")

	cases := []struct {
		name          string
		config        Config
		wantAPIKey    string
		wantAuth      string
		wantVersion   string
		wantExtraName string
	}{
		{name: "api key", config: Config{APIKey: "config-key"}, wantAPIKey: "config-key", wantVersion: "2023-06-01"},
		{
			name: "bearer token with headers",
			config: Config{AuthToken: "config-token", Headers: map[string]string{
				"anthropic-version": "2099-01-01",
				"X-Title":           "lebro-test",
			}},
			wantAuth:      "Bearer config-token",
			wantVersion:   "2099-01-01",
			wantExtraName: "lebro-test",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			type captured struct {
				header http.Header
				path   string
			}
			requests := make(chan captured, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- captured{header: r.Header.Clone(), path: r.URL.Path}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(compatibleMessageResponse))
			}))
			defer server.Close()
			config := tc.config
			config.BaseURL = server.URL + "/api"
			config.Model = "glm-4.6"
			model, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := model.Generate(context.Background(), compatibleRequest()); err != nil {
				t.Fatal(err)
			}
			request := <-requests
			got, path := request.header, request.path
			if path != "/api/v1/messages" {
				t.Fatalf("path = %q, want the configured base URL", path)
			}
			if got.Get("X-Api-Key") != tc.wantAPIKey || got.Get("Authorization") != tc.wantAuth {
				t.Fatalf("x-api-key = %q, authorization = %q", got.Get("X-Api-Key"), got.Get("Authorization"))
			}
			if got.Get("Anthropic-Version") != tc.wantVersion {
				t.Fatalf("anthropic-version = %q, want %q", got.Get("Anthropic-Version"), tc.wantVersion)
			}
			if got.Get("X-Title") != tc.wantExtraName {
				t.Fatalf("x-title = %q, want %q", got.Get("X-Title"), tc.wantExtraName)
			}
		})
	}
}

func TestNewRejectsAmbiguousCompatibleConfig(t *testing.T) {
	for name, config := range map[string]Config{
		"no credential":          {},
		"both credentials":       {APIKey: "key", AuthToken: "token"},
		"relative base URL":      {APIKey: "key", BaseURL: "/api"},
		"non-HTTP base URL":      {APIKey: "key", BaseURL: "ftp://example.com"},
		"hostless base URL":      {APIKey: "key", BaseURL: "https:///api"},
		"opaque base URL":        {APIKey: "key", BaseURL: "mailto:api@example.com"},
		"credential header":      {APIKey: "key", Headers: map[string]string{"Authorization": "Bearer other"}},
		"negative timeout":       {APIKey: "key", Timeout: -1},
		"negative retries count": {APIKey: "key", MaxRetries: new(-1)},
	} {
		if _, err := New(config); err == nil {
			t.Fatalf("%s: New() succeeded, want an error", name)
		}
	}
}

func TestCompatibleEndpointIdentityAndPricingDomain(t *testing.T) {
	cases := []struct {
		config       Config
		wantProvider lebro.ProviderID
		wantDomain   lebro.PricingDomain
	}{
		{Config{APIKey: "key"}, "anthropic", lebro.PricingDomainAnthropic},
		{Config{APIKey: "key", BaseURL: "https://api.anthropic.com"}, "anthropic", lebro.PricingDomainAnthropic},
		{Config{AuthToken: "token", BaseURL: "https://openrouter.ai/api"}, "openrouter", lebro.PricingDomainOpenRouter},
		{Config{APIKey: "key", BaseURL: "https://api.z.ai/api/anthropic"}, "anthropic", lebro.PricingDomainAnthropicCompatible},
		{Config{APIKey: "key", BaseURL: "https://api.z.ai/api/anthropic", ProviderID: "zai", PricingDomain: "zai_coding_plan"}, "zai", "zai_coding_plan"},
	}
	for _, tc := range cases {
		model, err := New(tc.config)
		if err != nil {
			t.Fatal(err)
		}
		if model.ProviderID() != tc.wantProvider || model.pricingDomain != tc.wantDomain {
			t.Fatalf("%s: provider = %q, domain = %q", tc.config.BaseURL, model.ProviderID(), model.pricingDomain)
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad"}}`))
	}))
	defer server.Close()
	model, err := New(Config{APIKey: "key", Model: "glm-4.6", BaseURL: server.URL, ProviderID: "zai"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = model.Generate(context.Background(), compatibleRequest())
	var modelErr *lebro.ModelError
	if !errors.As(err, &modelErr) || modelErr.Provider != "zai" || modelErr.Kind != lebro.ModelErrorInvalidRequest {
		t.Fatalf("error = %#v, want an invalid request labelled zai", err)
	}
}

func TestCompatibleEndpointAccountingUsesConfiguredDomain(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(compatibleMessageResponse))
	}))
	defer server.Close()
	model, err := New(Config{APIKey: "key", Model: "glm-4.6", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	response, err := model.Generate(context.Background(), compatibleRequest())
	if err != nil {
		t.Fatal(err)
	}
	if domain := response.Accounting.Domain(); domain != lebro.PricingDomainAnthropicCompatible {
		t.Fatalf("accounting domain = %q, want anthropic_compatible", domain)
	}
}

func TestCompatibleEndpointRetriesAndTimeouts(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	model, err := New(Config{APIKey: "key", Model: "glm-4.6", BaseURL: server.URL, MaxRetries: new(0)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.Generate(context.Background(), compatibleRequest()); err == nil {
		t.Fatal("Generate() succeeded against a failing endpoint")
	}
	if hits.Load() != 1 {
		t.Fatalf("requests = %d, want 1 with retries disabled", hits.Load())
	}

	// A non-streaming request too large for the SDK's default timeout is an
	// invalid request, refused before any network call.
	hits.Store(0)
	large, err := New(Config{APIKey: "key", Model: "glm-4.6", BaseURL: server.URL, MaxTokens: 64000})
	if err != nil {
		t.Fatal(err)
	}
	_, err = large.Generate(context.Background(), compatibleRequest())
	var modelErr *lebro.ModelError
	if !errors.As(err, &modelErr) || modelErr.Kind != lebro.ModelErrorInvalidRequest || hits.Load() != 0 {
		t.Fatalf("large non-streaming error = %v, requests = %d", err, hits.Load())
	}
	// An explicit Timeout lets the caller accept the long request.
	timed, err := New(Config{APIKey: "key", Model: "glm-4.6", BaseURL: server.URL, MaxTokens: 64000, Timeout: 30 * time.Minute, MaxRetries: new(0)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := timed.Generate(context.Background(), compatibleRequest()); err == nil || hits.Load() != 1 {
		t.Fatalf("timed request error = %v, requests = %d, want the endpoint to be called", err, hits.Load())
	}
}

func TestStreamKeepsUnsignedThinkingAsDisplayOnly(t *testing.T) {
	stream := &scriptedAnthropicStream{events: []claude.MessageStreamEventUnion{
		anthropicStreamEvent(t, `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
		anthropicStreamEvent(t, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"unsigned"}}`),
		anthropicStreamEvent(t, `{"type":"content_block_stop","index":0}`),
		anthropicStreamEvent(t, `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
		anthropicStreamEvent(t, `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`),
		anthropicStreamEvent(t, `{"type":"content_block_stop","index":1}`),
		anthropicStreamEvent(t, `{"type":"message_stop"}`),
	}}
	r := &anthropicStream{
		stream:  stream,
		values:  make(chan lebro.StreamDelta, 8),
		done:    make(chan struct{}),
		errorFn: func(_ context.Context, err error) error { return err },
	}
	r.start(context.Background(), lebro.ModelRequest{})

	var reasoning lebro.ModelReasoning
	var text strings.Builder
	for delta := range r.values {
		if delta.Err != nil {
			t.Fatalf("unsigned thinking failed the stream: %v", delta.Err)
		}
		text.WriteString(delta.Text)
		reasoning.Text += delta.Reasoning.Text
		if delta.Reasoning.Details != "" {
			reasoning.Details = delta.Reasoning.Details
		}
	}
	if text.String() != "answer" || reasoning.Text != "unsigned" || reasoning.Details == "" {
		t.Fatalf("text = %q, reasoning = %#v", text.String(), reasoning)
	}

	// Replaying the assistant turn sends only signed thinking back.
	signed := newAnthropicReasoning("", []anthropicReasoningDetail{{Type: "thinking", Thinking: "signed", Signature: "sig"}})
	var details []json.RawMessage
	for _, raw := range []lebro.ModelReasoningDetails{reasoning.Details, signed.Details} {
		var array []json.RawMessage
		if err := json.Unmarshal(raw.Raw(), &array); err != nil {
			t.Fatal(err)
		}
		details = append(details, array...)
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := anthropicReasoningBlocks(lebro.ModelReasoning{Text: "unsigned signed", Details: lebro.NewModelReasoningDetails(encoded)})
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].OfThinking == nil || blocks[0].OfThinking.Signature != "sig" {
		t.Fatalf("replayed blocks = %#v, want only the signed thinking block", blocks)
	}
}
