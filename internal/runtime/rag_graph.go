package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// DefaultGraphTraversalDepth and DefaultGraphTraversalResults bound graph
// work when an application does not select tighter limits.
const (
	DefaultGraphTraversalDepth   = 2
	DefaultGraphTraversalResults = 10
)

// GraphNode is a provider-neutral graph result. ID is stable within the graph;
// Content and Metadata are application-defined adapter payloads. Tools expose
// only ID and Content so graph metadata cannot be leaked into model context.
type GraphNode struct {
	ID       string          `json:"id"`
	Content  string          `json:"content"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// Validate checks that a graph node is safe to return through a retrieval
// boundary.
func (n GraphNode) Validate() error {
	if strings.TrimSpace(n.ID) == "" {
		return errors.New("lebro: graph node ID is required")
	}
	if err := validateJSON(n.Metadata); err != nil {
		return fmt.Errorf("lebro: graph node metadata %s", err)
	}
	return nil
}

// GraphTraversal is a bounded graph-store request. Roots are graph node IDs;
// stores must not traverse beyond MaxDepth edges or return more than
// MaxResults nodes. Results must be deterministic for equal graph state and
// query, with a stable node-ID tie break.
type GraphTraversal struct {
	Roots      []string `json:"roots"`
	MaxDepth   int      `json:"max_depth"`
	MaxResults int      `json:"max_results"`
}

// Validate checks that traversal has non-empty roots and explicit positive
// resource bounds.
func (q GraphTraversal) Validate() error {
	if len(q.Roots) == 0 {
		return errors.New("lebro: graph traversal roots are required")
	}
	for _, root := range q.Roots {
		if strings.TrimSpace(root) == "" {
			return errors.New("lebro: graph traversal root must not be empty")
		}
	}
	if q.MaxDepth < 1 {
		return errors.New("lebro: graph traversal max depth must be at least 1")
	}
	if q.MaxResults < 1 {
		return errors.New("lebro: graph traversal max results must be at least 1")
	}
	return nil
}

// GraphStore is an optional graph adapter. Lebro core ships no graph
// persistence implementation: applications can bridge an existing graph
// database while retaining one bounded, provider-neutral retrieval contract.
type GraphStore interface {
	Traverse(context.Context, GraphTraversal) ([]GraphNode, error)
}

// GraphRetrievalQuery describes a model-facing graph lookup. MaxDepth and
// MaxResults are always set by the caller that owns resource policy.
type GraphRetrievalQuery struct {
	Query      string `json:"query"`
	MaxDepth   int    `json:"max_depth"`
	MaxResults int    `json:"max_results"`
}

// Validate checks graph retrieval input and its resource limits.
func (q GraphRetrievalQuery) Validate() error {
	if strings.TrimSpace(q.Query) == "" {
		return errors.New("lebro: graph retrieval query must not be empty")
	}
	if q.MaxDepth < 1 {
		return errors.New("lebro: graph retrieval max depth must be at least 1")
	}
	if q.MaxResults < 1 {
		return errors.New("lebro: graph retrieval max results must be at least 1")
	}
	return nil
}

// GraphRetriever turns a natural-language query into bounded graph results.
// Implementations commonly use a vector retriever to select graph roots, then
// call GraphStore.Traverse. They must respect both query caps and return nodes
// in deterministic relevance order with a node-ID tie break.
type GraphRetriever interface {
	RetrieveGraph(context.Context, GraphRetrievalQuery) ([]GraphNode, error)
}
