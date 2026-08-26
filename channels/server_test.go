package channels_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/channels"
)

// errContext is a fixed error a fake adapter returns from Send to exercise the
// delivery-failure path.
var errContext = errors.New("delivery failed")

// controllableAdapter is an in-memory adapter for handler tests. It verifies
// nothing, decodes a caller-supplied message, and records every chunk Send
// delivers, so a test can assert on the streamed reply without a real platform.
type controllableAdapter struct {
	platform string
	message  channels.InboundMessage
	decodeOK bool

	mu            sync.Mutex
	delivered     []channels.OutboundMessage
	sendErr       error
	failFinalOnly bool
}

type challengeAdapter struct {
	controllableAdapter
	challenge []byte
}

func (a *challengeAdapter) WebhookResponse(*http.Request, []byte) ([]byte, bool, error) {
	return a.challenge, true, nil
}

func (a *controllableAdapter) Platform() string { return a.platform }

func (a *controllableAdapter) Verify(*http.Request) ([]byte, error) { return nil, nil }

func (a *controllableAdapter) Decode(*http.Request, []byte) (channels.InboundMessage, bool, error) {
	return a.message, a.decodeOK, nil
}

func (a *controllableAdapter) Send(_ context.Context, _ channels.ConversationRef, message channels.OutboundMessage) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sendErr != nil && (!a.failFinalOnly || message.Final) {
		return a.sendErr
	}
	a.delivered = append(a.delivered, message)
	return nil
}

func (a *controllableAdapter) delivers() []channels.OutboundMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]channels.OutboundMessage(nil), a.delivered...)
}

var _ channels.Adapter = (*controllableAdapter)(nil)
var _ channels.WebhookResponder = (*challengeAdapter)(nil)

func newServer(t *testing.T, store lebro.Store, mapper channels.ThreadMapper, dedup channels.Deduplicator) *channels.Server {
	t.Helper()
	server, err := channels.NewServer(channels.Config{Store: store, Mapper: mapper, Deduplicator: dedup})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return server
}

func post(server *channels.Server, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	return rec
}

func TestServerStreamsReplyToAdapter(t *testing.T) {
	store := lebro.NewMemoryStore()
	model := newScriptedStreamModel("Hello", ", ", "world")
	agent := newTestAgent(t, "assistant", model, store)

	adapter := &controllableAdapter{
		platform: "webhook",
		message: channels.InboundMessage{
			Conversation:      channels.ConversationRef{Platform: "webhook", ID: "c1"},
			ProviderMessageID: "m1",
			Sender:            channels.ChannelIdentity{ProviderUserID: "u1"},
			Text:              "hi",
		},
		decodeOK: true,
	}

	server := newServer(t, store, channels.NamespaceThreadMapper{Namespace: "prod"}, channels.NewMemoryDeduplicator(16))
	if err := server.ExposeAgent(agent, adapter); err != nil {
		t.Fatalf("ExposeAgent: %v", err)
	}

	rec := post(server, "/agents/assistant/channels/webhook/webhook")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	delivered := adapter.delivers()
	if len(delivered) == 0 {
		t.Fatal("no reply chunks delivered")
	}
	final := delivered[len(delivered)-1]
	if !final.Final {
		t.Fatal("last chunk is not marked final")
	}
	if final.Text != "Hello, world" {
		t.Fatalf("final reply = %q, want %q", final.Text, "Hello, world")
	}

	// Incremental chunks carried the streamed text in order.
	var incremental strings.Builder
	for _, chunk := range delivered[:len(delivered)-1] {
		if chunk.Final {
			t.Fatal("a non-terminal chunk was marked final")
		}
		incremental.WriteString(chunk.Text)
	}
	if incremental.String() != "Hello, world" {
		t.Fatalf("incremental text = %q, want %q", incremental.String(), "Hello, world")
	}
}

func TestServerWritesVerifiedWebhookChallenge(t *testing.T) {
	server := newServer(t, nil, channels.NamespaceThreadMapper{}, channels.NewMemoryDeduplicator(4))
	agent := newTestAgent(t, "assistant", newScriptedStreamModel("unused"), nil)
	adapter := &challengeAdapter{controllableAdapter: controllableAdapter{platform: "webhook"}, challenge: []byte("challenge-value")}
	if err := server.ExposeAgent(agent, adapter); err != nil {
		t.Fatalf("ExposeAgent: %v", err)
	}
	rec := post(server, "/agents/assistant/channels/webhook/webhook")
	if rec.Code != http.StatusOK || rec.Body.String() != "challenge-value" {
		t.Fatalf("response = (%d, %q), want (200, challenge-value)", rec.Code, rec.Body.String())
	}
}

func TestServerPersistsThreadWithScoping(t *testing.T) {
	store := lebro.NewMemoryStore()
	agent := newTestAgent(t, "assistant", newScriptedStreamModel("ok"), store)
	mapper := channels.NamespaceThreadMapper{Namespace: "prod"}
	adapter := &controllableAdapter{
		platform: "webhook",
		message: channels.InboundMessage{
			Conversation:      channels.ConversationRef{Platform: "webhook", ID: "c1"},
			ProviderMessageID: "m1",
			Sender:            channels.ChannelIdentity{ProviderUserID: "u1"},
			Text:              "hi",
		},
		decodeOK: true,
	}
	server := newServer(t, store, mapper, channels.NewMemoryDeduplicator(16))
	if err := server.ExposeAgent(agent, adapter); err != nil {
		t.Fatalf("ExposeAgent: %v", err)
	}

	if rec := post(server, "/agents/assistant/channels/webhook/webhook"); rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	threadID, ns, owner := mapper.Map(adapter.message)
	if ns != "prod" || owner != "u1" {
		t.Fatalf("mapper scoping = (%q,%q)", ns, owner)
	}
	record, err := store.Threads().GetThread(context.Background(), threadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if record.Namespace != "prod" || record.OwnerID != "u1" {
		t.Fatalf("thread scoping = (%q,%q), want (prod,u1)", record.Namespace, record.OwnerID)
	}
	page, err := store.Messages().ListMessages(context.Background(), threadID, lebro.PageRequest{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(page.Records) == 0 {
		t.Fatal("no messages persisted for the conversation")
	}
}

func TestServerDropsDuplicateDelivery(t *testing.T) {
	store := lebro.NewMemoryStore()
	model := newScriptedStreamModel("ok")
	agent := newTestAgent(t, "assistant", model, store)
	adapter := &controllableAdapter{
		platform: "webhook",
		message: channels.InboundMessage{
			Conversation:      channels.ConversationRef{Platform: "webhook", ID: "c1"},
			ProviderMessageID: "dup-1",
			Sender:            channels.ChannelIdentity{ProviderUserID: "u1"},
			Text:              "hi",
		},
		decodeOK: true,
	}
	server := newServer(t, store, channels.NamespaceThreadMapper{}, channels.NewMemoryDeduplicator(16))
	if err := server.ExposeAgent(agent, adapter); err != nil {
		t.Fatalf("ExposeAgent: %v", err)
	}

	if rec := post(server, "/agents/assistant/channels/webhook/webhook"); rec.Code != 200 {
		t.Fatalf("first status = %d", rec.Code)
	}
	if rec := post(server, "/agents/assistant/channels/webhook/webhook"); rec.Code != 200 {
		t.Fatalf("redelivery status = %d", rec.Code)
	}

	if calls := model.streamCalls(); calls != 1 {
		t.Fatalf("model ran %d times, want 1 (redelivery must be dropped)", calls)
	}
}

func TestServerRejectsUnknownRoute(t *testing.T) {
	server := newServer(t, nil, channels.NamespaceThreadMapper{}, channels.NewMemoryDeduplicator(4))
	if rec := post(server, "/agents/nobody/channels/webhook/webhook"); rec.Code == 200 {
		t.Fatalf("unknown route returned 200")
	}
}

func TestServerReturns500OnDeliveryFailure(t *testing.T) {
	store := lebro.NewMemoryStore()
	agent := newTestAgent(t, "assistant", newScriptedStreamModel("ok"), store)
	adapter := &controllableAdapter{
		platform: "webhook",
		message: channels.InboundMessage{
			Conversation:      channels.ConversationRef{Platform: "webhook", ID: "c1"},
			ProviderMessageID: "m1",
			Sender:            channels.ChannelIdentity{ProviderUserID: "u1"},
			Text:              "hi",
		},
		decodeOK: true,
		// Fail only the final chunk so the run itself succeeds; this isolates
		// the delivery-failure path from a run error and pins that a failed
		// terminal delivery alone yields 500.
		sendErr:       errContext,
		failFinalOnly: true,
	}
	server := newServer(t, store, channels.NamespaceThreadMapper{}, channels.NewMemoryDeduplicator(16))
	if err := server.ExposeAgent(agent, adapter); err != nil {
		t.Fatalf("ExposeAgent: %v", err)
	}
	if rec := post(server, "/agents/assistant/channels/webhook/webhook"); rec.Code != 500 {
		t.Fatalf("status = %d, want 500 on delivery failure", rec.Code)
	}
}

func TestServerExposeRejectsMetacharAgentID(t *testing.T) {
	// An agent ID carrying ServeMux pattern syntax must be refused so it cannot
	// register a wildcard route that captures other agents' paths.
	agent := newTestAgent(t, "{id}", newScriptedStreamModel("ok"), nil)
	server := newServer(t, nil, channels.NamespaceThreadMapper{}, channels.NewMemoryDeduplicator(4))
	if err := server.ExposeAgent(agent, &controllableAdapter{platform: "webhook", decodeOK: true}); err == nil {
		t.Fatal("agent ID with ServeMux metacharacters was accepted")
	}
}

func TestServerExposeRejectsMetacharPlatform(t *testing.T) {
	agent := newTestAgent(t, "assistant", newScriptedStreamModel("ok"), nil)
	server := newServer(t, nil, channels.NamespaceThreadMapper{}, channels.NewMemoryDeduplicator(4))
	if err := server.ExposeAgent(agent, &controllableAdapter{platform: "a/b", decodeOK: true}); err == nil {
		t.Fatal("platform containing a slash was accepted")
	}
}

func TestServerExposeRejectsTypedNilAdapter(t *testing.T) {
	agent := newTestAgent(t, "assistant", newScriptedStreamModel("ok"), nil)
	server := newServer(t, nil, channels.NamespaceThreadMapper{}, channels.NewMemoryDeduplicator(4))
	// A typed-nil adapter is non-nil as an interface; registration must reject
	// it rather than panic calling Platform().
	var typedNil *controllableAdapter
	if err := server.ExposeAgent(agent, typedNil); err == nil {
		t.Fatal("typed-nil adapter was accepted")
	}
}

func TestServerScopesDedupPerRoute(t *testing.T) {
	// One shared deduplicator across two agents. The same provider message ID on
	// each agent's route must not be conflated: the second agent's message must
	// still run.
	store := lebro.NewMemoryStore()
	dedup := channels.NewMemoryDeduplicator(64)

	modelA := newScriptedStreamModel("a")
	modelB := newScriptedStreamModel("b")
	agentA := newTestAgent(t, "agent-a", modelA, store)
	agentB := newTestAgent(t, "agent-b", modelB, store)

	// Same provider message ID on both routes, but distinct conversations so
	// each agent persists to its own thread; the dedup key is scoped by agent
	// and platform, not by conversation, so this still exercises route scoping.
	msg := func(conversation string) channels.InboundMessage {
		return channels.InboundMessage{
			Conversation:      channels.ConversationRef{Platform: "webhook", ID: conversation},
			ProviderMessageID: "shared-id",
			Sender:            channels.ChannelIdentity{ProviderUserID: "u1"},
			Text:              "hi",
		}
	}

	server := newServer(t, store, channels.NamespaceThreadMapper{}, dedup)
	if err := server.ExposeAgent(agentA, &controllableAdapter{platform: "webhook", message: msg("c-a"), decodeOK: true}); err != nil {
		t.Fatalf("expose agent-a: %v", err)
	}
	if err := server.ExposeAgent(agentB, &controllableAdapter{platform: "webhook", message: msg("c-b"), decodeOK: true}); err != nil {
		t.Fatalf("expose agent-b: %v", err)
	}

	if rec := post(server, "/agents/agent-a/channels/webhook/webhook"); rec.Code != 200 {
		t.Fatalf("agent-a status = %d", rec.Code)
	}
	if rec := post(server, "/agents/agent-b/channels/webhook/webhook"); rec.Code != 200 {
		t.Fatalf("agent-b status = %d", rec.Code)
	}
	if modelA.streamCalls() != 1 {
		t.Fatalf("agent-a ran %d times, want 1", modelA.streamCalls())
	}
	if modelB.streamCalls() != 1 {
		t.Fatalf("agent-b ran %d times, want 1 (shared ID must not conflate routes)", modelB.streamCalls())
	}
}

func TestServerExposeRejectsDuplicatePlatform(t *testing.T) {
	agent := newTestAgent(t, "assistant", newScriptedStreamModel("ok"), nil)
	server := newServer(t, nil, channels.NamespaceThreadMapper{}, channels.NewMemoryDeduplicator(4))
	a1 := &controllableAdapter{platform: "webhook", decodeOK: true}
	a2 := &controllableAdapter{platform: "webhook", decodeOK: true}
	if err := server.ExposeAgent(agent, a1, a2); err == nil {
		t.Fatal("exposing two adapters with the same platform succeeded")
	}
}
