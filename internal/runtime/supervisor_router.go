package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// RoutingRequest is the stable, provider-neutral input given to a specialist
// router. Candidates have stable ID order so deterministic routers can make a
// repeatable choice without consulting a model.
type RoutingRequest struct {
	Task       string
	Candidates []RoutingCandidate
	// Hops is number of completed specialist handoffs. Existing routers may
	// ignore it; Network routers can use it to stop bounded traversal.
	Hops int
	// PreviousOutput is final assistant text from prior specialist, if any.
	PreviousOutput string
}

// RoutingCandidate describes one supervised delegation target.
type RoutingCandidate struct {
	ID          ToolID
	Description string
}

// RoutingDecision chooses the first specialist to attempt. Fallback order is
// owned by RoutedSubagentConfig, so a router cannot silently widen execution.
type RoutingDecision struct {
	SpecialistID ToolID
	// Complete ends a Network after at least one successful specialist. It is
	// ignored by RoutedSubagent, preserving its single-handoff behavior.
	Complete bool
}

// SpecialistRouter selects one configured specialist for a focused task.
// Implementations must return one of request.Candidates.
type SpecialistRouter interface {
	Route(context.Context, RoutingRequest) (RoutingDecision, error)
}

// RouteRule is evaluated in declaration order by RuleRouter. The first match
// wins, making routing deterministic and easy to replay from task input.
type RouteRule struct {
	SpecialistID ToolID
	Match        func(RoutingRequest) bool
}

// RuleRouter selects a specialist through ordered application-owned rules.
// Default is used when no rule matches.
type RuleRouter struct {
	rules     []RouteRule
	defaultID ToolID
}

// NewRuleRouter validates and builds a deterministic specialist router.
func NewRuleRouter(rules []RouteRule, defaultID ToolID) (*RuleRouter, error) {
	if defaultID == "" {
		return nil, errors.New("lebro: rule router default specialist is required")
	}
	cloned := make([]RouteRule, len(rules))
	copy(cloned, rules)
	for i, rule := range cloned {
		if rule.SpecialistID == "" || rule.Match == nil {
			return nil, fmt.Errorf("lebro: rule router rule %d needs specialist ID and match", i)
		}
	}
	return &RuleRouter{rules: cloned, defaultID: defaultID}, nil
}

// Route evaluates rules in declaration order, then uses Default.
func (r *RuleRouter) Route(_ context.Context, request RoutingRequest) (RoutingDecision, error) {
	if r == nil {
		return RoutingDecision{}, errors.New("lebro: rule router is nil")
	}
	// Rule routers are single-handoff strategies. A Network can opt into
	// multi-hop traversal with a router that inspects RoutingRequest.Hops;
	// ending here keeps existing declarative rules useful without inventing a
	// second termination callback.
	if request.Hops > 0 {
		return RoutingDecision{Complete: true}, nil
	}
	for _, rule := range r.rules {
		if rule.Match(request) {
			return RoutingDecision{SpecialistID: rule.SpecialistID}, nil
		}
	}
	return RoutingDecision{SpecialistID: r.defaultID}, nil
}

// ModelSpecialistRouterConfig configures one provider-neutral model call that
// selects a specialist. Model must reply with JSON containing specialist_id.
type ModelSpecialistRouterConfig struct {
	Model        Model
	ModelName    string
	Instructions string
}

// ModelSpecialistRouter asks a Model to select a configured specialist. It
// never invokes a specialist itself and validates model output locally.
type ModelSpecialistRouter struct {
	model        Model
	modelName    string
	instructions string
}

// NewModelSpecialistRouter builds a model-backed specialist router.
func NewModelSpecialistRouter(config ModelSpecialistRouterConfig) (*ModelSpecialistRouter, error) {
	if config.Model == nil || isNilInterface(config.Model) {
		return nil, errors.New("lebro: model specialist router model is required")
	}
	if config.ModelName == "" {
		return nil, errors.New("lebro: model specialist router model name is required")
	}
	return &ModelSpecialistRouter{model: config.Model, modelName: config.ModelName, instructions: config.Instructions}, nil
}

// Route asks the model for a JSON specialist_id, then confirms that ID is an
// available candidate. Structured output is preferred; plain JSON content is
// accepted for adapters without structured-output support.
func (r *ModelSpecialistRouter) Route(ctx context.Context, request RoutingRequest) (RoutingDecision, error) {
	if r == nil || r.model == nil || isNilInterface(r.model) {
		return RoutingDecision{}, errors.New("lebro: model specialist router is nil")
	}
	if err := ctx.Err(); err != nil {
		return RoutingDecision{}, err
	}
	prompt, err := json.Marshal(struct {
		Task           string             `json:"task"`
		Candidates     []RoutingCandidate `json:"candidates"`
		Hops           int                `json:"hops,omitempty"`
		PreviousOutput string             `json:"previous_output,omitempty"`
	}{Task: request.Task, Candidates: request.Candidates, Hops: request.Hops, PreviousOutput: request.PreviousOutput})
	if err != nil {
		return RoutingDecision{}, fmt.Errorf("lebro: encode routing request: %w", err)
	}
	messages := []Message{{Role: RoleUser, Content: string(prompt)}}
	if r.instructions != "" {
		messages = append([]Message{{Role: RoleSystem, Content: r.instructions}}, messages...)
	}
	response, err := r.model.Generate(ctx, ModelRequest{
		Model:        r.modelName,
		Messages:     messages,
		OutputSchema: &ModelOutputSchema{Name: "specialist_route", Strict: true, Schema: json.RawMessage(`{"type":"object","properties":{"specialist_id":{"type":"string"},"complete":{"type":"boolean"}},"anyOf":[{"required":["specialist_id"]},{"required":["complete"]}],"additionalProperties":false}`)},
	})
	if err != nil {
		return RoutingDecision{}, fmt.Errorf("lebro: select specialist: %w", err)
	}
	if err := response.Validate(); err != nil {
		return RoutingDecision{}, fmt.Errorf("lebro: invalid specialist route response: %w", err)
	}
	raw := response.Message.StructuredOutput.Raw()
	if len(raw) == 0 {
		raw = json.RawMessage(response.Message.Content)
	}
	var selected struct {
		SpecialistID ToolID `json:"specialist_id"`
		Complete     bool   `json:"complete"`
	}
	if err := json.Unmarshal(raw, &selected); err != nil || (selected.SpecialistID == "" && !selected.Complete) {
		if err == nil {
			err = errors.New("specialist_id is required")
		}
		return RoutingDecision{}, fmt.Errorf("lebro: decode specialist route: %w", err)
	}
	if selected.Complete {
		return RoutingDecision{Complete: true}, nil
	}
	if !hasRoutingCandidate(request.Candidates, selected.SpecialistID) {
		return RoutingDecision{}, fmt.Errorf("lebro: selected specialist %q is not a candidate", selected.SpecialistID)
	}
	return RoutingDecision{SpecialistID: selected.SpecialistID}, nil
}

// RouteErrorKind identifies failures before a specialist returns a successful
// delegation result.
type RouteErrorKind string

const (
	RouteErrorSelection RouteErrorKind = "route_selection_failed"
	RouteErrorExhausted RouteErrorKind = "route_fallback_exhausted"
)

var (
	ErrRouteSelection = errors.New("lebro: route selection failed")
	ErrRouteExhausted = errors.New("lebro: route fallback exhausted")
)

// RouteError preserves selected and attempted specialists for logs, traces,
// and callers deciding whether to surface or retry a route failure.
type RouteError struct {
	Kind      RouteErrorKind
	Selected  ToolID
	Attempted []ToolID
	Err       error
}

const routedSubagentOutputSchema = `{
	"type":"object",
	"required":["agent_id","run_id","status","output","route"],
	"properties":{
		"agent_id":{"type":"string"},
		"run_id":{"type":"string"},
		"status":{"type":"string"},
		"output":{"type":"string"},
		"route":{
			"type":"object",
			"required":["selected","candidates","attempted","child_run_ids"],
			"properties":{
				"selected":{"type":"string"},
				"candidates":{"type":"array","items":{"type":"string"}},
				"attempted":{"type":"array","items":{"type":"string"}},
				"child_run_ids":{"type":"array","items":{"type":"string"}}
			},
			"additionalProperties":false
		}
	},
	"additionalProperties":false
}`

type routedSubagentOutput struct {
	subagentOutput
	Route struct {
		Selected    ToolID   `json:"selected"`
		Candidates  []ToolID `json:"candidates"`
		Attempted   []ToolID `json:"attempted"`
		ChildRunIDs []RunID  `json:"child_run_ids"`
	} `json:"route"`
}

func (e *RouteError) Error() string {
	if e == nil {
		return "lebro: route failure"
	}
	if e.Err == nil {
		return fmt.Sprintf("lebro: route %s", e.Kind)
	}
	return fmt.Sprintf("lebro: route %s: %v", e.Kind, e.Err)
}
func (e *RouteError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}
func (e *RouteError) Is(target error) bool {
	return e != nil && ((target == ErrRouteSelection && e.Kind == RouteErrorSelection) || (target == ErrRouteExhausted && e.Kind == RouteErrorExhausted))
}

// RoutedSubagentConfig composes an explicit router with NewSubagent targets.
// All targets must use the default Subagent input and output contracts so the
// wrapper has one schema-stable supervisor tool surface.
type RoutedSubagentConfig struct {
	ID          ToolID
	Description string
	Router      SpecialistRouter
	Specialists []*Subagent
	Fallback    []ToolID
}

// RoutedSubagent is a schema-backed supervisor tool. Register it in the same
// ToolRegistry as any other tool: registry validation, policy authorization,
// and approval-gate contexts stay on the existing NewSubagent path.
type RoutedSubagent struct {
	definition  ToolDefinition
	router      SpecialistRouter
	specialists map[ToolID]*Subagent
	fallback    []ToolID
}

var _ Tool = (*RoutedSubagent)(nil)

// NewRoutedSubagent builds one common routed delegation capability.
func NewRoutedSubagent(config RoutedSubagentConfig) (*RoutedSubagent, error) {
	if config.ID == "" {
		return nil, errors.New("lebro: routed subagent ID is required")
	}
	if config.Router == nil || isNilInterface(config.Router) {
		return nil, errors.New("lebro: routed subagent router is required")
	}
	if len(config.Specialists) == 0 {
		return nil, errors.New("lebro: routed subagent needs specialists")
	}
	specialists := make(map[ToolID]*Subagent, len(config.Specialists))
	for _, specialist := range config.Specialists {
		if specialist == nil {
			return nil, errors.New("lebro: routed subagent specialist is nil")
		}
		definition := specialist.Definition()
		if definition.ID == "" || !rawJSONEqual(definition.InputSchema, json.RawMessage(subagentInputSchema)) || !rawJSONEqual(definition.OutputSchema, json.RawMessage(subagentOutputSchema)) {
			return nil, fmt.Errorf("lebro: routed subagent specialist %q must use default subagent schemas", definition.ID)
		}
		if _, exists := specialists[definition.ID]; exists {
			return nil, fmt.Errorf("lebro: routed subagent duplicate specialist %q", definition.ID)
		}
		specialists[definition.ID] = specialist
	}
	fallback := append([]ToolID(nil), config.Fallback...)
	seenFallback := make(map[ToolID]struct{}, len(fallback))
	for _, id := range fallback {
		if _, ok := specialists[id]; !ok {
			return nil, fmt.Errorf("lebro: routed subagent fallback %q is not a specialist", id)
		}
		if _, exists := seenFallback[id]; exists {
			return nil, fmt.Errorf("lebro: routed subagent duplicate fallback %q", id)
		}
		seenFallback[id] = struct{}{}
	}
	return &RoutedSubagent{definition: ToolDefinition{ID: config.ID, Description: config.Description, InputSchema: json.RawMessage(subagentInputSchema), OutputSchema: json.RawMessage(routedSubagentOutputSchema)}, router: config.Router, specialists: specialists, fallback: fallback}, nil
}

func (s *RoutedSubagent) Definition() ToolDefinition {
	if s == nil {
		return ToolDefinition{}
	}
	return cloneToolDefinition(s.definition)
}

// Execute chooses a specialist, invokes it through NewSubagent, and retries
// only failed/unavailable specialists. Cancellation and approval decisions do
// not fan out into another specialist, avoiding duplicate side effects.
func (s *RoutedSubagent) Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	if s == nil {
		return nil, errors.New("lebro: routed subagent is nil")
	}
	if ctx == nil {
		return nil, errors.New("lebro: routed subagent context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var decoded subagentInput
	if err := json.Unmarshal(input, &decoded); err != nil || decoded.Task == "" {
		if err == nil {
			err = errors.New("task is required")
		}
		return nil, &SubagentError{Kind: SubagentErrorInvalidInput, ID: s.definition.ID, Err: fmt.Errorf("lebro: decode routed subagent input: %w", err)}
	}
	candidates := s.candidates()
	decision, err := s.router.Route(ctx, RoutingRequest{Task: decoded.Task, Candidates: candidates})
	if err != nil {
		return nil, &RouteError{Kind: RouteErrorSelection, Err: err}
	}
	if _, ok := s.specialists[decision.SpecialistID]; !ok {
		return nil, &RouteError{Kind: RouteErrorSelection, Selected: decision.SpecialistID, Err: fmt.Errorf("lebro: selected specialist %q is not configured", decision.SpecialistID)}
	}
	attempts := make([]ToolID, 0, 1+len(s.fallback))
	childRunIDs := make([]RunID, 0, 1+len(s.fallback))
	var lastErr error
	attempt := func(id ToolID) (json.RawMessage, error) {
		if containsToolID(attempts, id) {
			return nil, nil
		}
		attempts = append(attempts, id)
		output, err := s.specialists[id].Execute(ctx, input)
		appendSubagentRunID(&childRunIDs, err)
		if err == nil {
			return s.encodeRouteOutput(output, decision.SpecialistID, candidates, attempts, childRunIDs)
		}
		lastErr = err
		return nil, err
	}
	if output, err := attempt(decision.SpecialistID); err == nil && output != nil {
		return output, nil
	} else if err != nil && !routeFallbackable(err) {
		return nil, &RouteError{Kind: RouteErrorExhausted, Selected: decision.SpecialistID, Attempted: attempts, Err: err}
	}
	for _, id := range s.fallback {
		output, err := attempt(id)
		if err == nil && output != nil {
			return output, nil
		}
		if err != nil && !routeFallbackable(err) {
			return nil, &RouteError{Kind: RouteErrorExhausted, Selected: decision.SpecialistID, Attempted: attempts, Err: err}
		}
	}
	return nil, &RouteError{Kind: RouteErrorExhausted, Selected: decision.SpecialistID, Attempted: attempts, Err: lastErr}
}

func (s *RoutedSubagent) encodeRouteOutput(output json.RawMessage, selected ToolID, candidates []RoutingCandidate, attempts []ToolID, childRunIDs []RunID) (json.RawMessage, error) {
	// The child result already passed the child schema boundary. Decode only to
	// re-embed its stable fields beside the route trace emitted to supervisor.
	var child subagentOutput
	if err := json.Unmarshal(output, &child); err != nil {
		return nil, fmt.Errorf("lebro: decode routed subagent result: %w", err)
	}
	result := routedSubagentOutput{subagentOutput: child}
	result.Route.Selected = selected
	result.Route.Attempted = append([]ToolID(nil), attempts...)
	result.Route.ChildRunIDs = append([]RunID(nil), childRunIDs...)
	if !containsRunID(result.Route.ChildRunIDs, RunID(child.RunID)) {
		result.Route.ChildRunIDs = append(result.Route.ChildRunIDs, RunID(child.RunID))
	}
	result.Route.Candidates = make([]ToolID, len(candidates))
	for i, candidate := range candidates {
		result.Route.Candidates[i] = candidate.ID
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("lebro: encode routed subagent result: %w", err)
	}
	return encoded, nil
}

func (s *RoutedSubagent) candidates() []RoutingCandidate {
	result := make([]RoutingCandidate, 0, len(s.specialists))
	ids := make([]ToolID, 0, len(s.specialists))
	for id := range s.specialists {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		specialist := s.specialists[id]
		result = append(result, RoutingCandidate{ID: id, Description: specialist.Definition().Description})
	}
	return result
}

func hasRoutingCandidate(candidates []RoutingCandidate, id ToolID) bool {
	for _, candidate := range candidates {
		if candidate.ID == id {
			return true
		}
	}
	return false
}
func containsToolID(ids []ToolID, id ToolID) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

func containsRunID(ids []RunID, id RunID) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

func appendSubagentRunID(ids *[]RunID, err error) {
	var subagentErr *SubagentError
	if errors.As(err, &subagentErr) && subagentErr.RunID != "" && !containsRunID(*ids, subagentErr.RunID) {
		*ids = append(*ids, subagentErr.RunID)
	}
}

// Route fallback is safe only before a specialist produces a successful
// result. Invalid delegation input has no child side effects, so it may try an
// equivalent fallback; cancellation and approval outcomes never reroute.
func routeFallbackable(err error) bool {
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrSubagentCancelled) && !errors.Is(err, ErrApprovalRejected) && !errors.Is(err, ErrApprovalExpired) && !errors.Is(err, ErrApprovalInvalidDecision) && (errors.Is(err, ErrSubagentRunFailed) || errors.Is(err, ErrSubagentInvalidInput))
}
