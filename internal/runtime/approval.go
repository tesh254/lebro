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
// opaque input the guarded handler will receive once the request is approved;
// the guard invokes the handler with the arguments from this persisted request,
// never a copy supplied at resume, so an approved run cannot execute input the
// approver never reviewed.
type ApprovalRequest struct {
	Action      Action          `json:"action,omitempty"`
	Resource    Resource        `json:"resource,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	RequestedAt time.Time       `json:"requested_at"`
	ExpiresAt   time.Time       `json:"expires_at"`
	Arguments   json.RawMessage `json:"arguments,omitempty"`
}

// ApprovalDecision is the human input that resumes a gated run, supplied as the
// resume input. The guard validates it against the requirement's DecisionSchema.
// Approved records the outcome; Decider and Reason are optional audit context;
// DecidedAt is compared against the request's ExpiresAt to detect a timeout.
// Request must echo the ApprovalRequest published at suspend exactly: the guard
// compares it against the authoritative request persisted in the durable
// snapshot and rejects any mismatch, so a resumer cannot alter the reviewed
// action, resource, window, or arguments.
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
// JSON Schema the resume input (an ApprovalDecision) is validated against by the
// guard before any protected work runs; it is compiled once with the workflow's
// SchemaCompiler.
type ApprovalRequirement struct {
	Action         Action
	Resource       Resource
	Reason         string
	TTL            time.Duration
	DecisionSchema json.RawMessage
}

// approvalSuspendSchema is the permissive schema the gate's request step carries
// as its SuspendSchema. It must accept both the empty contract published at
// suspend and any decision object submitted at resume, because the gate does not
// pin resume to a single contract value: the human sets Approved, Decider, and
// DecidedAt, so a value-equality contract cannot express a valid resume. The
// caller's ApprovalRequirement.DecisionSchema is enforced by the guard instead,
// against the authoritative request loaded from the durable snapshot.
var approvalSuspendSchema = json.RawMessage(`{}`)

var (
	// ErrApprovalRejected matches a run failed because a human denied the
	// pending approval. It is wrapped in a WorkflowErrorStepFailed so the
	// rejection is recorded in the run record and terminal event.
	ErrApprovalRejected = errors.New("lebro: approval rejected")
	// ErrApprovalExpired matches a run failed because the decision was recorded
	// after the request's ExpiresAt, or the gate observed expiry at resume. The
	// timeout is recorded like any other step failure.
	ErrApprovalExpired = errors.New("lebro: approval expired")
	// ErrApprovalInvalidDecision matches a decision the guard cannot act on: one
	// that fails the requirement's DecisionSchema, carries a zero DecidedAt, or
	// echoes a Request that does not match the one published at suspend.
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
// req declares the approval semantics; req.DecisionSchema is required and
// compiled once with compiler so the guard validates every resume decision.
// store is the durable Store the guard loads the authoritative request from so a
// tampered decision cannot smuggle unreviewed arguments past the gate; it must
// be the same Store bound to the workflow. clock supplies the timestamps that
// stamp the request and detect expiry; a nil clock uses the wall clock.
//
// The Request step turns its input (the arguments the protected handler would
// receive) into an ApprovalRequest and suspends. The Guard step receives the
// resume decision, validates it against DecisionSchema, requires it to echo the
// exact request published at suspend, rejects a denial or a decision recorded
// outside the request window, and only then invokes inner with the reviewed
// arguments.
func NewApprovalGate(requestID, guardID StepID, inner StepHandler, req ApprovalRequirement, compiler SchemaCompiler, store Store, clock Clock) (ApprovalGate, error) {
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
	if compiler == nil || isNilInterface(compiler) {
		return ApprovalGate{}, errors.New("lebro: approval gate schema compiler is required")
	}
	if store == nil || isNilInterface(store) {
		return ApprovalGate{}, errors.New("lebro: approval gate store is required")
	}
	decisionSchema, err := compiler.Compile(cloneRawMessage(req.DecisionSchema))
	if err != nil {
		return ApprovalGate{}, fmt.Errorf("lebro: compile approval decision schema: %w", err)
	}
	if clock == nil || isNilInterface(clock) {
		clock = defaultClock{}
	}

	requestHandler := &approvalRequestHandler{stepID: requestID, req: req, clock: clock}
	guardHandler := &approvalGuardHandler{
		requestStepID:  requestID,
		inner:          inner,
		decisionSchema: decisionSchema,
		store:          store,
		clock:          clock,
	}

	return ApprovalGate{
		Request: Step{
			Definition: StepDefinition{ID: requestID, SuspendSchema: cloneRawMessage(approvalSuspendSchema)},
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
// handler only when the decision approves within the request window and echoes
// the exact request published at suspend.
type approvalGuardHandler struct {
	requestStepID  StepID
	inner          StepHandler
	decisionSchema CompiledSchema
	store          Store
	clock          Clock
}

// Execute receives the resume decision as its input. It validates the decision
// against the caller's DecisionSchema, loads the authoritative request published
// at suspend from the durable snapshot, and requires the decision to echo it so
// a resumer cannot approve arguments that were never reviewed. It then enforces
// expiry before the approval outcome so a late decision is always a timeout
// rather than a rejection, and on approval invokes the protected handler with
// the authoritative request's arguments (never the decision's copy). A
// rejection, timeout, invalid, or tampered decision is returned as an ordinary
// step error so the executor records it as a terminal step failure.
func (h *approvalGuardHandler) Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateStepValue(h.decisionSchema, ValidationTargetResumeInput, input); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrApprovalInvalidDecision, err)
	}
	var decision ApprovalDecision
	if err := json.Unmarshal(input, &decision); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrApprovalInvalidDecision, err)
	}
	if decision.DecidedAt.IsZero() {
		return nil, fmt.Errorf("%w: decision has no DecidedAt", ErrApprovalInvalidDecision)
	}

	// The authoritative request is the one persisted at suspend. Comparing the
	// decision's echoed request against it closes the tamper path: a resumer
	// cannot alter the reviewed action, resource, expiry, or arguments.
	authoritative, err := h.loadRequest(ctx)
	if err != nil {
		return nil, err
	}
	echoed, err := json.Marshal(decision.Request)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal decision request: %s", ErrApprovalInvalidDecision, err)
	}
	if !rawJSONEqual(echoed, authoritative) {
		return nil, fmt.Errorf("%w: decision request does not match the request published at suspend", ErrApprovalInvalidDecision)
	}

	var request ApprovalRequest
	if err := json.Unmarshal(authoritative, &request); err != nil {
		return nil, fmt.Errorf("lebro: decode persisted approval request: %w", err)
	}

	// Expiry is evaluated before the approval outcome so a decision recorded
	// outside the request window is consistently a timeout, whether it approves
	// or denies.
	if expiry := request.ExpiresAt; !expiry.IsZero() {
		if decision.DecidedAt.After(expiry) || h.clock.Now().After(expiry) {
			return nil, fmt.Errorf("%w: decided at %s, expired at %s", ErrApprovalExpired, decision.DecidedAt.UTC().Format(time.RFC3339), expiry.UTC().Format(time.RFC3339))
		}
	}
	if !decision.Approved {
		reason := decision.Reason
		if reason == "" {
			reason = "no reason given"
		}
		return nil, fmt.Errorf("%w: %s", ErrApprovalRejected, reason)
	}
	return h.inner.Execute(ctx, cloneRawMessage(request.Arguments))
}

// loadRequest returns the ApprovalRequest published at suspend, as raw JSON,
// from the latest suspend snapshot of the current run. It is the authoritative
// record the guard compares the resumed decision against.
func (h *approvalGuardHandler) loadRequest(ctx context.Context) (json.RawMessage, error) {
	runID := workflowInvocationFromContext(ctx).runID
	if runID == "" {
		return nil, errors.New("lebro: approval guard has no run context")
	}
	var (
		best    int64 = -1
		payload json.RawMessage
		cursor  string
	)
	for {
		page, err := h.store.WorkflowSnapshots().ListWorkflowSnapshots(ctx, runID, PageRequest{Cursor: cursor, Limit: 1000})
		if err != nil {
			return nil, fmt.Errorf("lebro: load approval request for run %q: %w", runID, err)
		}
		for _, record := range page.Records {
			var env workflowSnapshotEnvelope
			if json.Unmarshal(record.State, &env) != nil || env.Suspend == nil {
				continue
			}
			if env.StepID != h.requestStepID {
				continue
			}
			if record.Sequence > best {
				best = record.Sequence
				payload = cloneRawMessage(env.Suspend.Payload)
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if best < 0 {
		return nil, fmt.Errorf("%w: no persisted request for run %q", ErrApprovalInvalidDecision, runID)
	}
	return payload, nil
}
