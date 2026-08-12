package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ApprovalRequest is the typed payload a protected step publishes when it
// suspends to await a human decision. It is persisted verbatim as the suspend
// Payload so a resuming process (possibly after a restart) can render the
// pending request without re-consulting the step. Action and Resource identify
// what is being gated, mirroring the authorization vocabulary so a request is
// auditable alongside a PolicyDenial. Reason is an optional human-readable
// explanation. RequestedAt and ExpiresAt bound the decision window; a decision
// recorded after ExpiresAt is rejected as a timeout. Arguments carries the
// opaque input the guarded handler will receive once the request is approved,
// echoed back in the decision so the guarded step runs against the exact input
// that was reviewed.
type ApprovalRequest struct {
	Action      Action          `json:"action,omitempty"`
	Resource    Resource        `json:"resource,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	RequestedAt time.Time       `json:"requested_at"`
	ExpiresAt   time.Time       `json:"expires_at"`
	Arguments   json.RawMessage `json:"arguments,omitempty"`
}

// ApprovalDecision is the validated human input that resumes a gated run. It is
// the resume input the executor validates against the gate's decision schema
// before any step runs. Approved records the outcome; Decider and Reason are
// optional audit context; DecidedAt is compared against the request's ExpiresAt
// to detect a timeout. Request echoes the ApprovalRequest the decider acted on
// so the guarded step recovers the reviewed arguments and the executor persists
// a self-describing decision.
type ApprovalDecision struct {
	Approved  bool            `json:"approved"`
	Decider   string          `json:"decider,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	DecidedAt time.Time       `json:"decided_at"`
	Request   ApprovalRequest `json:"request"`
}

// ApprovalRequirement declares that a step is protected by a human approval
// gate. Action, Resource, and Reason seed the ApprovalRequest published on
// suspend. TTL bounds the decision window: a request suspended at time t
// expires at t+TTL, and a decision recorded after that instant fails the run as
// a timeout. A zero TTL means the request never expires. DecisionSchema is the
// JSON Schema the resume input (an ApprovalDecision) is validated against; it
// is the SuspendSchema the gate step must carry so a process restart never
// observes an unvalidated decision contract.
type ApprovalRequirement struct {
	Action         Action
	Resource       Resource
	Reason         string
	TTL            time.Duration
	DecisionSchema json.RawMessage
}

var (
	// ErrApprovalRejected matches a run failed because a human denied the
	// pending approval. It is wrapped in a WorkflowErrorStepFailed so the
	// rejection is recorded in the run record and terminal event.
	ErrApprovalRejected = errors.New("lebro: approval rejected")
	// ErrApprovalExpired matches a run failed because the decision was recorded
	// after the request's ExpiresAt, or the gate observed expiry at resume. The
	// timeout is recorded like any other step failure.
	ErrApprovalExpired = errors.New("lebro: approval expired")
	// ErrApprovalInvalidDecision matches a decision that passed the JSON Schema
	// but is not usable — a zero DecidedAt, or a Request that does not match the
	// one published at suspend. Structurally malformed decisions are rejected
	// earlier by the executor's resume-input validation as ErrInvalidResumeInput.
	ErrApprovalInvalidDecision = errors.New("lebro: approval decision invalid")
)

// ApprovalGate pairs the two steps that implement a durable human approval
// gate: a Request step that suspends the run to await a decision, and a Guard
// step that enforces the decision and invokes the protected handler only on
// approval. The gate reuses the workflow suspend/resume machinery unchanged —
// the request is the suspend Payload and the decision is the validated resume
// input — so a pending approval survives a process restart and every outcome
// (approve, reject, timeout, invalid) is recorded through the existing run
// events. Compose the steps in declared order with the Request step first.
type ApprovalGate struct {
	Request Step
	Guard   Step
}

// NewApprovalGate builds an approval gate around inner, the protected handler
// that must not run before a human approves. requestID and guardID are the
// StepIDs of the two generated steps; both must be unique within the workflow.
// req declares the approval semantics; req.DecisionSchema is required so the
// resume decision is validated durably. clock supplies the timestamps that
// stamp the request and detect expiry; a nil clock uses the wall clock.
//
// The Request step turns its input (the arguments the protected handler would
// receive) into an ApprovalRequest and suspends. The Guard step receives the
// resume decision, rejects a denial, a timeout, or a tampered request, and on
// approval invokes inner with the arguments echoed in the decision.
func NewApprovalGate(requestID, guardID StepID, inner StepHandler, req ApprovalRequirement, clock Clock) (ApprovalGate, error) {
	if requestID == "" || guardID == "" {
		return ApprovalGate{}, errors.New("lebro: approval gate step IDs are required")
	}
	if requestID == guardID {
		return ApprovalGate{}, errors.New("lebro: approval gate request and guard step IDs must differ")
	}
	if inner == nil || isNilInterface(inner) {
		return ApprovalGate{}, errors.New("lebro: approval gate protected handler is required")
	}
	if len(req.DecisionSchema) == 0 {
		return ApprovalGate{}, errors.New("lebro: approval gate decision schema is required")
	}
	if req.TTL < 0 {
		return ApprovalGate{}, errors.New("lebro: approval gate TTL must not be negative")
	}
	if clock == nil || isNilInterface(clock) {
		clock = defaultClock{}
	}

	requestHandler := &approvalRequestHandler{stepID: requestID, req: req, clock: clock}
	guardHandler := &approvalGuardHandler{inner: inner, clock: clock}

	return ApprovalGate{
		Request: Step{
			Definition: StepDefinition{ID: requestID, SuspendSchema: cloneRawMessage(req.DecisionSchema)},
			Handler:    requestHandler,
		},
		Guard: Step{
			Definition: StepDefinition{ID: guardID},
			Handler:    guardHandler,
		},
	}, nil
}

// approvalRequestHandler builds an ApprovalRequest from the step input and
// suspends the run to await a human decision.
type approvalRequestHandler struct {
	stepID StepID
	req    ApprovalRequirement
	clock  Clock
}

// Execute always suspends: it never lets the protected handler run before a
// decision. The step input is captured as the request Arguments so the guard
// can invoke the protected handler against the exact reviewed input.
func (h *approvalRequestHandler) Execute(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
	now := h.clock.Now()
	request := ApprovalRequest{
		Action:      h.req.Action,
		Resource:    h.req.Resource,
		Reason:      h.req.Reason,
		RequestedAt: now,
		Arguments:   cloneRawMessage(input),
	}
	if h.req.TTL > 0 {
		request.ExpiresAt = now.Add(h.req.TTL)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("lebro: marshal approval request: %w", err)
	}
	return nil, &SuspendError{Signal: SuspendSignal{
		StepID:  h.stepID,
		Payload: payload,
	}}
}

// approvalGuardHandler enforces the resumed decision and invokes the protected
// handler only when the decision approves within the request window.
type approvalGuardHandler struct {
	inner StepHandler
	clock Clock
}

// Execute receives the resume decision as its input. It rejects a denial, a
// timeout, or a decision missing its DecidedAt stamp, then invokes the
// protected handler with the arguments echoed in the decision request. A
// rejection, timeout, or invalid decision is returned as an ordinary step error
// so the executor records it as a terminal step failure.
func (h *approvalGuardHandler) Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var decision ApprovalDecision
	if err := json.Unmarshal(input, &decision); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrApprovalInvalidDecision, err)
	}
	if decision.DecidedAt.IsZero() {
		return nil, fmt.Errorf("%w: decision has no DecidedAt", ErrApprovalInvalidDecision)
	}
	if !decision.Approved {
		reason := decision.Reason
		if reason == "" {
			reason = "no reason given"
		}
		return nil, fmt.Errorf("%w: %s", ErrApprovalRejected, reason)
	}
	if expiry := decision.Request.ExpiresAt; !expiry.IsZero() {
		if decision.DecidedAt.After(expiry) || h.clock.Now().After(expiry) {
			return nil, fmt.Errorf("%w: decided at %s, expired at %s", ErrApprovalExpired, decision.DecidedAt.UTC().Format(time.RFC3339), expiry.UTC().Format(time.RFC3339))
		}
	}
	return h.inner.Execute(ctx, cloneRawMessage(decision.Request.Arguments))
}
