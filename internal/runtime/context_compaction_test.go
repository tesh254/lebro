package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

type memorySummaryStore struct {
	mu   sync.Mutex
	item ContextSummary
}

func (s *memorySummaryStore) LoadContextSummary(_ context.Context, threadID ThreadID) (ContextSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.item.ThreadID != threadID {
		return ContextSummary{ThreadID: threadID}, nil
	}
	return s.item, nil
}

func (s *memorySummaryStore) CompareAndSwapContextSummary(_ context.Context, next ContextSummary, expected int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.item.Version != expected {
		return false, nil
	}
	next.Version = expected + 1
	s.item = next
	return true, nil
}

func TestContextCompactionSummarizesDurablePrefixWithoutChangingTranscript(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Threads().CreateThread(ctx, ThreadRecord{ID: "compact-thread", CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	records := make([]MessageRecord, 0, 6)
	for i := 0; i < 6; i++ {
		records = append(records, MessageRecord{ID: "old-" + string(rune('a'+i)), ThreadID: "compact-thread", Message: Message{Role: RoleUser, Content: strings.Repeat("x", 220)}, CreatedAt: time.Now()})
	}
	if err := store.Messages().AppendMessages(ctx, records); err != nil {
		t.Fatal(err)
	}
	model := newScriptedModel(textResponse("rolling summary"), textResponse("answer"))
	summaries := &memorySummaryStore{}
	agent, err := NewAgent(AgentConfig{Definition: AgentDefinition{ID: "compact-agent", Instructions: "help"}, Model: model, Store: store, ContextCompaction: &ContextCompactionConfig{Store: summaries, Budget: ContextBudget{ContextWindowTokens: 3000, OutputReserveTokens: 64, FutureToolReserveTokens: 64, TriggerFraction: .2, TargetFraction: .1, SummaryMaxOutputTokens: 32}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(ctx, RunInput{ThreadID: "compact-thread", Messages: []Message{{Role: RoleUser, Content: "new request"}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %s", result.Status)
	}
	if summaries.item.Version != 1 || summaries.item.CoveredThroughMessageID == "" {
		t.Fatalf("summary = %#v", summaries.item)
	}
	if len(model.calls) != 2 || model.calls[0].MaxOutputTokens != 32 {
		t.Fatalf("calls = %#v", model.calls)
	}
	if model.calls[1].MaxOutputTokens != 64 {
		t.Fatalf("normal output bound = %d", model.calls[1].MaxOutputTokens)
	}
	found := false
	for _, message := range model.calls[1].Messages {
		if message.Content == summaryMessage("rolling summary").Content {
			found = true
		}
	}
	if !found {
		t.Fatal("compacted request omitted summary")
	}
	page, err := store.Messages().ListMessages(ctx, "compact-thread", PageRequest{})
	if err != nil || len(page.Records) != 8 {
		t.Fatalf("canonical transcript changed: %d %v", len(page.Records), err)
	}
	for i, want := range records {
		got := page.Records[i]
		if got.ID != want.ID || got.Message != want.Message {
			t.Fatalf("canonical record %d changed: got %#v want %#v", i, got, want)
		}
	}
}

func TestContextCompactionNeverSplitsToolInteraction(t *testing.T) {
	records := []MessageRecord{{ID: "a", Message: Message{Role: RoleAssistant, ToolCalls: mustToolCalls(t, ModelToolCall{ID: "call", ToolID: "tool", Arguments: []byte(`{}`)})}}, {ID: "b", Message: Message{Role: RoleTool, ToolCallID: "call", Content: "done"}}}
	if !safeCompactionBoundary(records) {
		t.Fatal("completed tool pair rejected")
	}
	if safeCompactionBoundary(records[:1]) {
		t.Fatal("tool request split accepted")
	}
}

func mustToolCalls(t *testing.T, calls ...ModelToolCall) ModelToolCalls {
	t.Helper()
	value, err := NewModelToolCalls(calls...)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
