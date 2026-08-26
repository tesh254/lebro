package discord_test

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tesh254/lebro/channels"
	"github.com/tesh254/lebro/channels/discord"
)

func newAdapter(t *testing.T, configure func(*discord.Config)) (*discord.Adapter, ed25519.PrivateKey) {
	t.Helper()
	privateKey := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	config := discord.Config{PublicKey: hex.EncodeToString(privateKey.Public().(ed25519.PublicKey)), Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
	if configure != nil {
		configure(&config)
	}
	adapter, err := discord.New(config)
	if err != nil {
		t.Fatalf("discord.New: %v", err)
	}
	return adapter, privateKey
}

func signedRequest(t *testing.T, privateKey ed25519.PrivateKey, body string) *http.Request {
	t.Helper()
	timestamp := "1700000000"
	signature := ed25519.Sign(privateKey, []byte(timestamp+body))
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-Signature-Timestamp", timestamp)
	req.Header.Set("X-Signature-Ed25519", hex.EncodeToString(signature))
	return req
}

func TestVerifyAndPing(t *testing.T) {
	adapter, privateKey := newAdapter(t, nil)
	body := `{"type":1}`
	verified, err := adapter.Verify(signedRequest(t, privateKey, body))
	if err != nil || string(verified) != body {
		t.Fatalf("Verify = (%q, %v)", verified, err)
	}
	response, handled, err := adapter.WebhookResponse(nil, verified)
	if err != nil || !handled || string(response) != `{"type":1}` {
		t.Fatalf("WebhookResponse = (%q, %t, %v)", response, handled, err)
	}
}

func TestVerifyRejectsTamperedRequest(t *testing.T) {
	adapter, privateKey := newAdapter(t, nil)
	req := signedRequest(t, privateKey, `{"type":1}`)
	req.Body = io.NopCloser(strings.NewReader(`{"type":2}`))
	if _, err := adapter.Verify(req); err == nil {
		t.Fatal("Verify accepted tampered request")
	}
}

func TestDecodeCommandAndDeferredAcknowledgement(t *testing.T) {
	adapter, _ := newAdapter(t, nil)
	body := []byte(`{
  "id":"I1", "application_id":"A1", "type":2, "token":"token-1", "guild_id":"G1", "channel_id":"C1",
  "member":{"user":{"id":"U1","username":"alice"}},
  "data":{"name":"ask","options":[{"name":"prompt","value":"hello world"}]}
}`)
	message, ok, err := adapter.Decode(nil, body)
	if err != nil || !ok {
		t.Fatalf("Decode = (%+v, %t, %v)", message, ok, err)
	}
	if message.Conversation.ID != "C1" || message.Conversation.ReplyTarget != "A1\x00token-1" {
		t.Fatalf("conversation = %+v", message.Conversation)
	}
	if message.Text != "/ask hello world" || message.ProviderMessageID != "I1" || message.Sender.Tenant != "G1" {
		t.Fatalf("message = %+v", message)
	}
	if _, found := message.Metadata["discord.interaction_token"]; found {
		t.Fatal("interaction token leaked into metadata")
	}
	response, contentType, err := adapter.WebhookAcknowledgement(nil, body)
	if err != nil || string(response) != `{"type":5}` || !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("WebhookAcknowledgement = (%q, %q, %v)", response, contentType, err)
	}
}

func TestSendEditsOriginalAndUsesFollowups(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if !strings.Contains(string(body), `"allowed_mentions":{"parse":[]}`) {
			t.Errorf("allowed mentions = %s", body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	adapter, _ := newAdapter(t, func(c *discord.Config) { c.APIBaseURL = server.URL })
	text := strings.Repeat("x", 2001)
	err := adapter.Send(context.Background(), channels.ConversationRef{Platform: "discord", ID: "C1", ReplyTarget: "A1\x00token-1"}, channels.OutboundMessage{Text: text, Final: true})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got, want := strings.Join(paths, ","), "PATCH /webhooks/A1/token-1/messages/@original,POST /webhooks/A1/token-1"; got != want {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

func TestSendIgnoresStreamDeltas(t *testing.T) {
	adapter, _ := newAdapter(t, nil)
	err := adapter.Send(context.Background(), channels.ConversationRef{Platform: "discord", ID: "C1"}, channels.OutboundMessage{Text: "delta"})
	if err != nil {
		t.Fatalf("Send delta: %v", err)
	}
}
