package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type memoryExtractorFunc func(context.Context, MemoryExtractionRequest) ([]MemoryFactProposal, error)

func (f memoryExtractorFunc) ExtractMemoryFacts(ctx context.Context, request MemoryExtractionRequest) ([]MemoryFactProposal, error) {
	return f(ctx, request)
}

func TestMemoryProcessorRecallsFilteredBudgetedFacts(t *testing.T) {
	store := NewMemoryStore()
	scope := WorkingMemoryScope{Namespace: "tenant", OwnerID: "user"}
	for _, fact := range []WorkingMemoryFact{
		{ID: "1", Namespace: scope.Namespace, OwnerID: scope.OwnerID, Key: "keep", Value: []byte(`"Ada"`)},
		{ID: "2", Namespace: scope.Namespace, OwnerID: scope.OwnerID, Key: "skip", Value: []byte(`"no"`)},
	} {
		if _, err := store.WorkingMemory().UpsertWorkingMemoryFact(context.Background(), fact, 0); err != nil {
			t.Fatal(err)
		}
	}
	processor, err := NewMemoryProcessor(store, &MemoryProcessorConfig{Scope: scope, Recall: MemoryRecallConfig{MaxFacts: 1, Filter: func(_ context.Context, fact WorkingMemoryFact) (bool, error) { return fact.Key == "keep", nil }}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := processor.ProcessInput(context.Background(), ProcessorInputRequest{Input: RunInput{Messages: []Message{{Role: RoleUser, Content: "hello"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision.Kind != ProcessorTransform || len(result.Input.Messages) != 2 {
		t.Fatalf("result = %#v", result)
	}
	if !strings.Contains(result.Input.Messages[0].Content, "keep: \"Ada\"") || strings.Contains(result.Input.Messages[0].Content, "skip") {
		t.Fatalf("injected = %q", result.Input.Messages[0].Content)
	}
}

func TestMemoryProcessorRequiresApprovalAndAuditsWrites(t *testing.T) {
	store := NewMemoryStore()
	scope := WorkingMemoryScope{Namespace: "tenant", OwnerID: "user"}
	proposals := memoryExtractorFunc(func(context.Context, MemoryExtractionRequest) ([]MemoryFactProposal, error) {
		return []MemoryFactProposal{{Key: "name", Value: []byte(`"Ada"`)}}, nil
	})
	var events []MemoryAuditEvent
	processor, err := NewMemoryProcessor(store, &MemoryProcessorConfig{Scope: scope, Extractor: proposals, Audit: func(_ context.Context, event MemoryAuditEvent) error { events = append(events, event); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	request := ProcessorOutputRequest{Run: ProcessorRun{ID: "run"}, Result: RunResult{Status: RunStatusSucceeded}}
	if _, err := processor.ProcessOutput(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := store.WorkingMemory().GetWorkingMemoryFact(context.Background(), scope, "name"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unapproved fact = %v", err)
	}
	if len(events) != 1 || events[0].Approved {
		t.Fatalf("audit = %#v", events)
	}

	processor.config.Approval = func(context.Context, MemoryWriteRequest) (bool, error) { return true, nil }
	if _, err := processor.ProcessOutput(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	fact, err := store.WorkingMemory().GetWorkingMemoryFact(context.Background(), scope, "name")
	if err != nil || fact.Version != 1 || string(fact.Value) != `"Ada"` {
		t.Fatalf("fact = %#v, %v", fact, err)
	}
	if len(events) != 2 || !events[1].Approved || events[1].Fact == nil {
		t.Fatalf("audit = %#v", events)
	}
}

func TestAgentMemoryProcessorInjectsAndWritesAfterSuccess(t *testing.T) {
	store := NewMemoryStore()
	scope := WorkingMemoryScope{Namespace: "tenant", OwnerID: "user"}
	_, err := store.WorkingMemory().UpsertWorkingMemoryFact(context.Background(), WorkingMemoryFact{ID: "before", Namespace: scope.Namespace, OwnerID: scope.OwnerID, Key: "locale", Value: []byte(`"sw"`), CreatedAt: time.Now(), UpdatedAt: time.Now()}, 0)
	if err != nil {
		t.Fatal(err)
	}
	model := newScriptedModel(textResponse("done"))
	agent, err := NewAgent(AgentConfig{Definition: AgentDefinition{ID: "memory"}, Model: model, Store: store, Memory: &MemoryProcessorConfig{Scope: scope, Extractor: memoryExtractorFunc(func(context.Context, MemoryExtractionRequest) ([]MemoryFactProposal, error) {
		return []MemoryFactProposal{{Key: "name", Value: []byte(`"Ada"`)}}, nil
	}), Approval: func(context.Context, MemoryWriteRequest) (bool, error) { return true, nil }, Audit: func(context.Context, MemoryAuditEvent) error { return nil }}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), RunInput{ThreadID: "thread", Messages: []Message{{Role: RoleUser, Content: "hello"}}}); err != nil {
		t.Fatal(err)
	}
	if len(model.calls) != 1 || len(model.calls[0].Messages) < 2 || !strings.Contains(model.calls[0].Messages[0].Content, "locale") {
		t.Fatalf("request = %#v", model.calls)
	}
	if _, err := store.WorkingMemory().GetWorkingMemoryFact(context.Background(), scope, "name"); err != nil {
		t.Fatal(err)
	}
	messages, err := store.Messages().ListMessages(context.Background(), "thread", PageRequest{})
	if err != nil || len(messages.Records) != 2 {
		t.Fatalf("stored messages = %#v, %v", messages, err)
	}
}
