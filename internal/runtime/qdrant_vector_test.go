package runtime

import (
	"encoding/json"
	"testing"

	"github.com/qdrant/go-client/qdrant"
)

func TestQdrantVectorPayloadPreservesMetadataAndFilters(t *testing.T) {
	payload, err := qdrantVectorPayload(EmbeddingRecord{ID: "a/id", Content: "text", Metadata: json.RawMessage(`{"tag":"one","nested":{"n":1}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if got := payload[qdrantRecordIDKey].GetStringValue(); got != "a/id" {
		t.Fatalf("record ID = %q", got)
	}
	if got := payload[qdrantMetadataKey].GetStringValue(); got != `{"tag":"one","nested":{"n":1}}` {
		t.Fatalf("metadata = %q", got)
	}
	if got := payload[qdrantFilterField("nested")].GetStringValue(); got != `{"n":1}` {
		t.Fatalf("nested filter = %q", got)
	}
}

func TestQdrantVectorFilterCanonicalizesJSON(t *testing.T) {
	filter, err := qdrantVectorFilter(VectorMetadataFilter{Match: map[string]json.RawMessage{
		"tag": json.RawMessage(` "one" `),
		"n":   json.RawMessage(`1`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(filter.GetMust()) != 2 {
		t.Fatalf("must conditions = %d", len(filter.GetMust()))
	}
	conditions := map[string]string{}
	for _, must := range filter.GetMust() {
		condition := must.GetField()
		conditions[condition.GetKey()] = condition.GetMatch().GetKeyword()
	}
	if conditions[qdrantFilterField("tag")] != `"one"` || conditions[qdrantFilterField("n")] != "1" {
		t.Fatalf("conditions = %#v", conditions)
	}
}

func TestQdrantPointIDIsStableUUID(t *testing.T) {
	first, second := qdrantPointID("docs", "an/arbitrary/id"), qdrantPointID("docs", "an/arbitrary/id")
	if first.GetUuid() != second.GetUuid() {
		t.Fatalf("IDs differ: %q %q", first.GetUuid(), second.GetUuid())
	}
	if len(first.GetUuid()) != 36 {
		t.Fatalf("UUID = %q", first.GetUuid())
	}
}

func TestQdrantSimilarityResult(t *testing.T) {
	result, err := qdrantSimilarityResult(&qdrant.ScoredPoint{Score: .9, Payload: map[string]*qdrant.Value{
		qdrantRecordIDKey: qdrant.NewValueString("r1"), qdrantMetadataKey: qdrant.NewValueString(`{"tag":"one"}`), qdrantContentKey: qdrant.NewValueString("content"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "r1" || result.Content != "content" || result.Score != .9 || string(result.Metadata) != `{"tag":"one"}` {
		t.Fatalf("result = %#v", result)
	}
}
