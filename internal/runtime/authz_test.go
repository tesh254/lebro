package runtime

import (
	"context"
	"errors"
	"testing"
)

// staticPolicy allows or denies every request uniformly, recording the last
// arguments it saw so tests can assert what the runtime passed.
type staticPolicy struct {
	allow  bool
	reason string

	gotIdentity Identity
	gotAction   Action
	gotResource Resource
	calls       int
}

func (p *staticPolicy) Authorize(_ context.Context, identity Identity, action Action, resource Resource) Decision {
	p.calls++
	p.gotIdentity = identity
	p.gotAction = action
	p.gotResource = resource
	if p.allow {
		return Allow()
	}
	return Deny(p.reason)
}

func TestIdentityFromContextRoundTrip(t *testing.T) {
	id := Identity{
		Subject:      "user-1",
		Tenant:       "acme",
		Capabilities: []Capability{"read", "write"},
		Attributes:   map[string]string{"team": "platform"},
	}
	ctx := WithIdentity(context.Background(), id)

	got, ok := IdentityFromContext(ctx)
	if !ok {
		t.Fatal("expected identity present")
	}
	if got.Subject != "user-1" || got.Tenant != "acme" {
		t.Fatalf("unexpected identity: %+v", got)
	}
	if !got.HasCapability("write") || got.HasCapability("admin") {
		t.Fatalf("unexpected capabilities: %+v", got.Capabilities)
	}
}

func TestIdentityFromContextAbsent(t *testing.T) {
	if _, ok := IdentityFromContext(context.Background()); ok {
		t.Fatal("expected no identity in a bare context")
	}
}

func TestWithIdentityClonesSliceAndMap(t *testing.T) {
	caps := []Capability{"read"}
	attrs := map[string]string{"team": "platform"}
	ctx := WithIdentity(context.Background(), Identity{Capabilities: caps, Attributes: attrs})

	// Mutating the caller's originals must not affect the stored identity.
	caps[0] = "admin"
	attrs["team"] = "security"

	got, _ := IdentityFromContext(ctx)
	if got.HasCapability("admin") {
		t.Fatal("stored capabilities aliased the caller slice")
	}
	if got.Attributes["team"] != "platform" {
		t.Fatal("stored attributes aliased the caller map")
	}
}

func TestAuthorizeNilPolicyAllows(t *testing.T) {
	if err := authorize(context.Background(), nil, ActionAgentRun, Resource{Kind: ResourceKindAgent, ID: "a"}); err != nil {
		t.Fatalf("nil policy must allow: %v", err)
	}
	var typed Policy
	if err := authorize(context.Background(), typed, ActionAgentRun, Resource{Kind: ResourceKindAgent, ID: "a"}); err != nil {
		t.Fatalf("typed-nil policy must allow: %v", err)
	}
}

func TestAuthorizeAllowAllPolicy(t *testing.T) {
	if err := authorize(context.Background(), AllowAllPolicy{}, ActionToolCall, Resource{Kind: ResourceKindTool, ID: "t"}); err != nil {
		t.Fatalf("allow-all must allow: %v", err)
	}
}

func TestAuthorizeDenialIsTypedAndMatchesSentinel(t *testing.T) {
	ctx := WithIdentity(context.Background(), Identity{Subject: "user-1", Tenant: "acme"})
	policy := &staticPolicy{allow: false, reason: "no access"}

	err := authorize(ctx, policy, ActionToolCall, Resource{Kind: ResourceKindTool, ID: "search"})
	if err == nil {
		t.Fatal("expected denial")
	}
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("denial must match ErrPolicyDenied: %v", err)
	}
	var denial *PolicyDenial
	if !errors.As(err, &denial) {
		t.Fatalf("denial must be a *PolicyDenial: %v", err)
	}
	if denial.Subject != "user-1" || denial.Tenant != "acme" {
		t.Fatalf("denial lost identity: %+v", denial)
	}
	if denial.Action != ActionToolCall || denial.Resource.ID != "search" || denial.Reason != "no access" {
		t.Fatalf("denial lost context: %+v", denial)
	}

	// The policy must have seen the context identity, not a zero value.
	if policy.gotIdentity.Subject != "user-1" {
		t.Fatalf("policy received wrong identity: %+v", policy.gotIdentity)
	}
	if policy.gotAction != ActionToolCall || policy.gotResource.ID != "search" {
		t.Fatalf("policy received wrong args: %s %+v", policy.gotAction, policy.gotResource)
	}
}
