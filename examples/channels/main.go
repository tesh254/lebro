// channels demonstrates receiving a message from a messaging platform through a
// channel adapter, running an agent, and streaming the reply back to the same
// conversation.
//
// The example is network-free: it signs its own webhook request and drives the
// channel server through httptest with a scripted streaming model, so it runs
// without an API key, a real platform, or an open port. It uses the generic
// HMAC webhook adapter, which needs no platform SDK.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/channels"
)

// scriptedModel streams a reply one word at a time so the channel relay has
// something to deliver incrementally. It stands in for a provider adapter.
type scriptedModel struct{}

func (scriptedModel) Generate(context.Context, lebro.ModelRequest) (lebro.ModelResponse, error) {
	return lebro.ModelResponse{}, fmt.Errorf("scripted model does not support Generate")
}

func (scriptedModel) Stream(context.Context, lebro.ModelRequest) (lebro.StreamReader, error) {
	words := []string{"Hello ", "from ", "the ", "channel ", "adapter."}
	index := 0
	return &lebro.StreamReaderFunc{
		NextFn: func() (lebro.StreamDelta, error) {
			if index >= len(words) {
				return lebro.StreamDelta{}, io.EOF
			}
			delta := lebro.StreamDelta{Text: words[index]}
			index++
			if index == len(words) {
				delta.FinishReason = lebro.FinishReasonStop
			}
			return delta, nil
		},
	}, nil
}

func main() {
	store := lebro.NewMemoryStore()

	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "assistant", Name: "Assistant"},
		Model:      scriptedModel{},
		Store:      store,
	})
	if err != nil {
		log.Fatalf("new agent: %v", err)
	}

	// Stand in for the platform's reply endpoint: the adapter POSTs each reply
	// chunk here, so the example shows the streamed response actually delivered
	// rather than dropped. A real deployment points ReplyURL at the platform.
	var replies []string
	replyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var chunk struct {
			Text  string `json:"text"`
			Final bool   `json:"final"`
		}
		if err := json.NewDecoder(r.Body).Decode(&chunk); err != nil {
			http.Error(w, "bad chunk", http.StatusBadRequest)
			return
		}
		marker := "delta"
		if chunk.Final {
			marker = "final"
		}
		replies = append(replies, fmt.Sprintf("[%s] %q", marker, chunk.Text))
		w.WriteHeader(http.StatusOK)
	}))
	defer replyServer.Close()

	secret := []byte("shared-webhook-secret")
	adapter, err := channels.NewWebhookAdapter(channels.WebhookAdapterConfig{
		Platform: "webhook",
		Secret:   secret,
		ReplyURL: replyServer.URL,
	})
	if err != nil {
		log.Fatalf("new adapter: %v", err)
	}

	server, err := channels.NewServer(channels.Config{
		Store:  store,
		Mapper: channels.NamespaceThreadMapper{Namespace: "demo"},
	})
	if err != nil {
		log.Fatalf("new server: %v", err)
	}
	if err := server.ExposeAgent(agent, adapter); err != nil {
		log.Fatalf("expose agent: %v", err)
	}

	// Build a signed inbound webhook, exactly as a platform would.
	body := `{"message_id":"m-1","conversation_id":"chat-42","user_id":"alice","text":"hi"}`
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte{'.'})
	mac.Write([]byte(body))

	req := httptest.NewRequest(http.MethodPost, "/agents/assistant/channels/webhook/webhook", strings.NewReader(body))
	req.Header.Set("X-Timestamp", timestamp)
	req.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))

	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	fmt.Printf("webhook status: %d\n", rec.Code)
	if rec.Code != http.StatusOK {
		// A non-200 means the webhook was rejected or the run failed; the
		// exchange below would be misleading, so stop here.
		log.Fatalf("webhook returned %d, want 200", rec.Code)
	}

	// The streamed reply was delivered to the reply endpoint chunk by chunk.
	fmt.Printf("delivered reply chunks: %d\n", len(replies))
	for _, reply := range replies {
		fmt.Printf("  %s\n", reply)
	}

	// The reply was persisted to the conversation's thread. Show that a second
	// message on the same conversation continues one durable transcript.
	message := channels.InboundMessage{Conversation: channels.ConversationRef{Platform: "webhook", ID: "chat-42"}}
	threadID, _, _ := channels.NamespaceThreadMapper{Namespace: "demo"}.Map(message)
	page, err := store.Messages().ListMessages(context.Background(), threadID, lebro.PageRequest{})
	if err != nil {
		log.Fatalf("list messages: %v", err)
	}
	fmt.Printf("persisted messages on thread: %d\n", len(page.Records))
	for _, record := range page.Records {
		fmt.Printf("  %s: %s\n", record.Message.Role, record.Message.Content)
	}
}
