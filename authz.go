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

func Allow() Decision             { return runtime.Allow() }
func Deny(reason string) Decision { return runtime.Deny(reason) }

func NewPolicyStore(store Store, policy Policy) (*PolicyStore, error) {
	return runtime.NewPolicyStore(store, policy)
}
