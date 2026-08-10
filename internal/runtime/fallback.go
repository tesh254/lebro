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
// skips providers that lack the required capabilities or are not in the
// eligible set. Returns the first successful response or the last error.
func (f *FallbackPolicy) Generate(ctx context.Context, req ModelRequest, skip ProviderID, registry *ProviderRegistry, reqs ProviderCapabilities) (ModelResponse, error) {
	result, err := f.GenerateWithAttempts(ctx, req, skip, registry, reqs, nil, nil)
	if err != nil {
		return ModelResponse{}, err
	}
	return result.Response, nil
}

// GenerateWithAttempts walks the fallback chain and returns all attempts.
func (f *FallbackPolicy) GenerateWithAttempts(ctx context.Context, req ModelRequest, skip ProviderID, registry *ProviderRegistry, reqs ProviderCapabilities, eligible []ProviderID, priorAttempts []ModelAttempt) (RouteResult, error) {
	if f == nil {
		return RouteResult{}, errors.New("lebro: fallback policy is nil")
	}

	eligibleSet := make(map[ProviderID]struct{}, len(eligible))
	for _, id := range eligible {
		eligibleSet[id] = struct{}{}
	}

	attempts := append([]ModelAttempt(nil), priorAttempts...)
	var lastErr error

	for _, id := range f.Chain {
		if id == skip {
			continue
		}
		// Filter against eligible set if specified.
		if len(eligibleSet) > 0 {
			if _, ok := eligibleSet[id]; !ok {
				continue
			}
		}
		if err := ctx.Err(); err != nil {
			return RouteResult{Attempts: attempts}, err
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
			attempts = append(attempts, ModelAttempt{Provider: id, Model: req.Model, Status: ModelAttemptSuccess})
			return RouteResult{Response: resp, Attempts: attempts}, nil
		}
		var modelErr *ModelError
		if !errors.As(genErr, &modelErr) {
			attempts = append(attempts, ModelAttempt{Provider: id, Model: req.Model, Status: ModelAttemptFailed, Error: toModelError(genErr)})
			return RouteResult{Attempts: attempts}, genErr
		}
		attempts = append(attempts, ModelAttempt{Provider: id, Model: req.Model, Status: ModelAttemptFallback, Error: modelErr})
		if !f.IsRetryable(modelErr) {
			return RouteResult{Attempts: attempts}, genErr
		}
		lastErr = genErr
	}

	if lastErr == nil {
		lastErr = errors.New("lebro: fallback chain exhausted with no eligible providers")
	}
	return RouteResult{Attempts: attempts}, lastErr
}

// Stream walks the fallback chain for streaming requests.
func (f *FallbackPolicy) Stream(ctx context.Context, req ModelRequest, skip ProviderID, registry *ProviderRegistry, reqs ProviderCapabilities) (StreamReader, error) {
	result, err := f.StreamWithAttempts(ctx, req, skip, registry, reqs, nil, nil)
	if err != nil {
		return nil, err
	}
	return result.Reader, nil
}

// StreamWithAttempts walks the fallback chain for streaming and returns all attempts.
func (f *FallbackPolicy) StreamWithAttempts(ctx context.Context, req ModelRequest, skip ProviderID, registry *ProviderRegistry, reqs ProviderCapabilities, eligible []ProviderID, priorAttempts []ModelAttempt) (StreamRouteResult, error) {
	if f == nil {
		return StreamRouteResult{}, errors.New("lebro: fallback policy is nil")
	}

	eligibleSet := make(map[ProviderID]struct{}, len(eligible))
	for _, id := range eligible {
		eligibleSet[id] = struct{}{}
	}

	attempts := append([]ModelAttempt(nil), priorAttempts...)
	var lastErr error

	for _, id := range f.Chain {
		if id == skip {
			continue
		}
		if len(eligibleSet) > 0 {
			if _, ok := eligibleSet[id]; !ok {
				continue
			}
		}
		if err := ctx.Err(); err != nil {
			return StreamRouteResult{Attempts: attempts}, err
		}
		entry, err := registry.Get(id)
		if err != nil {
			lastErr = err
			continue
		}
		if !entry.Capabilities.Satisfies(reqs) {
			continue
		}

		streamingModel := AsStreamingModel(entry.Model)
		if streamingModel != nil {
			reader, streamErr := streamingModel.Stream(ctx, req)
			if streamErr == nil {
				attempts = append(attempts, ModelAttempt{Provider: id, Model: req.Model, Status: ModelAttemptSuccess})
				return StreamRouteResult{Reader: reader, Attempts: attempts}, nil
			}
			var modelErr *ModelError
			if !errors.As(streamErr, &modelErr) {
				attempts = append(attempts, ModelAttempt{Provider: id, Model: req.Model, Status: ModelAttemptFailed, Error: toModelError(streamErr)})
				return StreamRouteResult{Attempts: attempts}, streamErr
			}
			attempts = append(attempts, ModelAttempt{Provider: id, Model: req.Model, Status: ModelAttemptFallback, Error: modelErr})
			if !f.IsRetryable(modelErr) {
				return StreamRouteResult{Attempts: attempts}, streamErr
			}
			lastErr = streamErr
			continue
		}

		// Fallback to Generate for non-streaming providers.
		resp, genErr := entry.Model.Generate(ctx, req)
		if genErr == nil {
			attempts = append(attempts, ModelAttempt{Provider: id, Model: req.Model, Status: ModelAttemptSuccess})
			reader := responseToStreamReader(resp)
			return StreamRouteResult{Reader: reader, Attempts: attempts}, nil
		}
		var modelErr *ModelError
		if !errors.As(genErr, &modelErr) {
			attempts = append(attempts, ModelAttempt{Provider: id, Model: req.Model, Status: ModelAttemptFailed, Error: toModelError(genErr)})
			return StreamRouteResult{Attempts: attempts}, genErr
		}
		attempts = append(attempts, ModelAttempt{Provider: id, Model: req.Model, Status: ModelAttemptFallback, Error: modelErr})
		if !f.IsRetryable(modelErr) {
			return StreamRouteResult{Attempts: attempts}, genErr
		}
		lastErr = genErr
	}

	if lastErr == nil {
		lastErr = errors.New("lebro: fallback chain exhausted with no eligible providers")
	}
	return StreamRouteResult{Attempts: attempts}, lastErr
}
