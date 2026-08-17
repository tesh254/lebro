package runtime

import (
	"context"
	"errors"
	"testing"
)

type graphStoreFunc func(context.Context, GraphTraversal) ([]GraphNode, error)

func (f graphStoreFunc) Traverse(ctx context.Context, query GraphTraversal) ([]GraphNode, error) {
	return f(ctx, query)
}

func TestGraphTraversalValidate(t *testing.T) {
	tests := []struct {
		name    string
		query   GraphTraversal
		wantErr bool
	}{
		{name: "valid", query: GraphTraversal{Roots: []string{"chunk-1"}, MaxDepth: 2, MaxResults: 3}},
		{name: "no roots", query: GraphTraversal{MaxDepth: 1, MaxResults: 1}, wantErr: true},
		{name: "empty root", query: GraphTraversal{Roots: []string{" "}, MaxDepth: 1, MaxResults: 1}, wantErr: true},
		{name: "unbounded depth", query: GraphTraversal{Roots: []string{"a"}, MaxResults: 1}, wantErr: true},
		{name: "unbounded results", query: GraphTraversal{Roots: []string{"a"}, MaxDepth: 1}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.query.Validate()
			if (err != nil) != test.wantErr {
				t.Fatalf("Validate() error = %v, want error %t", err, test.wantErr)
			}
		})
	}
}

func TestGraphStoreContractReceivesBoundedTraversal(t *testing.T) {
	var _ GraphStore = graphStoreFunc(func(context.Context, GraphTraversal) ([]GraphNode, error) { return nil, nil })
	store := graphStoreFunc(func(_ context.Context, query GraphTraversal) ([]GraphNode, error) {
		if err := query.Validate(); err != nil {
			return nil, err
		}
		return []GraphNode{{ID: query.Roots[0], Content: "root"}}, nil
	})
	nodes, err := store.Traverse(context.Background(), GraphTraversal{Roots: []string{"seed"}, MaxDepth: 1, MaxResults: 1})
	if err != nil {
		t.Fatalf("Traverse() error = %v", err)
	}
	if len(nodes) != 1 || nodes[0].ID != "seed" {
		t.Fatalf("Traverse() = %+v, want seed node", nodes)
	}
}

func TestGraphRetrievalQueryValidation(t *testing.T) {
	err := (GraphRetrievalQuery{Query: "x", MaxDepth: 1, MaxResults: 1}).Validate()
	if err != nil {
		t.Fatalf("valid query error = %v", err)
	}
	err = (GraphRetrievalQuery{Query: " ", MaxDepth: 1, MaxResults: 1}).Validate()
	if err == nil {
		t.Fatal("empty query error = nil")
	}
	if !errors.Is((&RAGError{Kind: RAGErrorGraphTraversal}), ErrRAGGraphTraversal) {
		t.Fatal("graph traversal RAG error does not match sentinel")
	}
}
