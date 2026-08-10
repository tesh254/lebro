package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// RoutingPolicy decides which provider handles a given request. When multiple
// routing strategies are needed, callers compose them into a single policy.
type RoutingPolicy struct {
	// Primary is the preferred provider. When empty, the router selects the
	// first eligible provider from the registry.
	Primary ProviderID
	// Eligible restricts routing to these providers when non-empty. An empty
	// slice means all registered providers are eligible.
	Eligible []ProviderID
	// Predicate overrides Primary and Eligible when set. It receives the
	// request and returns the target provider ID. When the predicate returns
	// an empty ID, the router falls back to Primary/Eligible resolution.
	Predicate func(ModelRequest) ProviderID
}

// ModelRouter implements Model and StreamingModel by routing requests through
// a provider registry according to a routing policy and optional fallback
// chain. It is safe for concurrent use.
type ModelRouter struct {
	registry *ProviderRegistry
	policy   RoutingPolicy
	fallback *FallbackPolicy
}

// ModelRouterConfig describes a ModelRouter. Registry is required.
type ModelRouterConfig struct {
	Registry *ProviderRegistry
	Policy   RoutingPolicy
	Fallback *FallbackPolicy
}

// NewModelRouter validates the configuration and returns a router that
// implements the Model and StreamingModel interfaces.
func NewModelRouter(config ModelRouterConfig) (*ModelRouter, error) {
	if config.Registry == nil {
		return nil, errors.New("lebro: model router requires a provider registry")
	}
	if config.Registry.Len() == 0 {
		return nil, errors.New("lebro: model router registry is empty")
	}
	if config.Fallback != nil {
		if err := config.Fallback.Validate(); err != nil {
			return nil, fmt.Errorf("lebro: model router fallback: %w", err)
		}
	}
	return &ModelRouter{
		registry: config.Registry,
		policy:   config.Policy,
		fallback: config.Fallback,
	}, nil
}

var _ Model = (*ModelRouter)(nil)
var _ StreamingModel = (*ModelRouter)(nil)

// RouteResult captures the outcome of routing a single request, including all
// provider attempts. It is returned by GenerateWithAttempts and StreamWithAttempts
// for observability.
type RouteResult struct {
	Response ModelResponse
	Attempts []ModelAttempt
}

// StreamRouteResult captures the outcome of routing a streaming request.
type StreamRouteResult struct {
	Reader   StreamReader
	Attempts []ModelAttempt
}

// Generate routes the request to the appropriate provider and delegates the
// call. When a fallback policy is configured and the primary provider fails
// with a retryable error, the router walks the fallback chain.
func (r *ModelRouter) Generate(ctx context.Context, req ModelRequest) (ModelResponse, error) {
	result, err := r.GenerateWithAttempts(ctx, req)
	if err != nil {
		return ModelResponse{}, err
	}
	return result.Response, nil
}

// GenerateWithAttempts routes the request and returns all provider attempts
// for observability.
func (r *ModelRouter) GenerateWithAttempts(ctx context.Context, req ModelRequest) (RouteResult, error) {
	if r == nil {
		return RouteResult{}, errors.New("lebro: model router is nil")
	}

	reqs := requestRequirements(req)
	target, entry, err := r.resolveTargetWithEntry(req, reqs)
	if err != nil {
		return RouteResult{}, err
	}

	var attempts []ModelAttempt

	resp, genErr := entry.Model.Generate(ctx, req)
	if genErr == nil {
		attempts = append(attempts, ModelAttempt{Provider: target, Model: req.Model, Status: ModelAttemptSuccess})
		return RouteResult{Response: resp, Attempts: attempts}, nil
	}

	if r.fallback == nil {
		attempts = append(attempts, ModelAttempt{Provider: target, Model: req.Model, Status: ModelAttemptFailed, Error: toModelError(genErr)})
		return RouteResult{Attempts: attempts}, genErr
	}

	var modelErr *ModelError
	if !errors.As(genErr, &modelErr) || !r.fallback.IsRetryable(modelErr) {
		attempts = append(attempts, ModelAttempt{Provider: target, Model: req.Model, Status: ModelAttemptFailed, Error: toModelError(genErr)})
		return RouteResult{Attempts: attempts}, genErr
	}

	attempts = append(attempts, ModelAttempt{Provider: target, Model: req.Model, Status: ModelAttemptFallback, Error: modelErr})
	return r.fallback.GenerateWithAttempts(ctx, req, target, r.registry, reqs, r.policy.Eligible, attempts)
}

// Stream routes the streaming request to the appropriate provider. When the
// resolved provider implements StreamingModel, the stream is returned
// directly. Otherwise, Generate is used and the response is wrapped in a
// single-delta reader for compatibility.
func (r *ModelRouter) Stream(ctx context.Context, req ModelRequest) (StreamReader, error) {
	result, err := r.StreamWithAttempts(ctx, req)
	if err != nil {
		return nil, err
	}
	return result.Reader, nil
}

// StreamWithAttempts routes the streaming request and returns all provider
// attempts for observability.
func (r *ModelRouter) StreamWithAttempts(ctx context.Context, req ModelRequest) (StreamRouteResult, error) {
	if r == nil {
		return StreamRouteResult{}, errors.New("lebro: model router is nil")
	}

	reqs := requestRequirements(req)
	target, entry, err := r.resolveTargetWithEntry(req, reqs)
	if err != nil {
		return StreamRouteResult{}, err
	}

	var attempts []ModelAttempt

	streamingModel := AsStreamingModel(entry.Model)
	if streamingModel != nil {
		reader, streamErr := streamingModel.Stream(ctx, req)
		if streamErr == nil {
			attempts = append(attempts, ModelAttempt{Provider: target, Model: req.Model, Status: ModelAttemptSuccess})
			return StreamRouteResult{Reader: reader, Attempts: attempts}, nil
		}

		if r.fallback == nil {
			attempts = append(attempts, ModelAttempt{Provider: target, Model: req.Model, Status: ModelAttemptFailed, Error: toModelError(streamErr)})
			return StreamRouteResult{Attempts: attempts}, streamErr
		}

		var modelErr *ModelError
		if !errors.As(streamErr, &modelErr) || !r.fallback.IsRetryable(modelErr) {
			attempts = append(attempts, ModelAttempt{Provider: target, Model: req.Model, Status: ModelAttemptFailed, Error: toModelError(streamErr)})
			return StreamRouteResult{Attempts: attempts}, streamErr
		}

		attempts = append(attempts, ModelAttempt{Provider: target, Model: req.Model, Status: ModelAttemptFallback, Error: modelErr})
		return r.fallback.StreamWithAttempts(ctx, req, target, r.registry, reqs, r.policy.Eligible, attempts)
	}

	// Fallback to Generate for non-streaming providers.
	resp, genErr := entry.Model.Generate(ctx, req)
	if genErr == nil {
		attempts = append(attempts, ModelAttempt{Provider: target, Model: req.Model, Status: ModelAttemptSuccess})
		reader := responseToStreamReader(resp)
		return StreamRouteResult{Reader: reader, Attempts: attempts}, nil
	}

	if r.fallback == nil {
		attempts = append(attempts, ModelAttempt{Provider: target, Model: req.Model, Status: ModelAttemptFailed, Error: toModelError(genErr)})
		return StreamRouteResult{Attempts: attempts}, genErr
	}

	var modelErr *ModelError
	if !errors.As(genErr, &modelErr) || !r.fallback.IsRetryable(modelErr) {
		attempts = append(attempts, ModelAttempt{Provider: target, Model: req.Model, Status: ModelAttemptFailed, Error: toModelError(genErr)})
		return StreamRouteResult{Attempts: attempts}, genErr
	}

	attempts = append(attempts, ModelAttempt{Provider: target, Model: req.Model, Status: ModelAttemptFallback, Error: modelErr})
	return r.fallback.StreamWithAttempts(ctx, req, target, r.registry, reqs, r.policy.Eligible, attempts)
}

// resolveTargetWithEntry applies the routing policy to determine the primary
// provider, respecting Eligible constraints and capability requirements.
func (r *ModelRouter) resolveTargetWithEntry(req ModelRequest, reqs ProviderCapabilities) (ProviderID, ProviderEntry, error) {
	eligible := r.policy.Eligible
	if len(eligible) == 0 {
		eligible = r.registry.IDs()
	}

	eligibleSet := make(map[ProviderID]struct{}, len(eligible))
	for _, id := range eligible {
		eligibleSet[id] = struct{}{}
	}

	// Check predicate first.
	if r.policy.Predicate != nil {
		if id := r.policy.Predicate(req); id != "" {
			if _, ok := eligibleSet[id]; !ok {
				return "", ProviderEntry{}, fmt.Errorf("lebro: predicate returned provider %q not in eligible set", id)
			}
			entry, err := r.registry.Get(id)
			if err != nil {
				return "", ProviderEntry{}, err
			}
			if !entry.Capabilities.Satisfies(reqs) {
				return "", ProviderEntry{}, fmt.Errorf("lebro: provider %q lacks required capabilities", id)
			}
			return id, entry, nil
		}
	}

	// Check primary if it's in the eligible set.
	if r.policy.Primary != "" {
		if _, ok := eligibleSet[r.policy.Primary]; ok {
			entry, err := r.registry.Get(r.policy.Primary)
			if err != nil {
				return "", ProviderEntry{}, err
			}
			if entry.Capabilities.Satisfies(reqs) {
				return r.policy.Primary, entry, nil
			}
		}
	}

	// Walk eligible list to find first with required capabilities.
	for _, id := range eligible {
		entry, err := r.registry.Get(id)
		if err != nil {
			continue
		}
		if entry.Capabilities.Satisfies(reqs) {
			return id, entry, nil
		}
	}

	return "", ProviderEntry{}, errors.New("lebro: no eligible provider satisfies request requirements")
}

// requestRequirements derives the capability requirements from a request.
func requestRequirements(req ModelRequest) ProviderCapabilities {
	var caps ProviderCapabilities
	if len(req.Tools) > 0 {
		caps.SupportsTools = true
	}
	if req.OutputSchema != nil {
		caps.SupportsStructuredOutput = true
	}
	return caps
}

// Registry exposes the router's provider registry for inspection.
func (r *ModelRouter) Registry() *ProviderRegistry {
	if r == nil {
		return nil
	}
	return r.registry
}

// Policy returns the router's routing policy.
func (r *ModelRouter) Policy() RoutingPolicy {
	if r == nil {
		return RoutingPolicy{}
	}
	return r.policy
}

// Fallback returns the router's fallback policy, or nil when none is
// configured.
func (r *ModelRouter) Fallback() *FallbackPolicy {
	if r == nil {
		return nil
	}
	return r.fallback
}

// toModelError converts an error to a ModelError if possible, otherwise wraps it.
func toModelError(err error) *ModelError {
	if err == nil {
		return nil
	}
	var modelErr *ModelError
	if errors.As(err, &modelErr) {
		return modelErr
	}
	return &ModelError{Kind: ModelErrorUnknown, Message: err.Error(), Err: err}
}

// responseToStreamReader wraps a ModelResponse in a StreamReader for
// compatibility with non-streaming providers.
func responseToStreamReader(resp ModelResponse) StreamReader {
	deltas := []StreamDelta{}

	calls := resp.Message.ToolCalls.Values()
	for i := range calls {
		call := calls[i]
		deltas = append(deltas, StreamDelta{ToolCall: &call})
	}

	if resp.Message.StructuredOutput != "" {
		deltas = append(deltas, StreamDelta{StructuredOutput: resp.Message.StructuredOutput})
	} else if resp.Message.Content != "" {
		deltas = append(deltas, StreamDelta{Text: resp.Message.Content})
	}

	terminal := StreamDelta{FinishReason: resp.FinishReason, Usage: resp.Usage}
	if terminal.Text == "" && terminal.ToolCall == nil && terminal.StructuredOutput == "" && terminal.FinishReason == "" {
		terminal.FinishReason = FinishReasonUnspecified
	}
	deltas = append(deltas, terminal)

	idx := 0
	return &StreamReaderFunc{
		NextFn: func() (StreamDelta, error) {
			if idx >= len(deltas) {
				return StreamDelta{}, io.EOF
			}
			delta := deltas[idx]
			idx++
			return delta, nil
		},
		CloseFn: func() error { return nil },
	}
}

// Ensure unused imports don't cause issues.
var _ = strings.Builder{}
var _ = time.Now
