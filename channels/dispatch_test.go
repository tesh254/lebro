package channels_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/channels"
)

type acknowledgedAdapter struct{ *controllableAdapter }

func (*acknowledgedAdapter) WebhookAcknowledgement(*http.Request, []byte) ([]byte, string, error) {
	return []byte(`{"type":5}`), "application/json", nil
}

var _ channels.WebhookAcknowledger = (*acknowledgedAdapter)(nil)

func TestServerDispatchAcknowledgesBeforeWorkRuns(t *testing.T) {
	store := lebro.NewMemoryStore()
	agent := newTestAgent(t, "assistant", newScriptedStreamModel("ok"), store)
	adapter := &controllableAdapter{
		platform: "webhook",
		message: channels.InboundMessage{
			Conversation:      channels.ConversationRef{Platform: "webhook", ID: "c1"},
			ProviderMessageID: "m1",
			Sender:            channels.ChannelIdentity{ProviderUserID: "u1"},
			Text:              "hello",
		},
		decodeOK: true,
	}
	accepted := make(chan channels.DispatchJob, 1)
	server, err := channels.NewServer(channels.Config{
		Store: store,
		Dispatch: func(_ context.Context, job channels.DispatchJob) error {
			accepted <- job
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := server.ExposeAgent(agent, adapter); err != nil {
		t.Fatalf("ExposeAgent: %v", err)
	}
	req := httptest.NewRequest("POST", "/agents/assistant/channels/webhook/webhook", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	select {
	case job := <-accepted:
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.RunDispatch(ctx, job); err != nil {
			t.Fatalf("dispatched work: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not receive work")
	}
	if delivered := adapter.delivers(); len(delivered) == 0 || !delivered[len(delivered)-1].Final {
		t.Fatalf("delivered = %#v, want terminal reply", delivered)
	}
}

func TestServerDispatchWritesAdapterAcknowledgement(t *testing.T) {
	agent := newTestAgent(t, "assistant", newScriptedStreamModel("unused"), nil)
	adapter := &acknowledgedAdapter{controllableAdapter: &controllableAdapter{
		platform: "discord",
		message: channels.InboundMessage{
			Conversation: channels.ConversationRef{Platform: "discord", ID: "channel"},
			Sender:       channels.ChannelIdentity{ProviderUserID: "user"},
			Text:         "/ask hello",
		},
		decodeOK: true,
	}}
	server, err := channels.NewServer(channels.Config{Dispatch: func(context.Context, channels.DispatchJob) error { return nil }})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := server.ExposeAgent(agent, adapter); err != nil {
		t.Fatalf("ExposeAgent: %v", err)
	}
	rec := post(server, "/agents/assistant/channels/discord/webhook")
	if rec.Code != 200 || rec.Body.String() != `{"type":5}` || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("response = (%d, %q, %q)", rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"))
	}
}
