package channels_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/channels"
)

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
