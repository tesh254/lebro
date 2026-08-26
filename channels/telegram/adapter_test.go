package telegram_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tesh254/lebro/channels"
	"github.com/tesh254/lebro/channels/telegram"
)

func newAdapter(t *testing.T, configure func(*telegram.Config)) *telegram.Adapter {
	t.Helper()
	config := telegram.Config{SecretToken: "secret", BotToken: "token"}
	if configure != nil {
		configure(&config)
	}
	adapter, err := telegram.New(config)
	if err != nil {
		t.Fatalf("telegram.New: %v", err)
	}
	return adapter
}

func TestVerifyAndDecodeTopicMessage(t *testing.T) {
	adapter := newAdapter(t, nil)
	body := `{"update_id":42,"message":{"message_id":7,"message_thread_id":9,"text":"hello","chat":{"id":-1001},"from":{"id":55,"username":"alice"}}}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "secret")
	verified, err := adapter.Verify(req)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	message, ok, err := adapter.Decode(nil, verified)
	if err != nil || !ok {
		t.Fatalf("Decode = (%+v, %t, %v)", message, ok, err)
	}
	if message.Conversation.ID != "-1001\x009" || message.Conversation.ReplyTarget != "-1001\x009\x007" {
		t.Fatalf("conversation = %+v", message.Conversation)
	}
	if message.ProviderMessageID != "42" || message.Sender.ProviderUserID != "55" {
		t.Fatalf("message = %+v", message)
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	adapter := newAdapter(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{}`))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "wrong")
	if _, err := adapter.Verify(req); err == nil {
		t.Fatal("Verify accepted wrong secret")
	}
}

func TestSendRepliesAndSplits(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		requests = append(requests, string(data))
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	adapter := newAdapter(t, func(c *telegram.Config) { c.APIBaseURL = server.URL })
	err := adapter.Send(context.Background(), channels.ConversationRef{Platform: "telegram", ID: "-1001\x009", ReplyTarget: "-1001\x009\x007"}, channels.OutboundMessage{Text: strings.Repeat("x", 4097), Final: true})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(requests) != 2 || !strings.Contains(requests[0], `"reply_parameters":{"message_id":7}`) || !strings.Contains(requests[0], `"message_thread_id":9`) {
		t.Fatalf("requests = %v", requests)
	}
}
