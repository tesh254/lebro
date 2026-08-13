package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const DefaultThreadHistoryTopK = 5

type ThreadHistoryConfig struct {
	Store      Store
	Vectors    VectorStore
	Embeddings EmbeddingModel
	Index      string
}

// ThreadHistory couples durable thread messages with a semantic index.
type ThreadHistory struct {
	store      Store
	vectors    VectorStore
	embeddings EmbeddingModel
	index      string
}

func NewThreadHistory(config ThreadHistoryConfig) (*ThreadHistory, error) {
	if config.Store == nil || isNilInterface(config.Store) {
		return nil, errors.New("lebro: thread history store is required")
	}
	if config.Vectors == nil || isNilInterface(config.Vectors) {
		return nil, errors.New("lebro: thread history vector store is required")
	}
	if config.Embeddings == nil || isNilInterface(config.Embeddings) {
		return nil, errors.New("lebro: thread history embedding model is required")
	}
	if config.Index == "" {
		return nil, errors.New("lebro: thread history index name is required")
	}
	if config.Embeddings.Dimension() <= 0 {
		return nil, errors.New("lebro: thread history embedding dimension must be positive")
	}
	return &ThreadHistory{store: config.Store, vectors: config.Vectors, embeddings: config.Embeddings, index: config.Index}, nil
}
func (h *ThreadHistory) EnsureIndex(ctx context.Context) error {
	if h == nil || ctx == nil {
		return errors.New("lebro: thread history and context are required")
	}
	err := h.vectors.CreateIndex(ctx, h.index, h.embeddings.Dimension())
	if err == nil || errors.Is(err, ErrVectorAlreadyExists) {
		return nil
	}
	return &RAGError{Kind: RAGErrorIndexing, Err: err}
}
func (h *ThreadHistory) AppendMessages(ctx context.Context, messages []MessageRecord) error {
	if h == nil {
		return errors.New("lebro: thread history is nil")
	}
	if ctx == nil {
		return errors.New("lebro: thread history context is nil")
	}
	if err := h.store.Messages().AppendMessages(ctx, messages); err != nil {
		return err
	}
	return h.indexMessages(ctx, messages)
}
func (h *ThreadHistory) UpdateMessages(ctx context.Context, messages []MessageRecord) error {
	if h == nil {
		return errors.New("lebro: thread history is nil")
	}
	if ctx == nil {
		return errors.New("lebro: thread history context is nil")
	}
	updated, err := h.withStoredCreatedAt(ctx, messages)
	if err != nil {
		return err
	}
	if err := h.store.Messages().UpdateMessages(ctx, updated); err != nil {
		return err
	}
	if err := h.deleteVectors(ctx, updated); err != nil {
		return err
	}
	return h.indexMessages(ctx, updated)
}
func (h *ThreadHistory) DeleteMessages(ctx context.Context, threadID ThreadID, messageIDs []string) error {
	if h == nil {
		return errors.New("lebro: thread history is nil")
	}
	if ctx == nil {
		return errors.New("lebro: thread history context is nil")
	}
	items := make([]MessageRecord, len(messageIDs))
	for i, id := range messageIDs {
		items[i] = MessageRecord{ID: id, ThreadID: threadID}
	}
	if err := h.store.Messages().DeleteMessages(ctx, threadID, messageIDs); err != nil {
		return err
	}
	return h.deleteVectors(ctx, items)
}
func (h *ThreadHistory) IndexThread(ctx context.Context, id ThreadID) error {
	if h == nil {
		return errors.New("lebro: thread history is nil")
	}
	if ctx == nil {
		return errors.New("lebro: thread history context is nil")
	}
	var all []MessageRecord
	page := PageRequest{}
	for {
		got, err := h.store.Messages().ListMessages(ctx, id, page)
		if err != nil {
			return err
		}
		all = append(all, got.Records...)
		if got.NextCursor == "" {
			break
		}
		page.Cursor = got.NextCursor
	}
	return h.indexMessages(ctx, all)
}
func (h *ThreadHistory) indexMessages(ctx context.Context, messages []MessageRecord) error {
	if len(messages) == 0 {
		return nil
	}
	threads := map[ThreadID]ThreadRecord{}
	inputs := make([]string, 0, len(messages))
	kept := make([]MessageRecord, 0, len(messages))
	for _, message := range messages {
		if message.Message.Content == "" {
			continue
		}
		if _, ok := threads[message.ThreadID]; !ok {
			thread, err := h.store.Threads().GetThread(ctx, message.ThreadID)
			if err != nil {
				return err
			}
			threads[message.ThreadID] = thread
		}
		inputs, kept = append(inputs, message.Message.Content), append(kept, message)
	}
	if len(kept) == 0 {
		return nil
	}
	vectors, err := h.embeddings.Embed(ctx, inputs)
	if err != nil {
		return &RAGError{Kind: RAGErrorEmbedding, Err: err}
	}
	if len(vectors) != len(kept) {
		return &RAGError{Kind: RAGErrorEmbedding, Err: fmt.Errorf("lebro: embedding model returned %d vectors for %d inputs", len(vectors), len(kept))}
	}
	records := make([]EmbeddingRecord, len(kept))
	for i, message := range kept {
		if len(vectors[i]) != h.embeddings.Dimension() {
			return &RAGError{Kind: RAGErrorEmbedding, Err: errors.New("lebro: invalid message embedding dimension")}
		}
		metadata, err := threadHistoryMetadata(threads[message.ThreadID], message)
		if err != nil {
			return err
		}
		records[i] = EmbeddingRecord{ID: threadHistoryVectorID(message.ThreadID, message.ID), Index: h.index, Vector: append([]float32(nil), vectors[i]...), Metadata: metadata, Content: message.Message.Content}
	}
	if err := h.vectors.Upsert(ctx, records); err != nil {
		return &RAGError{Kind: RAGErrorIndexing, Err: err}
	}
	return nil
}

// withStoredCreatedAt preserves immutable message timestamps before an update
// reaches an adapter. It also verifies every target exists before mutations.
func (h *ThreadHistory) withStoredCreatedAt(ctx context.Context, messages []MessageRecord) ([]MessageRecord, error) {
	updated := append([]MessageRecord(nil), messages...)
	byThread := make(map[ThreadID]map[string]time.Time, len(messages))
	for _, message := range messages {
		if message.ID == "" || message.ThreadID == "" {
			return nil, errors.New("lebro: message and thread IDs are required")
		}
		if _, ok := byThread[message.ThreadID]; !ok {
			byThread[message.ThreadID] = map[string]time.Time{}
			page := PageRequest{}
			for {
				got, err := h.store.Messages().ListMessages(ctx, message.ThreadID, page)
				if err != nil {
					return nil, err
				}
				for _, stored := range got.Records {
					byThread[message.ThreadID][stored.ID] = stored.CreatedAt
				}
				if got.NextCursor == "" {
					break
				}
				page.Cursor = got.NextCursor
			}
		}
		if _, ok := byThread[message.ThreadID][message.ID]; !ok {
			return nil, ErrNotFound
		}
	}
	for i := range updated {
		updated[i].CreatedAt = byThread[updated[i].ThreadID][updated[i].ID]
	}
	return updated, nil
}
func (h *ThreadHistory) deleteVectors(ctx context.Context, messages []MessageRecord) error {
	if len(messages) == 0 {
		return nil
	}
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		if message.ID == "" || message.ThreadID == "" {
			return errors.New("lebro: message and thread IDs are required")
		}
		ids = append(ids, threadHistoryVectorID(message.ThreadID, message.ID))
	}
	if err := h.vectors.Delete(ctx, h.index, ids); err != nil {
		return &RAGError{Kind: RAGErrorIndexing, Err: err}
	}
	return nil
}

type ThreadHistoryScope struct{ Namespace, OwnerID string }
type ThreadHistoryQuery struct {
	Scope     ThreadHistoryScope
	ThreadID  ThreadID
	Query     string
	TopK      int
	MaxTokens int
	Filter    VectorMetadataFilter
}
type ThreadHistoryHit struct {
	MessageID string    `json:"message_id"`
	ThreadID  ThreadID  `json:"thread_id"`
	Role      Role      `json:"role"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
	Score     float32   `json:"score"`
}

func (h *ThreadHistory) Retrieve(ctx context.Context, query ThreadHistoryQuery) ([]ThreadHistoryHit, error) {
	if h == nil {
		return nil, errors.New("lebro: thread history is nil")
	}
	if ctx == nil {
		return nil, errors.New("lebro: thread history context is nil")
	}
	if query.Scope.Namespace == "" || query.Scope.OwnerID == "" {
		return nil, errors.New("lebro: thread history scope is required")
	}
	if strings.TrimSpace(query.Query) == "" {
		return nil, errors.New("lebro: thread history query is required")
	}
	if query.TopK < 0 || query.MaxTokens < 0 {
		return nil, errors.New("lebro: thread history limits must not be negative")
	}
	topK := query.TopK
	if topK == 0 {
		topK = DefaultThreadHistoryTopK
	}
	vectors, err := h.embeddings.Embed(ctx, []string{query.Query})
	if err != nil {
		return nil, &RAGError{Kind: RAGErrorEmbedding, Err: err}
	}
	if len(vectors) != 1 || len(vectors[0]) != h.embeddings.Dimension() {
		return nil, &RAGError{Kind: RAGErrorEmbedding, Err: errors.New("lebro: invalid query embedding")}
	}
	filter := mergeVectorMetadataFilters(query.Filter, threadHistoryScopeFilter(query.Scope, query.ThreadID))
	results, err := h.vectors.Search(ctx, SimilarityQuery{Vector: vectors[0], Index: h.index, Filter: filter, TopK: topK})
	if err != nil {
		return nil, &RAGError{Kind: RAGErrorRetrieval, Err: err}
	}
	hits := make([]ThreadHistoryHit, 0, len(results))
	for _, result := range results {
		hit, err := threadHistoryHit(result)
		if err != nil {
			return nil, &RAGError{Kind: RAGErrorRetrieval, Err: err}
		}
		hit.Score = result.Score
		hits = append(hits, hit)
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].CreatedAt.After(hits[j].CreatedAt)
	})
	trimmed, tokens := make([]ThreadHistoryHit, 0, len(hits)), 0
	for _, hit := range hits {
		need := (len([]rune(hit.Content)) + 3) / 4
		if query.MaxTokens > 0 && tokens+need > query.MaxTokens {
			continue
		}
		trimmed, tokens = append(trimmed, hit), tokens+need
		if len(trimmed) == topK {
			break
		}
	}
	return trimmed, nil
}
func threadHistoryVectorID(threadID ThreadID, messageID string) string {
	return "thread-history:" + base64.RawURLEncoding.EncodeToString([]byte(threadID)) + ":" + base64.RawURLEncoding.EncodeToString([]byte(messageID))
}
func threadHistoryMetadata(thread ThreadRecord, message MessageRecord) (json.RawMessage, error) {
	value := map[string]any{"thread_id": message.ThreadID, "message_id": message.ID, "namespace": thread.Namespace, "owner_id": thread.OwnerID, "role": message.Message.Role, "created_at": message.CreatedAt.UTC().Format(time.RFC3339Nano)}
	if len(thread.Metadata) > 0 {
		var metadata any
		if err := json.Unmarshal(thread.Metadata, &metadata); err != nil {
			return nil, err
		}
		value["thread_metadata"] = metadata
	}
	if len(message.Metadata) > 0 {
		var metadata any
		if err := json.Unmarshal(message.Metadata, &metadata); err != nil {
			return nil, err
		}
		value["message_metadata"] = metadata
	}
	return json.Marshal(value)
}
func threadHistoryScopeFilter(scope ThreadHistoryScope, threadID ThreadID) VectorMetadataFilter {
	namespace, _ := json.Marshal(scope.Namespace)
	ownerID, _ := json.Marshal(scope.OwnerID)
	match := map[string]json.RawMessage{"namespace": namespace, "owner_id": ownerID}
	if threadID != "" {
		encoded, _ := json.Marshal(threadID)
		match["thread_id"] = encoded
	}
	return VectorMetadataFilter{Match: match}
}
func threadHistoryHit(result SimilarityResult) (ThreadHistoryHit, error) {
	var metadata struct {
		MessageID string   `json:"message_id"`
		ThreadID  ThreadID `json:"thread_id"`
		Role      Role     `json:"role"`
		CreatedAt string   `json:"created_at"`
	}
	if err := json.Unmarshal(result.Metadata, &metadata); err != nil {
		return ThreadHistoryHit{}, err
	}
	if metadata.MessageID == "" || metadata.ThreadID == "" || metadata.CreatedAt == "" {
		return ThreadHistoryHit{}, errors.New("lebro: incomplete thread history metadata")
	}
	created, err := time.Parse(time.RFC3339Nano, metadata.CreatedAt)
	if err != nil {
		return ThreadHistoryHit{}, err
	}
	return ThreadHistoryHit{MessageID: metadata.MessageID, ThreadID: metadata.ThreadID, Role: metadata.Role, Content: result.Content, CreatedAt: created}, nil
}
