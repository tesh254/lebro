package runtime

import (
	"context"
	"errors"
	"fmt"
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

// ModelRouter implements Model by routing requests through a provider registry
// according to a routing policy and optional fallback chain. It is safe for
// concurrent use.
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
// implements the Model interface.
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

// Generate routes the request to the appropriate provider and delegates the
// call. When a fallback policy is configured and the primary provider fails
// with a retryable error, the router walks the fallback chain.
func (r *ModelRouter) Generate(ctx context.Context, req ModelRequest) (ModelResponse, error) {
	if r == nil {
		return ModelResponse{}, errors.New("lebro: model router is nil")
	}

	target := r.resolveTarget(req)
	entry, err := r.registry.Get(target)
	if err != nil {
		return ModelResponse{}, fmt.Errorf("lebro: router resolve provider: %w", err)
	}

	reqs := requestRequirements(req)
	if !entry.Capabilities.Satisfies(reqs) {
		return ModelResponse{}, fmt.Errorf("lebro: provider %q lacks required capabilities", entry.ID)
	}

	resp, genErr := entry.Model.Generate(ctx, req)
	if genErr == nil {
		return resp, nil
	}

	if r.fallback == nil {
		return resp, genErr
	}

	var modelErr *ModelError
	if !errors.As(genErr, &modelErr) || !r.fallback.IsRetryable(modelErr) {
		return resp, genErr
	}

	return r.fallback.Generate(ctx, req, entry.ID, r.registry, reqs)
}

// resolveTarget applies the routing policy to determine the primary provider.
func (r *ModelRouter) resolveTarget(req ModelRequest) ProviderID {
	if r.policy.Predicate != nil {
		if id := r.policy.Predicate(req); id != "" {
			return id
		}
	}
	if r.policy.Primary != "" {
		return r.policy.Primary
	}
	if len(r.policy.Eligible) > 0 {
		return r.policy.Eligible[0]
	}
	ids := r.registry.IDs()
	if len(ids) > 0 {
		return ids[0]
	}
	return ""
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
