package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/qdrant/go-client/qdrant"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	qdrantMetadataKey  = "__lebro_metadata"
	qdrantContentKey   = "__lebro_content"
	qdrantRecordIDKey  = "__lebro_record_id"
	qdrantFilterPrefix = "__lebro_filter_"
)

// QdrantVectorStoreConfig configures a Qdrant gRPC client. Host and Port use
// Qdrant defaults (localhost and 6334) when zero. PoolSize of zero uses the
// client default. APIKey is sent by Qdrant's supported authentication header.
type QdrantVectorStoreConfig struct {
	Host     string
	Port     int
	APIKey   string
	UseTLS   bool
	PoolSize uint
}

// QdrantVectorStore is a VectorStore backed by Qdrant. Each Lebro index is a
// Qdrant collection with cosine distance. Qdrant accepts UUID or integer point
// IDs only, so Lebro record IDs are deterministically mapped to UUIDs while
// their original values are retained in payload.
type QdrantVectorStore struct {
	client *qdrant.Client
}

// NewQdrantVectorStore creates a Qdrant client. Collection creation remains
// explicit through CreateIndex, matching the VectorStore contract.
func NewQdrantVectorStore(config QdrantVectorStoreConfig) (*QdrantVectorStore, error) {
	client, err := qdrant.NewClient(&qdrant.Config{
		Host: config.Host, Port: config.Port, APIKey: config.APIKey,
		UseTLS: config.UseTLS, PoolSize: config.PoolSize,
	})
	if err != nil {
		return nil, fmt.Errorf("lebro: qdrant vector: connect: %w", err)
	}
	return &QdrantVectorStore{client: client}, nil
}

// Close releases Qdrant gRPC connections.
func (s *QdrantVectorStore) Close() error { return s.client.Close() }

func (s *QdrantVectorStore) CreateIndex(ctx context.Context, index string, dimension int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if index == "" {
		return fmt.Errorf("%w: empty index name", ErrVectorInvalidInput)
	}
	if dimension <= 0 {
		return fmt.Errorf("%w: dimension must be positive", ErrVectorInvalidInput)
	}
	exists, err := s.client.CollectionExists(ctx, index)
	if err != nil {
		return qdrantVectorError("check index", index, err)
	}
	if exists {
		return fmt.Errorf("%w: index %q", ErrVectorAlreadyExists, index)
	}
	err = s.client.CreateCollection(ctx, &qdrant.CreateCollection{
		CollectionName: index,
		VectorsConfig:  qdrant.NewVectorsConfig(&qdrant.VectorParams{Size: uint64(dimension), Distance: qdrant.Distance_Cosine}),
	})
	if status.Code(err) == codes.AlreadyExists {
		return fmt.Errorf("%w: index %q", ErrVectorAlreadyExists, index)
	}
	if err != nil {
		return qdrantVectorError("create index", index, err)
	}
	return nil
}

func (s *QdrantVectorStore) DeleteIndex(ctx context.Context, index string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if index == "" {
		return fmt.Errorf("%w: empty index name", ErrVectorInvalidInput)
	}
	exists, err := s.client.CollectionExists(ctx, index)
	if err != nil {
		return qdrantVectorError("check index", index, err)
	}
	if !exists {
		return fmt.Errorf("%w: index %q", ErrVectorNotFound, index)
	}
	if err := s.client.DeleteCollection(ctx, index); err != nil {
		return qdrantVectorError("delete index", index, err)
	}
	return nil
}

func (s *QdrantVectorStore) Upsert(ctx context.Context, records []EmbeddingRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(records) == 0 {
		return nil
	}
	byIndex := make(map[string][]*qdrant.PointStruct)
	dimensions := make(map[string]int)
	for _, record := range records {
		if err := validateVectorRecord(record); err != nil {
			return err
		}
		dimension, ok := dimensions[record.Index]
		if !ok {
			var err error
			dimension, err = s.indexDimension(ctx, record.Index)
			if err != nil {
				return err
			}
			dimensions[record.Index] = dimension
		}
		if len(record.Vector) != dimension {
			return fmt.Errorf("%w: record %q has dimension %d, index %q expects %d", ErrVectorInvalidDimension, record.ID, len(record.Vector), record.Index, dimension)
		}
		payload, err := qdrantVectorPayload(record)
		if err != nil {
			return err
		}
		byIndex[record.Index] = append(byIndex[record.Index], &qdrant.PointStruct{Id: qdrantPointID(record.Index, record.ID), Vectors: qdrant.NewVectorsDense(record.Vector), Payload: payload})
	}
	wait := true
	for index, points := range byIndex {
		if _, err := s.client.Upsert(ctx, &qdrant.UpsertPoints{CollectionName: index, Points: points, Wait: &wait}); err != nil {
			return qdrantVectorError("upsert records", index, err)
		}
	}
	return nil
}

func (s *QdrantVectorStore) Delete(ctx context.Context, index string, ids []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if index == "" {
		return fmt.Errorf("%w: empty index name", ErrVectorInvalidInput)
	}
	if _, err := s.indexDimension(ctx, index); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	pointIDs := make([]*qdrant.PointId, 0, len(ids))
	for _, id := range ids {
		pointIDs = append(pointIDs, qdrantPointID(index, id))
	}
	wait := true
	_, err := s.client.Delete(ctx, &qdrant.DeletePoints{CollectionName: index, Wait: &wait, Points: &qdrant.PointsSelector{PointsSelectorOneOf: &qdrant.PointsSelector_Points{Points: &qdrant.PointsIdsList{Ids: pointIDs}}}})
	if err != nil {
		return qdrantVectorError("delete records", index, err)
	}
	return nil
}

func (s *QdrantVectorStore) Search(ctx context.Context, query SimilarityQuery) ([]SimilarityResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateSimilarityQuery(query); err != nil {
		return nil, err
	}
	dimension, err := s.indexDimension(ctx, query.Index)
	if err != nil {
		return nil, err
	}
	if len(query.Vector) != dimension {
		return nil, fmt.Errorf("%w: query vector has dimension %d, index %q expects %d", ErrVectorInvalidDimension, len(query.Vector), query.Index, dimension)
	}
	filter, err := qdrantVectorFilter(query.Filter)
	if err != nil {
		return nil, err
	}
	limit := uint64(query.TopK)
	request := &qdrant.QueryPoints{CollectionName: query.Index, Query: qdrant.NewQueryDense(query.Vector), Filter: filter, Limit: &limit, WithPayload: qdrant.NewWithPayload(true)}
	// rankResults is contract source of truth; Qdrant threshold only avoids
	// returning results that rankResults would remove.
	if query.MinScore > 0 {
		request.ScoreThreshold = &query.MinScore
	}
	points, err := s.client.Query(ctx, request)
	if err != nil {
		return nil, qdrantVectorError("search", query.Index, err)
	}
	results := make([]SimilarityResult, 0, len(points))
	for _, point := range points {
		result, err := qdrantSimilarityResult(point)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return rankResults(results, query.TopK, query.MinScore), nil
}

func (s *QdrantVectorStore) indexDimension(ctx context.Context, index string) (int, error) {
	info, err := s.client.GetCollectionInfo(ctx, index)
	if status.Code(err) == codes.NotFound {
		return 0, fmt.Errorf("%w: index %q", ErrVectorNotFound, index)
	}
	if err != nil {
		return 0, qdrantVectorError("get index", index, err)
	}
	params := info.GetConfig().GetParams().GetVectorsConfig().GetParams()
	if params == nil || params.GetSize() == 0 || params.GetSize() > uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("lebro: qdrant vector: index %q must use one unnamed vector with a supported dimension", index)
	}
	return int(params.GetSize()), nil
}

func qdrantVectorPayload(record EmbeddingRecord) (map[string]*qdrant.Value, error) {
	payload := map[string]any{qdrantRecordIDKey: record.ID, qdrantContentKey: record.Content, qdrantMetadataKey: string(record.Metadata)}
	if len(record.Metadata) > 0 {
		var metadata map[string]json.RawMessage
		if err := json.Unmarshal(record.Metadata, &metadata); err == nil {
			for key, value := range metadata {
				canonical, err := qdrantCanonicalJSON(value)
				if err != nil {
					return nil, fmt.Errorf("%w: metadata %q: %v", ErrVectorInvalidInput, key, err)
				}
				payload[qdrantFilterField(key)] = canonical
			}
		}
	}
	values, err := qdrant.TryValueMap(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: metadata payload: %v", ErrVectorInvalidInput, err)
	}
	return values, nil
}

func qdrantVectorFilter(filter VectorMetadataFilter) (*qdrant.Filter, error) {
	if len(filter.Match) == 0 {
		return nil, nil
	}
	must := make([]*qdrant.Condition, 0, len(filter.Match))
	for key, value := range filter.Match {
		canonical, err := qdrantCanonicalJSON(value)
		if err != nil {
			return nil, fmt.Errorf("%w: metadata filter %q: %v", ErrVectorInvalidInput, key, err)
		}
		must = append(must, qdrant.NewMatch(qdrantFilterField(key), canonical))
	}
	return &qdrant.Filter{Must: must}, nil
}

// qdrantCanonicalJSON must be used for both payload values and filters so
// Qdrant keyword matching preserves VectorMetadataFilter's JSON equality.
func qdrantCanonicalJSON(raw json.RawMessage) (string, error) {
	value, err := decodeJSONNumber(raw)
	if err != nil {
		return "", err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(canonical), nil
}

func qdrantSimilarityResult(point *qdrant.ScoredPoint) (SimilarityResult, error) {
	payload := point.GetPayload()
	id := payload[qdrantRecordIDKey].GetStringValue()
	if id == "" {
		return SimilarityResult{}, errors.New("lebro: qdrant vector: search result missing record ID payload")
	}
	metadata := json.RawMessage(payload[qdrantMetadataKey].GetStringValue())
	if len(metadata) > 0 && !json.Valid(metadata) {
		return SimilarityResult{}, fmt.Errorf("lebro: qdrant vector: record %q has invalid metadata payload", id)
	}
	return SimilarityResult{ID: id, Score: point.GetScore(), Metadata: cloneJSON(metadata), Content: payload[qdrantContentKey].GetStringValue()}, nil
}

func qdrantPointID(index, id string) *qdrant.PointId {
	sum := sha256.Sum256([]byte(index + "\x00" + id))
	encoded := hex.EncodeToString(sum[:])
	// Replace UUID version and variant nibbles in the 8-4-4-4-12 layout.
	uuid := encoded[:8] + "-" + encoded[8:12] + "-5" + encoded[13:16] + "-8" + encoded[17:20] + "-" + encoded[20:32]
	return qdrant.NewID(uuid)
}

func qdrantFilterField(key string) string {
	sum := sha256.Sum256([]byte(key))
	return qdrantFilterPrefix + hex.EncodeToString(sum[:])
}

func qdrantVectorError(operation, index string, err error) error {
	switch status.Code(err) {
	case codes.NotFound:
		return fmt.Errorf("%w: index %q", ErrVectorNotFound, index)
	case codes.Canceled:
		return context.Canceled
	case codes.DeadlineExceeded:
		return context.DeadlineExceeded
	case codes.InvalidArgument:
		return fmt.Errorf("%w: %s index %q: %v", ErrVectorInvalidInput, operation, index, err)
	default:
		return fmt.Errorf("lebro: qdrant vector: %s index %q: %w", operation, index, err)
	}
}
