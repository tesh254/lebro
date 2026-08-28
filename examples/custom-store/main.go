// The custom-store example attaches a developer-owned storage adapter to an
// agent through the capability-based RuntimeStore contract. The adapter is an
// in-process key/value blob store with a native layout of its own — no Lebro
// tables, no migrations — and it implements only the capabilities it supports:
// thread transcripts and working memory. No transactions are provided, so
// coupled writes run with the documented sequential fallback.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"

	"github.com/tesh254/lebro"
)

// ownStore is the developer's existing storage: here, a namespaced key/value
// blob store with records encoded as JSON documents under their own keys.
// A real adapter would map these onto an existing database, API, event store,
// or document store instead.
type ownStore struct {
	mu     sync.Mutex
	blobs  map[string][]byte
	values map[string][]byte
}

var (
	_ lebro.RuntimeStore       = (*ownStore)(nil)
	_ lebro.TranscriptStore    = (*ownStore)(nil)
	_ lebro.WorkingMemoryStore = (*ownStore)(nil)
)

func newOwnStore() *ownStore {
	return &ownStore{blobs: map[string][]byte{}, values: map[string][]byte{}}
}

// Capabilities advertises exactly what the adapter supports. Lebro validates
// required capabilities against this set and fails setup with a typed error
// when a configured feature needs more.
func (s *ownStore) Capabilities() lebro.StoreCapabilities {
	return lebro.StoreCapabilities{Transcript: true, WorkingMemory: true}
}

// --- transcript capability: the adapter's own layout, not Lebro's schema ---

func (s *ownStore) Threads() lebro.ThreadRepository   { return s }
func (s *ownStore) Messages() lebro.MessageRepository { return s }

func (s *ownStore) CreateThread(_ context.Context, record lebro.ThreadRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := "thread:" + string(record.ID)
	if _, ok := s.blobs[key]; ok {
		return fmt.Errorf("thread %q already exists", record.ID)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	s.blobs[key] = encoded
	return nil
}

func (s *ownStore) GetThread(_ context.Context, id lebro.ThreadID) (lebro.ThreadRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	encoded, ok := s.blobs["thread:"+string(id)]
	if !ok {
		return lebro.ThreadRecord{}, lebro.ErrNotFound
	}
	var record lebro.ThreadRecord
	if err := json.Unmarshal(encoded, &record); err != nil {
		return lebro.ThreadRecord{}, err
	}
	return record, nil
}

func (s *ownStore) UpdateThread(_ context.Context, record lebro.ThreadRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := "thread:" + string(record.ID)
	if _, ok := s.blobs[key]; !ok {
		return lebro.ErrNotFound
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	s.blobs[key] = encoded
	return nil
}

func (s *ownStore) AppendMessages(_ context.Context, records []lebro.MessageRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, record := range records {
		key := fmt.Sprintf("message:%s:%s", record.ThreadID, record.ID)
		if _, ok := s.blobs[key]; ok {
			return fmt.Errorf("message %q already exists", record.ID)
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		s.blobs[key] = encoded
	}
	return nil
}

func (s *ownStore) UpdateMessages(_ context.Context, records []lebro.MessageRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, record := range records {
		key := fmt.Sprintf("message:%s:%s", record.ThreadID, record.ID)
		if _, ok := s.blobs[key]; !ok {
			return lebro.ErrNotFound
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		s.blobs[key] = encoded
	}
	return nil
}

func (s *ownStore) DeleteMessages(_ context.Context, threadID lebro.ThreadID, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		delete(s.blobs, fmt.Sprintf("message:%s:%s", threadID, id))
	}
	return nil
}

func (s *ownStore) ListMessages(_ context.Context, threadID lebro.ThreadID, page lebro.PageRequest) (lebro.Page[lebro.MessageRecord], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := "message:" + string(threadID) + ":"
	keys := make([]string, 0, len(s.blobs))
	for key := range s.blobs {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			keys = append(keys, key)
		}
	}
	// Lexicographic key order keeps the transcript order stable: the runtime
	// names message IDs so ordering by ID matches append order.
	sort.Strings(keys)
	start := 0
	if page.Cursor != "" {
		if _, err := fmt.Sscanf(page.Cursor, "%d", &start); err != nil || start < 0 {
			return lebro.Page[lebro.MessageRecord]{}, lebro.ErrInvalidPage
		}
	}
	limit := page.Limit
	if limit <= 0 {
		limit = len(keys)
	}
	end := min(start+limit, len(keys))
	records := make([]lebro.MessageRecord, 0, end-start)
	for _, key := range keys[start:end] {
		var record lebro.MessageRecord
		if err := json.Unmarshal(s.blobs[key], &record); err != nil {
			return lebro.Page[lebro.MessageRecord]{}, err
		}
		records = append(records, record)
	}
	result := lebro.Page[lebro.MessageRecord]{Records: records}
	if end < len(keys) {
		result.NextCursor = fmt.Sprintf("%d", end)
	}
	return result, nil
}

// --- working memory capability ---

func (s *ownStore) WorkingMemory() lebro.WorkingMemoryRepository { return s }

func (s *ownStore) UpsertWorkingMemoryFact(_ context.Context, fact lebro.WorkingMemoryFact, expectedVersion int64) (lebro.WorkingMemoryFact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fmt.Sprintf("fact:%s:%s:%s", fact.Namespace, fact.OwnerID, fact.Key)
	encoded, ok := s.blobs[key]
	var stored lebro.WorkingMemoryFact
	if ok {
		if err := json.Unmarshal(encoded, &stored); err != nil {
			return lebro.WorkingMemoryFact{}, err
		}
		if stored.Version != expectedVersion {
			return lebro.WorkingMemoryFact{}, lebro.ErrConflict
		}
	} else if expectedVersion != 0 {
		return lebro.WorkingMemoryFact{}, lebro.ErrConflict
	} else {
		stored = fact
		stored.Version = 0
	}
	stored.Value = fact.Value
	stored.Version++
	stored.UpdatedAt = fact.UpdatedAt
	encoded, err := json.Marshal(stored)
	if err != nil {
		return lebro.WorkingMemoryFact{}, err
	}
	s.blobs[key] = encoded
	return stored, nil
}

func (s *ownStore) GetWorkingMemoryFact(_ context.Context, scope lebro.WorkingMemoryScope, factKey string) (lebro.WorkingMemoryFact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadFact(fmt.Sprintf("fact:%s:%s:%s", scope.Namespace, scope.OwnerID, factKey))
}

func (s *ownStore) ListWorkingMemoryFacts(_ context.Context, scope lebro.WorkingMemoryScope, _ lebro.PageRequest) (lebro.Page[lebro.WorkingMemoryFact], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := fmt.Sprintf("fact:%s:%s:", scope.Namespace, scope.OwnerID)
	keys := make([]string, 0)
	for key := range s.blobs {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	records := make([]lebro.WorkingMemoryFact, 0, len(keys))
	for _, key := range keys {
		fact, err := s.loadFact(key)
		if err != nil {
			return lebro.Page[lebro.WorkingMemoryFact]{}, err
		}
		records = append(records, fact)
	}
	return lebro.Page[lebro.WorkingMemoryFact]{Records: records}, nil
}

func (s *ownStore) DeleteWorkingMemoryFact(_ context.Context, scope lebro.WorkingMemoryScope, factKey string, expectedVersion int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fmt.Sprintf("fact:%s:%s:%s", scope.Namespace, scope.OwnerID, factKey)
	stored, err := s.loadFact(key)
	if err != nil {
		return err
	}
	if stored.Version != expectedVersion {
		return lebro.ErrConflict
	}
	delete(s.blobs, key)
	return nil
}

func (s *ownStore) ClearWorkingMemory(_ context.Context, scope lebro.WorkingMemoryScope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := fmt.Sprintf("fact:%s:%s:", scope.Namespace, scope.OwnerID)
	for key := range s.blobs {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			delete(s.blobs, key)
		}
	}
	return nil
}

func (s *ownStore) loadFact(key string) (lebro.WorkingMemoryFact, error) {
	encoded, ok := s.blobs[key]
	if !ok {
		return lebro.WorkingMemoryFact{}, lebro.ErrNotFound
	}
	var fact lebro.WorkingMemoryFact
	if err := json.Unmarshal(encoded, &fact); err != nil {
		return lebro.WorkingMemoryFact{}, err
	}
	return fact, nil
}

// ownedKeys prints the adapter's own key layout, showing that the backing
// store keeps its native shape — Lebro never imposes tables or migrations.
func (s *ownStore) ownedKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.blobs))
	for key := range s.blobs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// fixtureModel is a deterministic stand-in for a provider adapter: one
// scripted step per call, consumed in order. A real deployment supplies
// openai.New or any other lebro.Model instead.
type fixtureModel struct {
	steps []string
	next  int
}

func (m *fixtureModel) Generate(_ context.Context, request lebro.ModelRequest) (lebro.ModelResponse, error) {
	if m.next >= len(m.steps) {
		return lebro.ModelResponse{}, errors.New("fixture model script exhausted")
	}
	content := m.steps[m.next]
	m.next++
	// Echo how much prior context the agent reloaded from the custom store so
	// the example output shows the round-trip.
	return lebro.ModelResponse{
		Message:      lebro.Message{Role: lebro.RoleAssistant, Content: fmt.Sprintf("%s (saw %d prior messages)", content, len(request.Messages))},
		FinishReason: lebro.FinishReasonStop,
	}, nil
}

func main() {
	must(run(os.Stdout))
}

func run(output io.Writer) error {
	ctx := context.Background()
	store := newOwnStore()

	// One fixture step per model call across both runs.
	model := &fixtureModel{steps: []string{
		"Your order ORD-9182 shipped yesterday.",
		"Your order ORD-9182 arrives tomorrow.",
	}}
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition:   lebro.AgentDefinition{ID: "support-agent", Name: "Support", Instructions: "Answer support questions briefly.", Model: "fixture-model"},
		Model:        model,
		RuntimeStore: store,
	})
	if err != nil {
		return err
	}

	const threadID = "support-42"
	if _, err := agent.Run(ctx, lebro.RunInput{
		ThreadID: threadID,
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "Where is my order ORD-9182?"}},
	}); err != nil {
		return err
	}
	if _, err := agent.Run(ctx, lebro.RunInput{
		ThreadID: threadID,
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "And when does it arrive?"}},
	}); err != nil {
		return err
	}

	messages, err := store.Messages().ListMessages(ctx, threadID, lebro.PageRequest{})
	if err != nil {
		return err
	}
	writef(output, "status: succeeded\n")
	writef(output, "thread %s holds %d persisted messages\n", threadID, len(messages.Records))
	for _, message := range messages.Records {
		writef(output, "%s: %s\n", message.Message.Role, message.Message.Content)
	}
	writef(output, "adapter-owned keys:\n")
	for _, key := range store.ownedKeys() {
		writef(output, "  %s\n", key)
	}

	// A store that cannot support a requested feature fails with a typed
	// error instead of silently falling back: this adapter supports only
	// working memory, so a run that asks for thread persistence fails before
	// any model call.
	limited, err := lebro.NewAgent(lebro.AgentConfig{
		Definition:   lebro.AgentDefinition{ID: "memory-only", Model: "fixture-model"},
		Model:        model,
		RuntimeStore: memoryOnlyExampleStore{},
	})
	if err != nil {
		return err
	}
	_, err = limited.Run(ctx, lebro.RunInput{
		ThreadID: "support-43",
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "Does this thread persist?"}},
	})
	var capabilityErr *lebro.StoreCapabilityError
	if !errors.As(err, &capabilityErr) || capabilityErr.Capability != lebro.StoreCapabilityTranscript {
		return fmt.Errorf("expected a transcript StoreCapabilityError, got %v", err)
	}
	writef(output, "capability check: %s\n", capabilityErr)
	return nil
}

// memoryOnlyExampleStore advertises exactly one capability. The repository is
// a stub because the example never reads working memory directly; the point is
// the typed failure when a thread-backed run needs the transcript capability.
type memoryOnlyExampleStore struct{}

var _ lebro.RuntimeStore = memoryOnlyExampleStore{}

func (memoryOnlyExampleStore) Capabilities() lebro.StoreCapabilities {
	return lebro.StoreCapabilities{WorkingMemory: true}
}

func (memoryOnlyExampleStore) WorkingMemory() lebro.WorkingMemoryRepository {
	return memoryOnlyExampleRepo{}
}

type memoryOnlyExampleRepo struct{}

func (memoryOnlyExampleRepo) UpsertWorkingMemoryFact(context.Context, lebro.WorkingMemoryFact, int64) (lebro.WorkingMemoryFact, error) {
	return lebro.WorkingMemoryFact{}, errors.New("working memory is not implemented in this example")
}
func (memoryOnlyExampleRepo) GetWorkingMemoryFact(context.Context, lebro.WorkingMemoryScope, string) (lebro.WorkingMemoryFact, error) {
	return lebro.WorkingMemoryFact{}, errors.New("working memory is not implemented in this example")
}
func (memoryOnlyExampleRepo) ListWorkingMemoryFacts(context.Context, lebro.WorkingMemoryScope, lebro.PageRequest) (lebro.Page[lebro.WorkingMemoryFact], error) {
	return lebro.Page[lebro.WorkingMemoryFact]{}, errors.New("working memory is not implemented in this example")
}
func (memoryOnlyExampleRepo) DeleteWorkingMemoryFact(context.Context, lebro.WorkingMemoryScope, string, int64) error {
	return errors.New("working memory is not implemented in this example")
}
func (memoryOnlyExampleRepo) ClearWorkingMemory(context.Context, lebro.WorkingMemoryScope) error {
	return errors.New("working memory is not implemented in this example")
}

func writef(output io.Writer, format string, args ...any) {
	if _, err := fmt.Fprintf(output, format, args...); err != nil {
		panic(err)
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
