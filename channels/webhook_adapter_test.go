package channels_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tesh254/lebro/channels"
)

// signedRequest builds a POST carrying a body signed with secret over
// timestamp + "." + body, matching the adapter's scheme.
func signedRequest(t *testing.T, secret []byte, ts int64, body string) *http.Request {
	t.Helper()
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte{'.'})
	mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
	return req
}

func newFixedWebhookAdapter(t *testing.T, secret []byte, now time.Time) *channels.WebhookAdapter {
	t.Helper()
	adapter, err := channels.NewWebhookAdapter(channels.WebhookAdapterConfig{
		Platform: "webhook",
		Secret:   secret,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewWebhookAdapter: %v", err)
	}
	return adapter
}

func TestWebhookAdapterVerifyAcceptsValidSignature(t *testing.T) {
	secret := []byte("shhh")
	now := time.Unix(1_700_000_000, 0)
	adapter := newFixedWebhookAdapter(t, secret, now)

	body := `{"conversation_id":"c1","text":"hi"}`
	req := signedRequest(t, secret, now.Unix(), body)

	got, err := adapter.Verify(req)
	if err != nil {
		t.Fatalf("Verify rejected a valid request: %v", err)
	}
	if string(got) != body {
		t.Fatalf("Verify returned body %q, want %q", got, body)
	}
}

func TestWebhookAdapterVerifyRejectsTamperedBody(t *testing.T) {
	secret := []byte("shhh")
	now := time.Unix(1_700_000_000, 0)
	adapter := newFixedWebhookAdapter(t, secret, now)

	req := signedRequest(t, secret, now.Unix(), `{"conversation_id":"c1","text":"hi"}`)
	// Replace the body after signing so the signature no longer matches.
	req.Body = httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{"conversation_id":"c1","text":"tampered"}`)).Body

	if _, err := adapter.Verify(req); err == nil {
		t.Fatal("Verify accepted a tampered body")
	}
}

func TestWebhookAdapterVerifyRejectsWrongSecret(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	adapter := newFixedWebhookAdapter(t, []byte("right"), now)
	req := signedRequest(t, []byte("wrong"), now.Unix(), `{"conversation_id":"c1","text":"hi"}`)
	if _, err := adapter.Verify(req); err == nil {
		t.Fatal("Verify accepted a signature made with the wrong secret")
	}
}

func TestWebhookAdapterVerifyRejectsStaleTimestamp(t *testing.T) {
	secret := []byte("shhh")
	now := time.Unix(1_700_000_000, 0)
	adapter := newFixedWebhookAdapter(t, secret, now)

	// Timestamp is well outside the default skew window.
	stale := now.Add(-2 * channels.DefaultWebhookMaxSkew).Unix()
	req := signedRequest(t, secret, stale, `{"conversation_id":"c1","text":"hi"}`)
	if _, err := adapter.Verify(req); err == nil {
		t.Fatal("Verify accepted a stale timestamp")
	}
}

func TestWebhookAdapterVerifyAllowsDisabledSkew(t *testing.T) {
	secret := []byte("shhh")
	now := time.Unix(1_700_000_000, 0)
	adapter, err := channels.NewWebhookAdapter(channels.WebhookAdapterConfig{
		Platform: "webhook",
		Secret:   secret,
		MaxSkew:  -1, // disable the timestamp check
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewWebhookAdapter: %v", err)
	}

	// A timestamp far outside any window is accepted because the check is off,
	// provided the signature still matches.
	stale := now.Add(-100 * time.Hour).Unix()
	req := signedRequest(t, secret, stale, `{"conversation_id":"c1","text":"hi"}`)
	if _, err := adapter.Verify(req); err != nil {
		t.Fatalf("Verify rejected a valid request with skew disabled: %v", err)
	}
}

func TestWebhookAdapterDecode(t *testing.T) {
	adapter := newFixedWebhookAdapter(t, []byte("shhh"), time.Unix(1, 0))
	body := []byte(`{"message_id":"m1","conversation_id":"c1","user_id":"u1","user_name":"Ann","text":"hi"}`)
	message, ok, err := adapter.Decode(nil, body)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !ok {
		t.Fatal("Decode reported no message for a real payload")
	}
	if message.Conversation.ID != "c1" || message.Conversation.Platform != "webhook" {
		t.Fatalf("conversation = %+v", message.Conversation)
	}
	if message.ProviderMessageID != "m1" || message.Sender.ProviderUserID != "u1" || message.Text != "hi" {
		t.Fatalf("decoded message = %+v", message)
	}
}

func TestWebhookAdapterDecodeRejectsTrailingContent(t *testing.T) {
	adapter := newFixedWebhookAdapter(t, []byte("shhh"), time.Unix(1, 0))
	body := []byte(`{"conversation_id":"c1","text":"hi"}{"conversation_id":"c2","text":"evil"}`)
	if _, _, err := adapter.Decode(nil, body); err == nil {
		t.Fatal("Decode accepted a body with a second JSON value after the first")
	}
}

func TestWebhookAdapterDecodeTreatsEmptyAsNonMessage(t *testing.T) {
	adapter := newFixedWebhookAdapter(t, []byte("shhh"), time.Unix(1, 0))
	if _, ok, err := adapter.Decode(nil, []byte(`{}`)); err != nil || ok {
		t.Fatalf("empty payload: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}

func TestNewWebhookAdapterValidates(t *testing.T) {
	if _, err := channels.NewWebhookAdapter(channels.WebhookAdapterConfig{Secret: []byte("s")}); err == nil {
		t.Fatal("missing platform accepted")
	}
	if _, err := channels.NewWebhookAdapter(channels.WebhookAdapterConfig{Platform: "p"}); err == nil {
		t.Fatal("missing secret accepted")
	}
}
