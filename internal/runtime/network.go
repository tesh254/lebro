package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// DefaultNetworkMaxHops bounds a network unless NetworkConfig.MaxHops supplies
// a smaller or larger explicit budget.
const DefaultNetworkMaxHops = 8

// NetworkSpecialist is one named, independently executable member of a
// Network. IDs are stable route identities; they are intentionally separate
// from a workflow's definition ID so applications can replace an
// implementation without invalidating stored route history.
type NetworkSpecialist struct {
	ID          ToolID
	Workflow    Workflow
	Description string
}

// NetworkRouteRecord is one durable routing decision. Network writes these as
// WorkflowRunRecord.StepOutputs in hop order, so every Store implementation
// can reconstruct a traversal without a storage-schema migration.
type NetworkRouteRecord struct {
	Hop        int       `json:"hop"`
	Candidates []ToolID  `json:"candidates"`
	Selected   ToolID    `json:"selected,omitempty"`
	ChildRunID RunID     `json:"child_run_id,omitempty"`
	Status     RunStatus `json:"status"`
	Failure    string    `json:"failure,omitempty"`
}

// NetworkErrorKind classifies network-level failures before callers inspect a
// child workflow error.
type NetworkErrorKind string

const (
	NetworkErrorInvalidInput     NetworkErrorKind = "network_invalid_input"
	NetworkErrorSelection        NetworkErrorKind = "network_route_selection_failed"
	NetworkErrorCycle            NetworkErrorKind = "network_cycle_detected"
	NetworkErrorHopLimit         NetworkErrorKind = "network_hop_limit_exhausted"
	NetworkErrorSpecialistFailed NetworkErrorKind = "network_specialist_failed"
	NetworkErrorPersistFailed    NetworkErrorKind = "network_persist_failed"
	NetworkErrorUnauthorized     NetworkErrorKind = "network_unauthorized"
)

var (
	ErrNetworkInvalidInput     = errors.New("lebro: network invalid input")
	ErrNetworkSelection        = errors.New("lebro: network route selection failed")
	ErrNetworkCycle            = errors.New("lebro: network cycle detected")
	ErrNetworkHopLimit         = errors.New("lebro: network hop limit exhausted")
	ErrNetworkSpecialistFailed = errors.New("lebro: network specialist failed")
	ErrNetworkPersist          = errors.New("lebro: network persistence failed")
	ErrNetworkUnauthorized     = errors.New("lebro: network unauthorized")
)

// NetworkError keeps route history and the original error actionable.
type NetworkError struct {
	Kind   NetworkErrorKind
	Hop    int
	Routes []NetworkRouteRecord
	Err    error
}

func (e *NetworkError) Error() string {
	if e == nil {
		return "lebro: network failure"
	}
	if e.Err == nil {
		return fmt.Sprintf("lebro: network %s", e.Kind)
	}
	return fmt.Sprintf("lebro: network %s at hop %d: %v", e.Kind, e.Hop, e.Err)
}
func (e *NetworkError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}
func (e *NetworkError) Is(target error) bool {
	if e == nil {
		return false
	}
	switch e.Kind {
	case NetworkErrorInvalidInput:
		return target == ErrNetworkInvalidInput
	case NetworkErrorSelection:
		return target == ErrNetworkSelection
	case NetworkErrorCycle:
		return target == ErrNetworkCycle
	case NetworkErrorHopLimit:
		return target == ErrNetworkHopLimit
	case NetworkErrorSpecialistFailed:
		return target == ErrNetworkSpecialistFailed
	case NetworkErrorPersistFailed:
		return target == ErrNetworkPersist
	case NetworkErrorUnauthorized:
		return target == ErrNetworkUnauthorized
	default:
		return false
	}
}

// NetworkConfig defines bounded router-led specialist execution. It does not
// replace NewSubagent: Network is for application-owned orchestration where
// every handoff and route must be retained as a durable record.
type NetworkConfig struct {
	Definition  WorkflowDefinition
	Router      SpecialistRouter
	Specialists []NetworkSpecialist
	MaxHops     int
	Deadline    time.Duration
	Policy      Policy
	Store       Store
	Clock       Clock
	IDSource    IDSource
	Listener    RunListener
}

// Network coordinates named specialists through an explicit router.
type Network struct {
	definition  WorkflowDefinition
	router      SpecialistRouter
	specialists map[ToolID]NetworkSpecialist
	maxHops     int
	deadline    time.Duration
	policy      Policy
	store       Store
	clock       Clock
	idSource    IDSource
	listener    RunListener
}

var _ Workflow = (*Network)(nil)

// NewNetwork validates a network contract. A route may never revisit a
// specialist in one run; configure distinct specialists for retries instead.
func NewNetwork(config NetworkConfig) (*Network, error) {
	if config.Definition.ID == "" {
		return nil, errors.New("lebro: network definition ID is required")
	}
	if config.Router == nil || isNilInterface(config.Router) {
		return nil, errors.New("lebro: network router is required")
	}
	if len(config.Specialists) == 0 {
		return nil, errors.New("lebro: network needs specialists")
	}
	if config.MaxHops < 0 {
		return nil, errors.New("lebro: network max hops must not be negative")
	}
	if config.Deadline < 0 {
		return nil, errors.New("lebro: network deadline must not be negative")
	}
	specialists := make(map[ToolID]NetworkSpecialist, len(config.Specialists))
	for _, specialist := range config.Specialists {
		if specialist.ID == "" || specialist.Workflow == nil || isNilInterface(specialist.Workflow) {
			return nil, errors.New("lebro: network specialist ID and workflow are required")
		}
		if _, exists := specialists[specialist.ID]; exists {
			return nil, fmt.Errorf("lebro: network duplicate specialist %q", specialist.ID)
		}
		if specialist.Description == "" {
			specialist.Description = specialist.Workflow.Definition().Description
		}
		specialists[specialist.ID] = specialist
	}
	maxHops := config.MaxHops
	if maxHops == 0 {
		maxHops = DefaultNetworkMaxHops
	}
	clock := config.Clock
	if clock == nil {
		clock = defaultClock{}
	}
	ids := config.IDSource
	if ids == nil {
		ids = &sequentialIDSource{}
	}
	return &Network{definition: config.Definition, router: config.Router, specialists: specialists, maxHops: maxHops, deadline: config.Deadline, policy: config.Policy, store: config.Store, clock: clock, idSource: ids, listener: config.Listener}, nil
}

func (n *Network) Definition() WorkflowDefinition {
	if n == nil {
		return WorkflowDefinition{}
	}
	return n.definition
}

// Run executes one or more router-selected specialists. The final selected
// specialist result is returned unchanged apart from network run ID/metadata;
// callers retain ordinary Workflow semantics while Network owns traversal.
func (n *Network) Run(ctx context.Context, input RunInput) (RunResult, error) {
	if n == nil {
		return RunResult{}, &NetworkError{Kind: NetworkErrorInvalidInput, Err: errors.New("lebro: network is nil")}
	}
	if ctx == nil {
		return RunResult{}, &NetworkError{Kind: NetworkErrorInvalidInput, Err: errors.New("lebro: network context is nil")}
	}
	runID := n.idSource.NewRunID()
	emitter := newRunEmitter(ctx, n.listener, n.clock, n.idSource)
	emitter.emit(runID, 0, "", RunEventStarted)
	task, err := networkTask(input.Messages)
	if err != nil {
		return n.fail(ctx, emitter, runID, input, nil, 0, NetworkErrorInvalidInput, err)
	}
	runCtx, cancel := n.applyDeadline(ctx)
	defer cancel()
	if err := authorize(runCtx, n.policy, ActionNetworkRun, Resource{Kind: ResourceKindNetwork, ID: string(n.definition.ID)}); err != nil {
		return n.fail(runCtx, emitter, runID, input, nil, 0, NetworkErrorUnauthorized, err)
	}
	routes := make([]NetworkRouteRecord, 0, n.maxHops)
	visited := make(map[ToolID]struct{}, n.maxHops)
	// A caller's output schema must not constrain an arbitrary intermediate
	// specialist. Specialists declare their own output contracts; the network
	// validates their handoff presence below.
	current := input
	current.OutputSchema = nil
	var last RunResult
	for hop := 1; hop <= n.maxHops; hop++ {
		if err := runCtx.Err(); err != nil {
			return n.fail(runCtx, emitter, runID, input, routes, hop, NetworkErrorSpecialistFailed, err)
		}
		candidates := n.candidates(visited)
		decision, err := n.router.Route(runCtx, RoutingRequest{Task: task, Candidates: candidates, Hops: hop - 1, PreviousOutput: networkOutput(last)})
		if err != nil {
			return n.fail(runCtx, emitter, runID, input, routes, hop, NetworkErrorSelection, err)
		}
		emitter.emitRouteSelected(runID, hop, decision.SpecialistID, candidateIDs(candidates))
		if decision.Complete {
			if hop == 1 {
				return n.fail(runCtx, emitter, runID, input, routes, hop, NetworkErrorSelection, errors.New("lebro: network router completed before a specialist ran"))
			}
			return n.succeed(runCtx, emitter, runID, input, routes, last)
		}
		specialist, ok := n.specialists[decision.SpecialistID]
		if !ok {
			return n.fail(runCtx, emitter, runID, input, routes, hop, NetworkErrorSelection, fmt.Errorf("lebro: selected specialist %q is not configured", decision.SpecialistID))
		}
		if _, seen := visited[decision.SpecialistID]; seen {
			return n.fail(runCtx, emitter, runID, input, routes, hop, NetworkErrorCycle, fmt.Errorf("lebro: specialist %q already visited", decision.SpecialistID))
		}
		visited[decision.SpecialistID] = struct{}{}
		route := NetworkRouteRecord{Hop: hop, Candidates: candidateIDs(candidates), Selected: decision.SpecialistID}
		child, childErr := specialist.Workflow.Run(runCtx, current)
		route.ChildRunID, route.Status = child.ID, child.Status
		if childErr != nil || child.Status != RunStatusSucceeded || !validNetworkOutput(child) {
			if childErr != nil {
				route.Failure = childErr.Error()
			} else if !validNetworkOutput(child) {
				route.Failure = "lebro: specialist produced no handoff output"
			} else {
				route.Failure = fmt.Sprintf("specialist finished with status %q", child.Status)
			}
			routes = append(routes, route)
			return n.fail(runCtx, emitter, runID, input, routes, hop, NetworkErrorSpecialistFailed, firstErr(childErr, errors.New(route.Failure)))
		}
		routes = append(routes, route)
		last = child
		current = RunInput{Messages: []Message{{Role: RoleUser, Content: networkHandoffPrompt(task, child)}}, ThreadID: input.ThreadID, Metadata: input.Metadata, Memory: input.Memory}
	}
	return n.fail(runCtx, emitter, runID, input, routes, n.maxHops, NetworkErrorHopLimit, fmt.Errorf("lebro: network reached max hops %d", n.maxHops))
}

func (n *Network) candidates(visited map[ToolID]struct{}) []RoutingCandidate {
	result := make([]RoutingCandidate, 0, len(n.specialists))
	for _, id := range sortedNetworkIDs(n.specialists) {
		if _, seen := visited[id]; seen {
			continue
		}
		s := n.specialists[id]
		result = append(result, RoutingCandidate{ID: id, Description: s.Description})
	}
	return result
}

func (n *Network) succeed(ctx context.Context, emitter *runEmitter, id RunID, input RunInput, routes []NetworkRouteRecord, last RunResult) (RunResult, error) {
	result := last
	result.ID, result.Metadata = id, cloneMetadata(input.Metadata)
	result.Status = RunStatusSucceeded
	if err := n.save(ctx, id, input, routes, result, nil); err != nil {
		err := networkPersistError(len(routes), routes, err)
		failed := RunResult{ID: id, Status: RunStatusFailed, Metadata: cloneMetadata(input.Metadata)}
		emitter.terminal(id, len(routes), "", RunEventFailed, RunStatusFailed, err)
		return failed, err
	}
	emitter.terminal(id, len(routes), "", RunEventSucceeded, RunStatusSucceeded, nil)
	return result, nil
}
func (n *Network) fail(ctx context.Context, emitter *runEmitter, id RunID, input RunInput, routes []NetworkRouteRecord, hop int, kind NetworkErrorKind, cause error) (RunResult, error) {
	err := &NetworkError{Kind: kind, Hop: hop, Routes: append([]NetworkRouteRecord(nil), routes...), Err: cause}
	status := RunStatusFailed
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		status = RunStatusCancelled
	}
	result := RunResult{ID: id, Status: status, Metadata: cloneMetadata(input.Metadata)}
	persistCtx, cancel := networkPersistenceContext(ctx)
	defer cancel()
	if persistErr := n.save(persistCtx, id, input, routes, result, err); persistErr != nil {
		err = networkPersistError(hop, routes, fmt.Errorf("%w: persist network route record: %v", err, persistErr))
	}
	eventType := RunEventFailed
	if status == RunStatusCancelled {
		eventType = RunEventCancelled
	}
	emitter.terminal(id, hop, "", eventType, status, err)
	return result, err
}
func (n *Network) save(ctx context.Context, id RunID, input RunInput, routes []NetworkRouteRecord, result RunResult, failure *NetworkError) error {
	if n.store == nil {
		return nil
	}
	now := n.clock.Now()
	outputs := make([]json.RawMessage, 0, len(routes))
	for index, route := range routes {
		raw, err := json.Marshal(route)
		if err != nil {
			return fmt.Errorf("lebro: encode network route %d: %w", index+1, err)
		}
		outputs = append(outputs, raw)
	}
	var output json.RawMessage
	if raw := result.StructuredOutput().Raw(); len(raw) > 0 {
		output = raw
	} else if text := networkOutput(result); text != "" {
		output, _ = json.Marshal(map[string]string{"output": text})
	}
	record := WorkflowRunRecord{ID: id, WorkflowID: n.definition.ID, ThreadID: input.ThreadID, Status: result.Status, Input: networkInputRecord(input), Output: output, StepOutputs: outputs, CurrentStep: len(routes), Metadata: metadataJSON(input.Metadata), StartedAt: now, UpdatedAt: now}
	if result.Status == RunStatusSucceeded || result.Status == RunStatusFailed || result.Status == RunStatusCancelled {
		record.FinishedAt = &now
	}
	if failure != nil {
		record.Failure = &WorkflowFailureData{Kind: WorkflowErrorStepFailed, Step: failure.Hop, Message: failure.Error()}
	}
	return n.store.WorkflowRuns().SaveWorkflowRun(ctx, record)
}

func networkPersistError(hop int, routes []NetworkRouteRecord, cause error) *NetworkError {
	return &NetworkError{Kind: NetworkErrorPersistFailed, Hop: hop, Routes: append([]NetworkRouteRecord(nil), routes...), Err: fmt.Errorf("lebro: persist network route record: %w", cause)}
}

func networkPersistenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}
	// Preserve route history even after the execution context has timed out or
	// been cancelled. Values (including tracing and tenant identity) remain.
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}
func (n *Network) applyDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if n.deadline <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, n.deadline)
}

func networkTask(messages []Message) (string, error) {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == RoleUser && messages[i].Content != "" {
			return messages[i].Content, nil
		}
	}
	return "", errors.New("lebro: network requires a non-empty user message")
}
func networkOutput(result RunResult) string {
	for i := len(result.Messages) - 1; i >= 0; i-- {
		if result.Messages[i].Role == RoleAssistant {
			return result.Messages[i].Content
		}
	}
	return ""
}
func validNetworkOutput(result RunResult) bool {
	return networkOutput(result) != "" || result.StructuredOutput() != ""
}
func networkHandoffPrompt(task string, result RunResult) string {
	output := networkOutput(result)
	if output == "" {
		output = string(result.StructuredOutput().Raw())
	}
	return task + "\n\nPrevious specialist output:\n" + output
}
func candidateIDs(candidates []RoutingCandidate) []ToolID {
	ids := make([]ToolID, len(candidates))
	for i, candidate := range candidates {
		ids[i] = candidate.ID
	}
	return ids
}
func sortedNetworkIDs(m map[ToolID]NetworkSpecialist) []ToolID {
	ids := make([]ToolID, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
func firstErr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}
func networkInputRecord(input RunInput) json.RawMessage {
	raw, _ := json.Marshal(struct {
		Messages []Message         `json:"messages"`
		Metadata map[string]string `json:"metadata,omitempty"`
	}{Messages: input.Messages, Metadata: input.Metadata})
	return raw
}
func metadataJSON(metadata map[string]string) json.RawMessage {
	if len(metadata) == 0 {
		return nil
	}
	raw, _ := json.Marshal(metadata)
	return raw
}
