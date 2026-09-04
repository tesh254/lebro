package lebro

import (
	"context"

	"github.com/tesh254/lebro/internal/runtime"
)

func WithIdentity(ctx context.Context, identity Identity) context.Context {
	return runtime.WithIdentity(ctx, identity)
}

func IdentityFromContext(ctx context.Context) (Identity, bool) {
	return runtime.IdentityFromContext(ctx)
}

func WithRuntimeScope(ctx context.Context, scope RuntimeScope) context.Context {
	return runtime.WithRuntimeScope(ctx, scope)
}

func RuntimeScopeFromContext(ctx context.Context) (RuntimeScope, bool) {
	return runtime.RuntimeScopeFromContext(ctx)
}

func Allow() Decision             { return runtime.Allow() }
func Deny(reason string) Decision { return runtime.Deny(reason) }

func NewPolicyStore(store Store, policy Policy) (*PolicyStore, error) {
	return runtime.NewPolicyStore(store, policy)
}
