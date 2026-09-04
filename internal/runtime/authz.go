package runtime

import (
	"context"
	"errors"
	"fmt"
)

// Capability is an opaque, application-defined permission grant carried on an
// Identity. The core library never interprets a capability; a Policy decides
// what a capability means. Modeling permissions as capabilities keeps the
// runtime decoupled from any particular identity provider or role model.
type Capability string

// Action identifies the operation a Policy is asked to authorize. The core
// primitives request one of the Action constants below; applications may define
// their own for surfaces the library does not yet cover.
type Action string

const (
	// ActionAgentRun is requested once at the start of an agent run, before any
	// model call or tool invocation.
	ActionAgentRun Action = "agent.run"
	// ActionToolCall is requested before each model-requested tool invocation,
	// including delegation to a Subagent.
	ActionToolCall Action = "tool.call"
	// ActionWorkflowRun is requested once at the start of a workflow run.
	ActionWorkflowRun Action = "workflow.run"
	// ActionNetworkRun is requested before a router-led Network begins.
	ActionNetworkRun Action = "network.run"
	// ActionStorageRead is requested before a read against a storage
	// repository (thread, message, workflow run, or snapshot lookups and lists).
	ActionStorageRead Action = "storage.read"
	// ActionStorageWrite is requested before a write against a storage
	// repository (thread, message, workflow run, or snapshot mutations).
	ActionStorageWrite Action = "storage.write"
)

// ResourceKind identifies the type of resource an Action targets so a Policy
// can key decisions on the kind without parsing the resource ID.
type ResourceKind string

const (
	ResourceKindAgent            ResourceKind = "agent"
	ResourceKindTool             ResourceKind = "tool"
	ResourceKindWorkflow         ResourceKind = "workflow"
	ResourceKindNetwork          ResourceKind = "network"
	ResourceKindThread           ResourceKind = "thread"
	ResourceKindMessage          ResourceKind = "message"
	ResourceKindWorkflowRun      ResourceKind = "workflow_run"
	ResourceKindWorkflowSnapshot ResourceKind = "workflow_snapshot"
	ResourceKindSchedule         ResourceKind = "schedule"
	ResourceKindWorkingMemory    ResourceKind = "working_memory"
)

// Resource describes the target of an authorization decision. Kind and ID
// identify the target; Tenant scopes it for multi-tenant policies and is empty
// for single-tenant use. Attributes carries optional application context a
// Policy may key on; it is never interpreted by the core library.
type Resource struct {
	Kind       ResourceKind
	ID         string
	Tenant     string
	OwnerID    string
	Attributes map[string]string
}

type runtimeScopeContextKey struct{}

// WithRuntimeScope supplies an application-verified persistence scope for
// policy-guarded repository operations. Middleware normally sets it after
// authenticating the request; PolicyStore rejects a write whose claimed record
// scope differs from this value.
func WithRuntimeScope(ctx context.Context, scope RuntimeScope) context.Context {
	return context.WithValue(ctx, runtimeScopeContextKey{}, scope)
}

// RuntimeScopeFromContext returns the verified scope supplied by middleware.
func RuntimeScopeFromContext(ctx context.Context) (RuntimeScope, bool) {
	if ctx == nil {
		return RuntimeScope{}, false
	}
	scope, ok := ctx.Value(runtimeScopeContextKey{}).(RuntimeScope)
	return scope, ok
}

// Identity is the authenticated caller on whose behalf a run, tool call, or
// storage operation is performed. It is a plain value with no behavior so the
// runtime stays decoupled from any identity provider: an application
// authenticates however it likes and populates an Identity.
//
// Subject is the stable caller identifier; Tenant scopes the caller for
// multi-tenant policies; Capabilities are the opaque permission grants a Policy
// interprets; Attributes carries optional application context. All fields are
// optional — the zero Identity is the anonymous caller, which a Policy is free
// to allow or deny.
type Identity struct {
	Subject      string
	Tenant       string
	Capabilities []Capability
	Attributes   map[string]string
}

// HasCapability reports whether the identity carries the given capability.
func (i Identity) HasCapability(capability Capability) bool {
	for _, held := range i.Capabilities {
		if held == capability {
			return true
		}
	}
	return false
}

// Decision is the outcome of a Policy evaluation. Reason is an optional
// human-readable explanation that is preserved on a denial for auditing; it is
// ignored when Allowed is true.
type Decision struct {
	Allowed bool
	Reason  string
}

// Allow returns an allowing decision.
func Allow() Decision { return Decision{Allowed: true} }

// Deny returns a denying decision carrying an optional reason.
func Deny(reason string) Decision { return Decision{Allowed: false, Reason: reason} }

// Policy authorizes an Action against a Resource for an Identity. It is the
// single hook the core primitives consult before acting. Implementations must
// be safe for concurrent use and must not mutate the arguments.
//
// The library ships no identity provider and no concrete policy engine: an
// application supplies its own Policy, or none. A nil Policy means every
// operation is permitted, so the core library remains usable without one.
type Policy interface {
	Authorize(ctx context.Context, identity Identity, action Action, resource Resource) Decision
}

// AllowAllPolicy permits every operation. It is the explicit no-op policy,
// equivalent to configuring no policy at all, and is useful where a non-nil
// Policy is expected but no restriction is wanted.
type AllowAllPolicy struct{}

// Authorize always allows.
func (AllowAllPolicy) Authorize(context.Context, Identity, Action, Resource) Decision {
	return Allow()
}

var _ Policy = AllowAllPolicy{}

// ErrPolicyDenied matches every authorization denial via errors.Is, letting
// callers branch on a policy denial without depending on the concrete
// *PolicyDenial type.
var ErrPolicyDenied = errors.New("lebro: policy denied")

// PolicyDenial is the typed, auditable error produced when a Policy denies an
// operation. It preserves the caller subject and tenant, the attempted action,
// the target resource, and the policy's reason so a denial is fully
// reconstructable from a run record without re-consulting the policy.
type PolicyDenial struct {
	Subject  string
	Tenant   string
	Action   Action
	Resource Resource
	Reason   string
}

func (e *PolicyDenial) Error() string {
	if e == nil {
		return "lebro: policy denied"
	}
	subject := e.Subject
	if subject == "" {
		subject = "anonymous"
	}
	base := fmt.Sprintf("lebro: policy denied %s on %s %q for subject %q", e.Action, e.Resource.Kind, e.Resource.ID, subject)
	if e.Reason == "" {
		return base
	}
	return base + ": " + e.Reason
}

// Is reports a match against ErrPolicyDenied so errors.Is(err, ErrPolicyDenied)
// succeeds for any denial.
func (e *PolicyDenial) Is(target error) bool {
	return target == ErrPolicyDenied
}

// authorize consults policy for one decision and returns a *PolicyDenial when
// the decision denies the operation. A nil policy allows everything, so callers
// can invoke it unconditionally and a run without a configured policy behaves
// exactly as before. The identity is read from the context so every surface
// authorizes against the same caller without threading it through call sites.
func authorize(ctx context.Context, policy Policy, action Action, resource Resource) error {
	if policy == nil || isNilInterface(policy) {
		return nil
	}
	identity, _ := IdentityFromContext(ctx)
	decision := policy.Authorize(ctx, identity, action, resource)
	if decision.Allowed {
		return nil
	}
	return &PolicyDenial{
		Subject:  identity.Subject,
		Tenant:   identity.Tenant,
		Action:   action,
		Resource: resource,
		Reason:   decision.Reason,
	}
}

type identityContextKey struct{}

// WithIdentity returns a context carrying identity so downstream agent, tool,
// workflow, and storage operations authorize against the same caller. It is the
// canonical way an application propagates the authenticated caller into a run;
// nested runs (subagents, workflow steps) inherit it automatically because they
// share the run context.
func WithIdentity(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, identityContextKey{}, cloneIdentity(identity))
}

// IdentityFromContext returns a caller-owned copy of the identity previously
// stored with WithIdentity and whether one was present. It returns the zero
// Identity and false outside an identity-scoped context.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	if ctx == nil {
		return Identity{}, false
	}
	identity, ok := ctx.Value(identityContextKey{}).(Identity)
	if !ok {
		return Identity{}, false
	}
	return cloneIdentity(identity), true
}

// cloneIdentity deep-copies the slice and map so a stored identity cannot be
// mutated through an alias the caller retained, mirroring how metadata is
// copied across the runtime.
func cloneIdentity(identity Identity) Identity {
	identity.Capabilities = append([]Capability(nil), identity.Capabilities...)
	identity.Attributes = cloneMetadata(identity.Attributes)
	return identity
}
