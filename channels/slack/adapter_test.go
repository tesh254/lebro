package slack_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tesh254/lebro/channels"
	"github.com/tesh254/lebro/channels/slack"
)

const (
	testSecret = "signing-secret"
	testToken  = "xoxb-test-token"
)

func newAdapter(t *testing.T, options ...func(*slack.Config)) *slack.Adapter {
	t.Helper()
	config := slack.Config{
		SigningSecret: testSecret,
		BotToken:      testToken,
		Now:           func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	for _, option := range options {
		option(&config)
	}
	adapter, err := slack.New(config)
	if err != nil {
		t.Fatalf("slack.New: %v", err)
	}
	return adapter
}

func signedRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	timestamp := "1700000000"
	mac := hmac.New(sha256.New, []byte(testSecret))
	_, _ = mac.Write([]byte("v0:" + timestamp + ":" + body))
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", timestamp)
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	return req
}

func TestVerifyAcceptsSignedRawBody(t *testing.T) {
	adapter := newAdapter(t)
	body, err := adapter.Verify(signedRequest(t, `{"type":"event_callback"}`))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got, want := string(body), `{"type":"event_callback"}`; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestVerifyRejectsTamperedBodyAndOldTimestamp(t *testing.T) {
	adapter := newAdapter(t)
	req := signedRequest(t, `{"type":"event_callback","text":"signed"}`)
	req.Body = io.NopCloser(strings.NewReader(`{"type":"event_callback","text":"tampered"}`))
	if _, err := adapter.Verify(req); err == nil {
		t.Fatal("Verify accepted tampered body")
	}

	req = signedRequest(t, `{"type":"event_callback"}`)
	req.Header.Set("X-Slack-Request-Timestamp", "1699999000")
	if _, err := adapter.Verify(req); err == nil {
		t.Fatal("Verify accepted old timestamp")
	}
}

func TestWebhookResponseReturnsVerifiedChallenge(t *testing.T) {
	adapter := newAdapter(t)
	response, handled, err := adapter.WebhookResponse(nil, []byte(`{"type":"url_verification","challenge":"challenge-value"}`))
	if err != nil {
		t.Fatalf("WebhookResponse: %v", err)
	}
	if !handled || string(response) != "challenge-value" {
		t.Fatalf("response = (%q, %t), want (challenge-value, true)", response, handled)
	}
}

func TestDecodeMapsStableThreadAndIgnoresBotMessages(t *testing.T) {
	adapter := newAdapter(t)
	message, ok, err := adapter.Decode(nil, []byte(`{
  "type":"event_callback", "team_id":"T1", "event_id":"Ev1",
  "event":{"type":"message","user":"U1","text":"hello","channel":"C1","ts":"171.001","thread_ts":"170.001"}
}`))
	if err != nil || !ok {
		t.Fatalf("Decode = (%+v, %t, %v), want message", message, ok, err)
	}
	if message.Conversation.Platform != "slack" || message.Conversation.ID != "T1\x00C1\x00170.001" {
		t.Fatalf("conversation = %+v", message.Conversation)
	}
	if message.ProviderMessageID != "Ev1" || message.Sender.ProviderUserID != "U1" || message.Sender.Tenant != "T1" {
		t.Fatalf("identity/message ID = %+v / %q", message.Sender, message.ProviderMessageID)
	}
	if message.Metadata["slack.thread_ts"] != "170.001" {
		t.Fatalf("thread metadata = %q", message.Metadata["slack.thread_ts"])
	}

	_, ok, err = adapter.Decode(nil, []byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev2","event":{"type":"message","bot_id":"B1"}}`))
	if err != nil || ok {
		t.Fatalf("bot Decode = (%t, %v), want (false, nil)", ok, err)
	}
}

func TestSendPostsOnlyFinalThreadReply(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Errorf("Content-Type = %q", got)
		}
		body := readBody(t, r)
		if !strings.Contains(body, `"channel":"C1"`) || !strings.Contains(body, `"thread_ts":"170.001"`) || !strings.Contains(body, `"mrkdwn":false`) {
			t.Errorf("request body = %s", body)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	adapter := newAdapter(t, func(c *slack.Config) { c.PostMessageURL = server.URL })
	conversation := channels.ConversationRef{Platform: "slack", ID: "T1\x00C1\x00170.001"}
	if err := adapter.Send(context.Background(), conversation, channels.OutboundMessage{Text: "chunk"}); err != nil {
		t.Fatalf("Send incremental: %v", err)
	}
	if err := adapter.Send(context.Background(), conversation, channels.OutboundMessage{Text: "done", Final: true, Format: channels.FormatPlain}); err != nil {
		t.Fatalf("Send final: %v", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestSendReturnsSlackAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"not_in_channel"}`))
	}))
	defer server.Close()
	adapter := newAdapter(t, func(c *slack.Config) { c.PostMessageURL = server.URL })
	err := adapter.Send(context.Background(), channels.ConversationRef{Platform: "slack", ID: "T1\x00C1\x00170.001"}, channels.OutboundMessage{Final: true})
	if err == nil || !strings.Contains(err.Error(), "not_in_channel") {
		t.Fatalf("Send error = %v", err)
	}
}

func readBody(t *testing.T, r *http.Request) string {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	return string(body)
}
