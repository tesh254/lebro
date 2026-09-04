package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// transcriptOnlyStore is a minimal custom adapter implementing exactly the
// transcript capability. It backs the integration tests for attaching a
// developer-owned RuntimeStore to an agent.
type transcriptOnlyStore struct {
	mu       sync.Mutex
	threads  map[ThreadID]ThreadRecord
	messages map[ThreadID][]MessageRecord
}

var (
	_ RuntimeStore    = (*transcriptOnlyStore)(nil)
	_ TranscriptStore = (*transcriptOnlyStore)(nil)
)

func newTranscriptOnlyStore() *transcriptOnlyStore {
	return &transcriptOnlyStore{
		threads:  map[ThreadID]ThreadRecord{},
		messages: map[ThreadID][]MessageRecord{},
	}
}

func (s *transcriptOnlyStore) Capabilities() StoreCapabilities {
	return StoreCapabilities{Transcript: true}
}

func (s *transcriptOnlyStore) Threads() ThreadRepository   { return s }
func (s *transcriptOnlyStore) Messages() MessageRepository { return s }

func (s *transcriptOnlyStore) CreateThread(_ context.Context, record ThreadRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.threads[record.ID]; ok {
		return fmt.Errorf("lebro: thread %q already exists", record.ID)
	}
	s.threads[record.ID] = record
	return nil
}

func (s *transcriptOnlyStore) GetThread(_ context.Context, id ThreadID) (ThreadRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.threads[id]
	if !ok {
		return ThreadRecord{}, ErrNotFound
	}
	return record, nil
}

func (s *transcriptOnlyStore) UpdateThread(_ context.Context, record ThreadRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.threads[record.ID]; !ok {
		return ErrNotFound
	}
	s.threads[record.ID] = record
	return nil
}

func (s *transcriptOnlyStore) AppendMessages(_ context.Context, records []MessageRecord) error {
	if len(records) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, record := range records {
		if _, ok := s.threads[record.ThreadID]; !ok {
			return ErrNotFound
		}
		for _, existing := range s.messages[record.ThreadID] {
			if existing.ID == record.ID {
				return fmt.Errorf("lebro: message %q already exists", record.ID)
			}
		}
	}
	for _, record := range records {
		s.messages[record.ThreadID] = append(s.messages[record.ThreadID], record)
	}
	return nil
}

func (s *transcriptOnlyStore) UpdateMessages(_ context.Context, records []MessageRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, record := range records {
		stored := s.messages[record.ThreadID]
		for i, existing := range stored {
			if existing.ID == record.ID {
				stored[i] = record
			}
		}
	}
	return nil
}

func (s *transcriptOnlyStore) DeleteMessages(_ context.Context, threadID ThreadID, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	remove := map[string]struct{}{}
	for _, id := range ids {
		remove[id] = struct{}{}
	}
	kept := s.messages[threadID][:0]
	for _, message := range s.messages[threadID] {
		if _, ok := remove[message.ID]; !ok {
			kept = append(kept, message)
		}
	}
	s.messages[threadID] = kept
	return nil
}

func (s *transcriptOnlyStore) ListMessages(_ context.Context, threadID ThreadID, page PageRequest) (Page[MessageRecord], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return paginate(s.messages[threadID], page, cloneMessageRecord)
}

func (s *transcriptOnlyStore) messageCount(threadID ThreadID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.messages[threadID])
}

// memoryOnlyStore advertises exactly one non-transcript capability so tests
// can assert typed failures when a feature needs what the adapter lacks.
type memoryOnlyStore struct{}

func (memoryOnlyStore) Capabilities() StoreCapabilities {
	return StoreCapabilities{WorkingMemory: true}
}
func (memoryOnlyStore) WorkingMemory() WorkingMemoryRepository { return uncapableWorkingMemory{} }

// brokenAdvertisedStore advertises a capability whose interface it does not
// implement.
type brokenAdvertisedStore struct{}

func (brokenAdvertisedStore) Capabilities() StoreCapabilities {
	return StoreCapabilities{Transcript: true}
}

// brokenHiddenStore implements TranscriptStore but advertises only working
// memory, so the transcript interface is implemented but not advertised.
type brokenHiddenStore struct {
	transcriptOnlyStore
}

func (*brokenHiddenStore) Capabilities() StoreCapabilities {
	return StoreCapabilities{WorkingMemory: true}
}
func (*brokenHiddenStore) WorkingMemory() WorkingMemoryRepository {
	return uncapableWorkingMemory{}
}

func TestValidateRuntimeStoreRejectsBrokenAdvertisements(t *testing.T) {
	t.Parallel()
	if _, err := validateRuntimeStore(brokenAdvertisedStore{}); !errors.Is(err, ErrCapabilityMissing) {
		t.Fatalf("advertised-but-missing error = %v, want ErrCapabilityMissing", err)
	} else {
		var capabilityErr *StoreCapabilityError
		if !errors.As(err, &capabilityErr) || capabilityErr.Capability != StoreCapabilityTranscript {
			t.Fatalf("error = %v, want transcript StoreCapabilityError", err)
		}
	}
	if _, err := validateRuntimeStore(&brokenHiddenStore{}); !errors.Is(err, ErrCapabilityMissing) {
		t.Fatalf("implemented-but-unadvertised error = %v, want ErrCapabilityMissing", err)
	}
	if _, err := validateRuntimeStore(memoryOnlyStore{}); err != nil {
		t.Fatalf("consistent partial adapter rejected: %v", err)
	}
	if _, err := validateRuntimeStore(nil); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := validateRuntimeStore((*transcriptOnlyStore)(nil)); err == nil {
		t.Fatal("typed-nil store accepted")
	}
}

func TestBridgeFailsTypedOnUnsupportedCapabilityReachThrough(t *testing.T) {
	t.Parallel()
	store, err := bridgeRuntimeStore(newTranscriptOnlyStore())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.WorkingMemory().GetWorkingMemoryFact(context.Background(), WorkingMemoryScope{Namespace: "ns", OwnerID: "o"}, "k"); !errors.Is(err, ErrCapabilityMissing) {
		t.Fatalf("WorkingMemory reach-through = %v, want ErrCapabilityMissing", err)
	}
	if _, err := store.WorkflowRuns().GetWorkflowRun(context.Background(), "run-1"); !errors.Is(err, ErrCapabilityMissing) {
		t.Fatalf("WorkflowRuns reach-through = %v, want ErrCapabilityMissing", err)
	}
	if err := store.Schedules().DeleteSchedule(context.Background(), "schedule-1"); !errors.Is(err, ErrCapabilityMissing) {
		t.Fatalf("Schedules reach-through = %v, want ErrCapabilityMissing", err)
	}
}

// TestBridgeSequentialFallbackKeepsEarlierWrites pins the documented fallback
// semantics: an adapter without the transactions capability gets sequential
// writes in the coupled order, and a failure mid-sequence leaves the earlier
// writes in place.
func TestBridgeSequentialFallbackKeepsEarlierWrites(t *testing.T) {
	t.Parallel()
	adapter := newTranscriptOnlyStore()
	store, err := bridgeRuntimeStore(adapter)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	failure := errors.New("lebro: second write failed")
	err = store.Transaction(ctx, func(ctx context.Context, repos Repositories) error {
		if err := repos.Threads().CreateThread(ctx, ThreadRecord{ID: "fallback-thread", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("Transaction error = %v, want the injected failure", err)
	}
	if adapter.messageCount("fallback-thread") != 0 {
		t.Fatal("no messages were written, so none should persist")
	}
	if _, err := store.Threads().GetThread(ctx, "fallback-thread"); err != nil {
		t.Fatalf("thread written before the failure did not persist: %v", err)
	}
}

// transcriptObservabilityStore extends the transcript adapter with the
// observability capability but no transactions, exercising the sequential
// coupled write for a partial adapter.
type transcriptObservabilityStore struct {
	*transcriptOnlyStore
	events   map[RunID][]RunEventRecord
	attempts map[RunID][]ModelAttemptRecord
}

var _ ObservabilityStore = (*transcriptObservabilityStore)(nil)

func newTranscriptObservabilityStore() *transcriptObservabilityStore {
	return &transcriptObservabilityStore{
		transcriptOnlyStore: newTranscriptOnlyStore(),
		events:              map[RunID][]RunEventRecord{},
		attempts:            map[RunID][]ModelAttemptRecord{},
	}
}

func (s *transcriptObservabilityStore) Capabilities() StoreCapabilities {
	return StoreCapabilities{Transcript: true, Observability: true}
}
func (s *transcriptObservabilityStore) RunEvents() RunEventRepository         { return s }
func (s *transcriptObservabilityStore) ModelAttempts() ModelAttemptRepository { return s }
func (s *transcriptObservabilityStore) ToolExecutions() ToolExecutionRepository {
	return uncapableToolExecutions{}
}

func (s *transcriptObservabilityStore) AppendRunEvents(_ context.Context, records []RunEventRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, record := range records {
		s.events[record.RunID] = append(s.events[record.RunID], record)
	}
	return nil
}

func (s *transcriptObservabilityStore) ListRunEvents(_ context.Context, filter RunEventFilter, page PageRequest) (Page[RunEventRecord], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return paginate(s.events[filter.RunID], page, cloneRunEventRecord)
}

func (s *transcriptObservabilityStore) SaveModelAttempts(_ context.Context, records []ModelAttemptRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, record := range records {
		s.attempts[record.RunID] = append(s.attempts[record.RunID], record)
	}
	return nil
}

func (s *transcriptObservabilityStore) ListModelAttempts(_ context.Context, filter ModelAttemptFilter, page PageRequest) (Page[ModelAttemptRecord], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return paginate(s.attempts[filter.RunID], page, cloneModelAttemptRecord)
}

// TestAgentRuntimeStorePersistsAndReloadsMultiTurnThread is the deterministic
// integration check for acceptance: a custom adapter attached through
// AgentConfig.RuntimeStore persists one run's transcript and reloads it as
// prior context for the next run on the same thread.
func TestAgentRuntimeStorePersistsAndReloadsMultiTurnThread(t *testing.T) {
	t.Parallel()
	adapter := newTranscriptOnlyStore()
	model := newScriptedModel(textResponse("first answer"), textResponse("second answer"))
	agent, err := NewAgent(AgentConfig{
		Definition:   AgentDefinition{ID: "agent-1", Model: "fixture-model", Instructions: "be brief"},
		Model:        model,
		RuntimeStore: adapter,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := agent.Run(ctx, RunInput{
		ThreadID: "support-42",
		Messages: []Message{{Role: RoleUser, Content: "first question"}},
	}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if got := adapter.messageCount("support-42"); got != 2 {
		t.Fatalf("messages after first run = %d, want 2 (user+assistant)", got)
	}
	result, err := agent.Run(ctx, RunInput{
		ThreadID: "support-42",
		Messages: []Message{{Role: RoleUser, Content: "second question"}},
	})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := adapter.messageCount("support-42"); got != 4 {
		t.Fatalf("messages after second run = %d, want 4", got)
	}
	// The second model call must have received the reloaded history ahead of
	// the new input.
	if len(model.calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(model.calls))
	}
	reloaded := model.calls[1].Messages
	if len(reloaded) != 4 {
		t.Fatalf("reloaded transcript = %d messages, want system + 3", len(reloaded))
	}
	want := []struct {
		role    Role
		content string
	}{
		{RoleSystem, "be brief"},
		{RoleUser, "first question"},
		{RoleAssistant, "first answer"},
		{RoleUser, "second question"},
	}
	for i, w := range want {
		if reloaded[i].Role != w.role || reloaded[i].Content != w.content {
			t.Fatalf("reloaded[%d] = %s/%q, want %s/%q", i, reloaded[i].Role, reloaded[i].Content, w.role, w.content)
		}
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("second run status = %q", result.Status)
	}
}

func TestAgentRuntimeStoreWithoutTranscriptFailsTypedBeforeModelCall(t *testing.T) {
	t.Parallel()
	model := newScriptedModel(textResponse("should never run"))
	agent, err := NewAgent(AgentConfig{
		Definition:   AgentDefinition{ID: "agent-1", Model: "fixture-model"},
		Model:        model,
		RuntimeStore: memoryOnlyStore{},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = agent.Run(context.Background(), RunInput{
		ThreadID: "support-42",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	var capabilityErr *StoreCapabilityError
	if !errors.As(err, &capabilityErr) || capabilityErr.Capability != StoreCapabilityTranscript {
		t.Fatalf("run error = %v, want transcript StoreCapabilityError", err)
	}
	if !errors.Is(err, ErrCapabilityMissing) {
		t.Fatalf("run error = %v, want ErrCapabilityMissing sentinel", err)
	}
	if capabilityErr.Feature != "thread persistence" {
		t.Fatalf("feature = %q, want thread persistence", capabilityErr.Feature)
	}
	if len(model.calls) != 0 {
		t.Fatalf("model was called %d times before the capability check", len(model.calls))
	}
}

func TestAgentRuntimeStoreRequiresWorkingMemoryCapabilityForMemoryProcessor(t *testing.T) {
	t.Parallel()
	_, err := NewAgent(AgentConfig{
		Definition:   AgentDefinition{ID: "agent-1", Model: "fixture-model"},
		Model:        newScriptedModel(),
		RuntimeStore: newTranscriptOnlyStore(),
		Memory:       &MemoryProcessorConfig{Scope: WorkingMemoryScope{Namespace: "ns", OwnerID: "owner"}},
	})
	var capabilityErr *StoreCapabilityError
	if !errors.As(err, &capabilityErr) || capabilityErr.Capability != StoreCapabilityWorkingMemory {
		t.Fatalf("construction error = %v, want working-memory StoreCapabilityError", err)
	}
}

func TestAgentStoreAndRuntimeStoreAreMutuallyExclusive(t *testing.T) {
	t.Parallel()
	_, err := NewAgent(AgentConfig{
		Definition:   AgentDefinition{ID: "agent-1", Model: "fixture-model"},
		Model:        newScriptedModel(),
		Store:        NewMemoryStore(),
		RuntimeStore: newTranscriptOnlyStore(),
	})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("construction error = %v, want mutual-exclusion error", err)
	}
}

// TestAgentRuntimeStoreObservabilityWithoutTransactions pins the coupled-write
// fallback: a transcript+observability adapter without transactions persists
// the transcript and the diagnostics sequentially and the run still succeeds.
func TestAgentRuntimeStoreObservabilityWithoutTransactions(t *testing.T) {
	t.Parallel()
	adapter := newTranscriptObservabilityStore()
	agent, err := NewAgent(AgentConfig{
		Definition:   AgentDefinition{ID: "agent-1", Model: "fixture-model", Instructions: "be brief"},
		Model:        newScriptedModel(textResponse("answer")),
		RuntimeStore: adapter,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), RunInput{
		ThreadID: "support-7",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
	if got := adapter.messageCount("support-7"); got != 2 {
		t.Fatalf("messages = %d, want 2", got)
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if len(adapter.events[result.ID]) == 0 {
		t.Fatal("no run events persisted through the observability capability")
	}
	if len(adapter.attempts[result.ID]) != 1 {
		t.Fatalf("model attempts = %d, want 1", len(adapter.attempts[result.ID]))
	}
}

// TestBuiltInStoresSatisfyRuntimeStoreContract pins the backward-compatibility
// requirement: the built-in stores advertise and implement every capability.
func TestBuiltInStoresSatisfyRuntimeStoreContract(t *testing.T) {
	t.Parallel()
	stores := []RuntimeStore{NewMemoryStore()}
	want := AllStoreCapabilities()
	for _, store := range stores {
		caps, err := validateRuntimeStore(store)
		if err != nil {
			t.Fatalf("%T: %v", store, err)
		}
		if caps != want {
			t.Fatalf("%T capabilities = %+v, want %+v", store, caps, want)
		}
	}
	sqlite, err := NewSQLiteStore(t.TempDir() + "/runtime-store-contract.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlite.Close() })
	caps, err := validateRuntimeStore(sqlite)
	if err != nil {
		t.Fatalf("SQLiteStore: %v", err)
	}
	if caps != want {
		t.Fatalf("SQLiteStore capabilities = %+v, want %+v", caps, want)
	}
}

// transactionalTranscriptStore adds InTransaction to the transcript double
// with copy-on-write commit semantics, so tests can drive the bridge's
// transactional delegation path.
type transactionalTranscriptStore struct {
	*transcriptOnlyStore
}

var (
	_ RuntimeStore       = (*transactionalTranscriptStore)(nil)
	_ TransactionalStore = (*transactionalTranscriptStore)(nil)
)

func newTransactionalTranscriptStore() *transactionalTranscriptStore {
	return &transactionalTranscriptStore{transcriptOnlyStore: newTranscriptOnlyStore()}
}

func (s *transactionalTranscriptStore) Capabilities() StoreCapabilities {
	return StoreCapabilities{Transcript: true, Transactions: true}
}

func (s *transactionalTranscriptStore) InTransaction(ctx context.Context, fn func(context.Context, RuntimeStore) error) error {
	s.mu.Lock()
	tx := &transcriptOnlyStore{
		threads:  make(map[ThreadID]ThreadRecord, len(s.threads)),
		messages: make(map[ThreadID][]MessageRecord, len(s.messages)),
	}
	for id, record := range s.threads {
		tx.threads[id] = record
	}
	for id, records := range s.messages {
		tx.messages[id] = append([]MessageRecord(nil), records...)
	}
	s.mu.Unlock()
	if err := fn(ctx, tx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.threads, s.messages = tx.threads, tx.messages
	return nil
}

func TestBridgeDelegatesToTransactionalStore(t *testing.T) {
	t.Parallel()
	adapter := newTransactionalTranscriptStore()
	store, err := bridgeRuntimeStore(adapter)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.Transaction(ctx, func(ctx context.Context, repos Repositories) error {
		return repos.Threads().CreateThread(ctx, ThreadRecord{ID: "tx-delegated", CreatedAt: now, UpdatedAt: now})
	}); err != nil {
		t.Fatalf("Transaction commit: %v", err)
	}
	if _, err := store.Threads().GetThread(ctx, "tx-delegated"); err != nil {
		t.Fatalf("committed record missing: %v", err)
	}
	rollback := errors.New("lebro: rollback requested")
	err = store.Transaction(ctx, func(ctx context.Context, repos Repositories) error {
		if err := repos.Threads().CreateThread(ctx, ThreadRecord{ID: "tx-discarded", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("Transaction error = %v, want the injected failure", err)
	}
	if _, err := store.Threads().GetThread(ctx, "tx-discarded"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("discarded record visible: %v", err)
	}
}

// TestBridgeUncapableAccessorsFailTyped sweeps every unsupported repository
// accessor on a transcript-only bridge so the reach-through stubs fail with
// the typed capability error.
func TestBridgeUncapableAccessorsFailTyped(t *testing.T) {
	t.Parallel()
	store, err := bridgeRuntimeStore(newTranscriptOnlyStore())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate on a custom adapter = %v, want a no-op", err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	mustCapability := func(err error) {
		t.Helper()
		if !errors.Is(err, ErrCapabilityMissing) {
			t.Fatalf("reach-through error = %v, want ErrCapabilityMissing", err)
		}
	}
	if _, err := store.WorkingMemory().UpsertWorkingMemoryFact(ctx, WorkingMemoryFact{Key: "k"}, 0); err != nil {
		mustCapability(err)
	}
	if err := store.WorkingMemory().ClearWorkingMemory(ctx, WorkingMemoryScope{}); err != nil {
		mustCapability(err)
	}
	if err := store.WorkflowRuns().SaveWorkflowRun(ctx, WorkflowRunRecord{ID: "r"}); err != nil {
		mustCapability(err)
	}
	if err := store.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, WorkflowSnapshotRecord{ID: "s", RunID: "r"}); err != nil {
		mustCapability(err)
	}
	if err := store.Schedules().SaveSchedule(ctx, ScheduleRecord{ID: "s"}); err != nil {
		mustCapability(err)
	}
	if err := store.ScheduleExecutions().SaveScheduleExecution(ctx, ScheduleExecutionRecord{ID: "e", ScheduleID: "s"}); err != nil {
		mustCapability(err)
	}
	if _, err := store.WorkingMemory().ListWorkingMemoryFacts(ctx, WorkingMemoryScope{}, PageRequest{}); err != nil {
		mustCapability(err)
	}
	if _, err := store.WorkflowRuns().ListWorkflowRuns(ctx, WorkflowRunFilter{}, PageRequest{}); err != nil {
		mustCapability(err)
	}
	if _, err := store.WorkflowSnapshots().ListWorkflowSnapshots(ctx, "r", PageRequest{}); err != nil {
		mustCapability(err)
	}
	if _, err := store.Schedules().ListSchedules(ctx, ScheduleFilter{}, PageRequest{}); err != nil {
		mustCapability(err)
	}
	if _, err := store.ScheduleExecutions().ListScheduleExecutions(ctx, "s", PageRequest{}); err != nil {
		mustCapability(err)
	}
	if _, err := store.Threads().GetThread(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("supported capability read = %v, want ErrNotFound", err)
	}
	if err := store.Threads().CreateThread(ctx, ThreadRecord{ID: "supported", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("supported capability write: %v", err)
	}
}

// TestObservableBridgeExposesObservabilityRepositories pins the opt-in: a
// bridge over an adapter with the observability capability satisfies
// ObservabilityRepositories, and one without it does not.
func TestObservableBridgeExposesObservabilityRepositories(t *testing.T) {
	t.Parallel()
	with, err := bridgeRuntimeStore(newTranscriptObservabilityStore())
	if err != nil {
		t.Fatal(err)
	}
	observability, ok := with.(ObservabilityRepositories)
	if !ok {
		t.Fatal("bridge without observability methods despite the capability")
	}
	ctx := context.Background()
	if _, err := observability.RunEvents().ListRunEvents(ctx, RunEventFilter{}, PageRequest{}); err != nil {
		t.Fatalf("ListRunEvents through the bridge: %v", err)
	}
	if _, err := observability.ModelAttempts().ListModelAttempts(ctx, ModelAttemptFilter{}, PageRequest{}); err != nil {
		t.Fatalf("ListModelAttempts through the bridge: %v", err)
	}
	// The adapter declares the capability but stubs tool executions, so the
	// reach-through fails typed.
	if _, err := observability.ToolExecutions().ListToolExecutions(ctx, ToolExecutionFilter{}, PageRequest{}); !errors.Is(err, ErrCapabilityMissing) {
		t.Fatalf("ToolExecutions reach-through = %v, want ErrCapabilityMissing", err)
	}
	without, err := bridgeRuntimeStore(newTranscriptOnlyStore())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := without.(ObservabilityRepositories); ok {
		t.Fatal("bridge satisfies ObservabilityRepositories without the capability")
	}
}

// TestRuntimeStoreViewRoundTrip exercises every capability accessor on the
// transaction-scoped view that built-in stores hand to TransactionalStore.
func TestRuntimeStoreViewRoundTrip(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	now := time.Now().UTC()
	err := store.InTransaction(ctx, func(ctx context.Context, view RuntimeStore) error {
		caps := view.Capabilities()
		if !caps.Has(StoreCapabilityTranscript) || !caps.Has(StoreCapabilityTransactions) {
			t.Fatal("view capabilities incomplete")
		}
		transcript := view.(TranscriptStore)
		if err := transcript.Threads().CreateThread(ctx, ThreadRecord{ID: "view-thread", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := transcript.Threads().UpdateThread(ctx, ThreadRecord{ID: "view-thread", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := transcript.Messages().AppendMessages(ctx, []MessageRecord{{ID: "view-msg-1", ThreadID: "view-thread", Message: Message{Role: RoleUser, Content: "hi"}, CreatedAt: now}}); err != nil {
			return err
		}
		if err := transcript.Messages().UpdateMessages(ctx, []MessageRecord{{ID: "view-msg-1", ThreadID: "view-thread", Message: Message{Role: RoleUser, Content: "edited"}, CreatedAt: now}}); err != nil {
			return err
		}
		if err := transcript.Messages().DeleteMessages(ctx, "view-thread", nil); err != nil {
			return err
		}
		if _, err := transcript.Messages().ListMessages(ctx, "view-thread", PageRequest{}); err != nil {
			return err
		}
		fact, err := view.(WorkingMemoryStore).WorkingMemory().UpsertWorkingMemoryFact(ctx, WorkingMemoryFact{
			ID: "view-fact", Namespace: "ns", OwnerID: "o", Key: "k", Value: []byte(`1`), CreatedAt: now, UpdatedAt: now,
		}, 0)
		if err != nil {
			return err
		}
		if _, err := view.(WorkingMemoryStore).WorkingMemory().GetWorkingMemoryFact(ctx, WorkingMemoryScope{Namespace: "ns", OwnerID: "o"}, "k"); err != nil {
			return err
		}
		if _, err := view.(WorkingMemoryStore).WorkingMemory().ListWorkingMemoryFacts(ctx, WorkingMemoryScope{Namespace: "ns", OwnerID: "o"}, PageRequest{}); err != nil {
			return err
		}
		if err := view.(WorkingMemoryStore).WorkingMemory().DeleteWorkingMemoryFact(ctx, WorkingMemoryScope{Namespace: "ns", OwnerID: "o"}, "k", fact.Version); err != nil {
			return err
		}
		state := view.(WorkflowStateStore)
		if err := state.WorkflowRuns().SaveWorkflowRun(ctx, WorkflowRunRecord{ID: "view-run", WorkflowID: "wf", Status: RunStatusRunning, StartedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if _, err := state.WorkflowRuns().GetWorkflowRun(ctx, "view-run"); err != nil {
			return err
		}
		if _, err := state.WorkflowRuns().ListWorkflowRuns(ctx, WorkflowRunFilter{}, PageRequest{}); err != nil {
			return err
		}
		if err := state.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, WorkflowSnapshotRecord{ID: "view-snap", RunID: "view-run", Sequence: 1, State: []byte(`{}`), CreatedAt: now}); err != nil {
			return err
		}
		if _, err := state.WorkflowSnapshots().ListWorkflowSnapshots(ctx, "view-run", PageRequest{}); err != nil {
			return err
		}
		schedules := view.(ScheduleStore)
		next := now.Add(time.Hour)
		if err := schedules.Schedules().SaveSchedule(ctx, ScheduleRecord{ID: "view-schedule", WorkflowID: "wf", Spec: "* * * * *", NextFireAt: &next, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if _, err := schedules.Schedules().GetSchedule(ctx, "view-schedule"); err != nil {
			return err
		}
		if _, err := schedules.Schedules().ListSchedules(ctx, ScheduleFilter{}, PageRequest{}); err != nil {
			return err
		}
		if err := schedules.ScheduleExecutions().SaveScheduleExecution(ctx, ScheduleExecutionRecord{ID: "view-exec", ScheduleID: "view-schedule", Status: ScheduleExecSucceeded, ScheduledFor: now, StartedAt: now}); err != nil {
			return err
		}
		if _, err := schedules.ScheduleExecutions().ListScheduleExecutions(ctx, "view-schedule", PageRequest{}); err != nil {
			return err
		}
		if err := schedules.Schedules().DeleteSchedule(ctx, "view-schedule"); err != nil {
			return err
		}
		observability := view.(ObservabilityStore)
		if err := observability.RunEvents().AppendRunEvents(ctx, []RunEventRecord{{ID: "view-event", RunID: "view-run", Sequence: 1, Type: RunEventStarted, Timestamp: now}}); err != nil {
			return err
		}
		if _, err := observability.RunEvents().ListRunEvents(ctx, RunEventFilter{}, PageRequest{}); err != nil {
			return err
		}
		if err := observability.ModelAttempts().SaveModelAttempts(ctx, []ModelAttemptRecord{{ID: "view-attempt", RunID: "view-run", Index: 1, Status: ModelAttemptSuccess, StartedAt: now, FinishedAt: now}}); err != nil {
			return err
		}
		if _, err := observability.ModelAttempts().ListModelAttempts(ctx, ModelAttemptFilter{}, PageRequest{}); err != nil {
			return err
		}
		if err := observability.ToolExecutions().SaveToolExecutions(ctx, []ToolExecutionRecord{{ID: "view-tool", RunID: "view-run", ToolCallID: "call-1", ToolID: "t", State: ToolExecutionSucceeded, StartedAt: now, FinishedAt: now}}); err != nil {
			return err
		}
		if _, err := observability.ToolExecutions().ListToolExecutions(ctx, ToolExecutionFilter{}, PageRequest{}); err != nil {
			return err
		}
		// A view over repositories without the observability opt-in fails
		// typed instead of panicking.
		bare := newRuntimeStoreView(AllStoreCapabilities(), uncapableRepositories{})
		if _, err := bare.(ObservabilityStore).RunEvents().ListRunEvents(ctx, RunEventFilter{}, PageRequest{}); !errors.Is(err, ErrCapabilityMissing) {
			t.Fatalf("view without observability = %v, want ErrCapabilityMissing", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InTransaction: %v", err)
	}
	if _, err := store.Threads().GetThread(ctx, "view-thread"); err != nil {
		t.Fatalf("committed view write missing: %v", err)
	}
}

// uncapableRepositories implements Repositories with typed failures so the
// view's no-observability branch is reachable without a full adapter.
type uncapableRepositories struct{}

func (uncapableRepositories) Threads() ThreadRepository           { return uncapableThreads{} }
func (uncapableRepositories) Messages() MessageRepository         { return uncapableMessages{} }
func (uncapableRepositories) WorkflowRuns() WorkflowRunRepository { return uncapableWorkflowRuns{} }
func (uncapableRepositories) WorkflowSnapshots() WorkflowSnapshotRepository {
	return uncapableWorkflowSnapshots{}
}
func (uncapableRepositories) Schedules() ScheduleRepository { return uncapableSchedules{} }
func (uncapableRepositories) ScheduleExecutions() ScheduleExecutionRepository {
	return uncapableScheduleExecutions{}
}
func (uncapableRepositories) WorkingMemory() WorkingMemoryRepository {
	return uncapableWorkingMemory{}
}

func TestStoreCapabilityErrorMessage(t *testing.T) {
	t.Parallel()
	featured := &StoreCapabilityError{Capability: StoreCapabilityTranscript, Feature: "thread persistence", Reason: "not advertised"}
	if got := featured.Error(); got != `lebro: storage adapter does not support capability "transcript" required by thread persistence: not advertised` {
		t.Fatalf("Error() = %q", got)
	}
	plain := &StoreCapabilityError{Capability: StoreCapabilitySchedules, Reason: "inconsistent"}
	if got := plain.Error(); got != `lebro: storage adapter capability "schedules": inconsistent` {
		t.Fatalf("Error() = %q", got)
	}
	var none StoreCapabilities
	if none.Has(StoreCapability("bogus")) {
		t.Fatal("unknown capability reported as supported")
	}
}

// TestBridgeRepositoriesViewCoversAllAccessors drives every repository
// accessor on the Repositories views handed to Transaction callbacks — both
// the sequential fallback and the unsupported-capability stubs.
func TestBridgeRepositoriesViewCoversAllAccessors(t *testing.T) {
	t.Parallel()
	store, err := bridgeRuntimeStore(newTranscriptOnlyStore())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	err = store.Transaction(ctx, func(ctx context.Context, repos Repositories) error {
		mustCapability := func(err error) {
			t.Helper()
			if !errors.Is(err, ErrCapabilityMissing) {
				t.Fatalf("view reach-through error = %v, want ErrCapabilityMissing", err)
			}
		}
		if _, err := repos.WorkingMemory().GetWorkingMemoryFact(ctx, WorkingMemoryScope{}, ""); err != nil {
			mustCapability(err)
		}
		if _, err := repos.WorkflowRuns().GetWorkflowRun(ctx, "r"); err != nil {
			mustCapability(err)
		}
		if _, err := repos.WorkflowSnapshots().ListWorkflowSnapshots(ctx, "r", PageRequest{}); err != nil {
			mustCapability(err)
		}
		if _, err := repos.Schedules().GetSchedule(ctx, "s"); err != nil {
			mustCapability(err)
		}
		if _, err := repos.ScheduleExecutions().ListScheduleExecutions(ctx, "s", PageRequest{}); err != nil {
			mustCapability(err)
		}
		if _, err := repos.Threads().GetThread(ctx, "missing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("supported read = %v, want ErrNotFound", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Transaction: %v", err)
	}
	// The observability repos view routes tool executions through the
	// capability accessor even when the adapter stubs them.
	observability, err := bridgeRuntimeStore(newTranscriptObservabilityStore())
	if err != nil {
		t.Fatal(err)
	}
	err = observability.Transaction(ctx, func(ctx context.Context, repos Repositories) error {
		_, err := repos.(ObservabilityRepositories).ToolExecutions().ListToolExecutions(ctx, ToolExecutionFilter{}, PageRequest{})
		if !errors.Is(err, ErrCapabilityMissing) {
			t.Fatalf("ToolExecutions through the repos view = %v, want ErrCapabilityMissing", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("observability Transaction: %v", err)
	}
	// A view over repositories without the observability opt-in fails typed
	// for attempts and tool executions too, not only events.
	bare := newRuntimeStoreView(AllStoreCapabilities(), uncapableRepositories{})
	if _, err := bare.(ObservabilityStore).ModelAttempts().ListModelAttempts(ctx, ModelAttemptFilter{}, PageRequest{}); !errors.Is(err, ErrCapabilityMissing) {
		t.Fatalf("bare view attempts = %v, want ErrCapabilityMissing", err)
	}
	if _, err := bare.(ObservabilityStore).ToolExecutions().ListToolExecutions(ctx, ToolExecutionFilter{}, PageRequest{}); !errors.Is(err, ErrCapabilityMissing) {
		t.Fatalf("bare view tool executions = %v, want ErrCapabilityMissing", err)
	}
	if err := bare.(WorkingMemoryStore).WorkingMemory().ClearWorkingMemory(ctx, WorkingMemoryScope{}); !errors.Is(err, ErrCapabilityMissing) {
		t.Fatalf("bare view working memory = %v, want ErrCapabilityMissing", err)
	}
}

// TestUncapableRepositoriesFailTyped drives every stub repository method so
// accidental reach-throughs always surface the typed capability error.
func TestUncapableRepositoriesFailTyped(t *testing.T) {
	t.Parallel()
	must := func(err error) {
		t.Helper()
		if !errors.Is(err, ErrCapabilityMissing) {
			t.Fatalf("stub error = %v, want ErrCapabilityMissing", err)
		}
	}
	ctx := context.Background()
	threads := uncapableThreads{}
	must(threads.CreateThread(ctx, ThreadRecord{}))
	if _, err := threads.GetThread(ctx, "t"); err != nil {
		must(err)
	}
	must(threads.UpdateThread(ctx, ThreadRecord{}))
	messages := uncapableMessages{}
	must(messages.AppendMessages(ctx, nil))
	must(messages.UpdateMessages(ctx, nil))
	must(messages.DeleteMessages(ctx, "t", nil))
	if _, err := messages.ListMessages(ctx, "t", PageRequest{}); err != nil {
		must(err)
	}
	memory := uncapableWorkingMemory{}
	if _, err := memory.GetWorkingMemoryFact(ctx, WorkingMemoryScope{}, ""); err != nil {
		must(err)
	}
	if _, err := memory.ListWorkingMemoryFacts(ctx, WorkingMemoryScope{}, PageRequest{}); err != nil {
		must(err)
	}
	must(memory.DeleteWorkingMemoryFact(ctx, WorkingMemoryScope{}, "", 0))
	must(memory.ClearWorkingMemory(ctx, WorkingMemoryScope{}))
	events := uncapableRunEvents{}
	must(events.AppendRunEvents(ctx, nil))
	if _, err := events.ListRunEvents(ctx, RunEventFilter{}, PageRequest{}); err != nil {
		must(err)
	}
	attempts := uncapableModelAttempts{}
	must(attempts.SaveModelAttempts(ctx, nil))
	if _, err := attempts.ListModelAttempts(ctx, ModelAttemptFilter{}, PageRequest{}); err != nil {
		must(err)
	}
	tools := uncapableToolExecutions{}
	must(tools.SaveToolExecutions(ctx, nil))
	if _, err := tools.ListToolExecutions(ctx, ToolExecutionFilter{}, PageRequest{}); err != nil {
		must(err)
	}
}

// TestBridgeAccessorsResolveBothBranches drives every bridge accessor helper
// against a full-capability store and a capability-less store, so both the
// delegation and the stub halves stay covered.
func TestBridgeAccessorsResolveBothBranches(t *testing.T) {
	t.Parallel()
	full := NewMemoryStore()
	empty := memoryOnlyStore{}
	mustTyped := func(repo any) {
		t.Helper()
		if repo == nil || isNilInterface(repo) {
			t.Fatal("accessor returned nil repository")
		}
	}
	if repo := bridgeThreads(full); repo == nil {
		t.Fatal("bridgeThreads(full) = nil")
	}
	mustTyped(bridgeThreads(empty))
	if repo := bridgeMessages(full); repo == nil {
		t.Fatal("bridgeMessages(full) = nil")
	}
	mustTyped(bridgeMessages(empty))
	if repo := bridgeWorkingMemory(full); repo == nil {
		t.Fatal("bridgeWorkingMemory(full) = nil")
	}
	mustTyped(bridgeWorkingMemory(empty))
	if repo := bridgeWorkflowRuns(full); repo == nil {
		t.Fatal("bridgeWorkflowRuns(full) = nil")
	}
	mustTyped(bridgeWorkflowRuns(empty))
	if repo := bridgeWorkflowSnapshots(full); repo == nil {
		t.Fatal("bridgeWorkflowSnapshots(full) = nil")
	}
	mustTyped(bridgeWorkflowSnapshots(empty))
	if repo := bridgeSchedules(full); repo == nil {
		t.Fatal("bridgeSchedules(full) = nil")
	}
	mustTyped(bridgeSchedules(empty))
	if repo := bridgeScheduleExecutions(full); repo == nil {
		t.Fatal("bridgeScheduleExecutions(full) = nil")
	}
	mustTyped(bridgeScheduleExecutions(empty))
	if repo := bridgeRunEvents(full); repo == nil {
		t.Fatal("bridgeRunEvents(full) = nil")
	}
	mustTyped(bridgeRunEvents(empty))
	if repo := bridgeModelAttempts(full); repo == nil {
		t.Fatal("bridgeModelAttempts(full) = nil")
	}
	mustTyped(bridgeModelAttempts(empty))
	if repo := bridgeToolExecutions(full); repo == nil {
		t.Fatal("bridgeToolExecutions(full) = nil")
	}
	mustTyped(bridgeToolExecutions(empty))
	if _, err := bridgeRuntimeStore(brokenAdvertisedStore{}); !errors.Is(err, ErrCapabilityMissing) {
		t.Fatalf("bridgeRuntimeStore(broken) = %v, want ErrCapabilityMissing", err)
	}
	if _, err := bridgeRuntimeStore(memoryOnlyStore{}); err != nil {
		t.Fatalf("bridgeRuntimeStore(consistent partial) = %v", err)
	}
}
