package runtime

import (
	"context"
	"errors"
	"fmt"
)

// FallbackPolicy governs how a router walks alternative providers when the
// primary fails with a retryable error.
type FallbackPolicy struct {
	// Chain is the ordered list of providers to try after the primary fails.
	Chain []ProviderID
	// Retryable overrides the default retryability predicate. When nil,
	// DefaultModelRetryable is used.
	Retryable func(*ModelError) bool
}

// DefaultModelRetryable reports whether a model error is eligible for
// fallback. It delegates to ModelError.Retryable.
func DefaultModelRetryable(err *ModelError) bool {
	if err == nil {
		return false
	}
	return err.Retryable()
}

// Validate checks the invariants a fallback policy must satisfy.
func (f *FallbackPolicy) Validate() error {
	if f == nil {
		return errors.New("lebro: fallback policy is nil")
	}
	if len(f.Chain) == 0 {
		return errors.New("lebro: fallback chain must not be empty")
	}
	seen := make(map[ProviderID]struct{}, len(f.Chain))
	for _, id := range f.Chain {
		if id == "" {
			return errors.New("lebro: fallback chain entry must not be empty")
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("lebro: fallback chain contains duplicate provider %q", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

// IsRetryable reports whether err is eligible for fallback under this policy.
func (f *FallbackPolicy) IsRetryable(err *ModelError) bool {
	if f == nil || f.Retryable == nil {
		return DefaultModelRetryable(err)
	}
	return f.Retryable(err)
}

// Generate walks the fallback chain after the primary provider fails. It
// skips providers that lack the required capabilities and returns the first
// successful response. When the chain is exhausted, the last error is
// returned.
func (f *FallbackPolicy) Generate(ctx context.Context, req ModelRequest, skip ProviderID, registry *ProviderRegistry, reqs ProviderCapabilities) (ModelResponse, error) {
	if f == nil {
		return ModelResponse{}, errors.New("lebro: fallback policy is nil")
	}
	var lastErr error
	for _, id := range f.Chain {
		if id == skip {
			continue
		}
		if err := ctx.Err(); err != nil {
			return ModelResponse{}, err
		}
		entry, err := registry.Get(id)
		if err != nil {
			lastErr = err
			continue
		}
		if !entry.Capabilities.Satisfies(reqs) {
			continue
		}
		resp, genErr := entry.Model.Generate(ctx, req)
		if genErr == nil {
			return resp, nil
		}
		var modelErr *ModelError
		if !errors.As(genErr, &modelErr) || !f.IsRetryable(modelErr) {
			return resp, genErr
		}
		lastErr = genErr
	}
	if lastErr == nil {
		lastErr = errors.New("lebro: fallback chain exhausted with no eligible providers")
	}
	return ModelResponse{}, lastErr
}
