package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/tesh254/lebro"
)

// ServerConfig configures an HTTP server over registered lebro primitives.
type ServerConfig struct {
	// Title identifies the API in the generated OpenAPI document. When empty,
	// "lebro" is used.
	Title string
	// Version is the API version reported in the generated OpenAPI document.
	// When empty, "0.0.0" is used.
	Version string
	// Description is optional prose for the generated OpenAPI document.
	Description string
	// Middleware wraps the router. Entries are applied so that the first
	// element is outermost: it sees every request before later entries and
	// before any handler. Use it for authentication, logging, rate limiting,
	// and tracing; this package deliberately implements none of them.
	Middleware []func(http.Handler) http.Handler
	// Redactor rewrites each stream delta before it is serialized. A nil
	// Redactor selects DefaultRedactor, which removes model-supplied tool-call
	// arguments and reasoning. Pass PassthroughRedactor only for trusted clients.
	Redactor Redactor
	// Store backs the thread routes and lets a run bind to a durable thread
	// through the thread_id query parameter. When nil, thread routes report
	// not-found and thread_id is rejected, because a thread cannot be resolved
	// without storage.
	Store lebro.Store
	// RuntimeStore is the capability-based alternative for thread routes. It
	// needs only the Transcript capability; it does not require a full legacy
	// Store or Lebro-owned migrations. Store and RuntimeStore are mutually
	// exclusive.
	RuntimeStore lebro.RuntimeStore
}

// Server routes HTTP requests to explicitly registered lebro agents and
// workflows. Only registered primitives are reachable. The zero value is not
// usable; construct one with NewServer.
type Server struct {
	config     ServerConfig
	transcript lebro.TranscriptStore
	configErr  error

	mu        sync.RWMutex
	agents    map[string]*lebro.Agent
	workflows map[string]*lebro.LinearWorkflow

	// handlerOnce guards lazy router construction so Handler is safe to call
	// concurrently and repeatedly.
	handlerOnce sync.Once
	handler     http.Handler
}

// NewServer creates an HTTP server that exposes lebro primitives. The server
// has no agents and no workflows until ExposeAgent or ExposeWorkflow is called;
// until then only the health, list, and OpenAPI routes return content.
func NewServer(config ServerConfig) *Server {
	server, _ := NewServerE(config)
	return server
}

// NewServerE is the error-returning constructor for callers that want to fail
// startup when a RuntimeStore does not provide a usable transcript capability.
// NewServer remains available for source compatibility; its Handler returns a
// configuration failure rather than silently treating a bad adapter as absent.
func NewServerE(config ServerConfig) (*Server, error) {
	if config.Title == "" {
		config.Title = "lebro"
	}
	if config.Version == "" {
		config.Version = "0.0.0"
	}
	if config.Redactor == nil {
		config.Redactor = DefaultRedactor
	}
	server := &Server{
		config:    config,
		agents:    make(map[string]*lebro.Agent),
		workflows: make(map[string]*lebro.LinearWorkflow),
	}
	if config.Store != nil && config.RuntimeStore != nil {
		server.configErr = errors.New("lebro/httpapi: store and runtime store are mutually exclusive")
	} else if config.RuntimeStore != nil {
		if !config.RuntimeStore.Capabilities().Transcript {
			server.configErr = &lebro.StoreCapabilityError{Capability: lebro.StoreCapabilityTranscript, Feature: "http thread routes", Reason: "the attached storage adapter does not advertise it"}
		} else if transcript, ok := config.RuntimeStore.(lebro.TranscriptStore); ok && transcript != nil && !isNil(transcript.Threads()) && !isNil(transcript.Messages()) {
			server.transcript = transcript
		} else {
			server.configErr = &lebro.StoreCapabilityError{Capability: lebro.StoreCapabilityTranscript, Feature: "http thread routes", Reason: "the adapter returned a nil transcript repository"}
		}
	}
	return server, server.configErr
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func (s *Server) threadStore() (lebro.ThreadRepository, lebro.MessageRepository, bool) {
	if s.configErr != nil {
		return nil, nil, false
	}
	if s.transcript != nil {
		return s.transcript.Threads(), s.transcript.Messages(), true
	}
	if s.config.Store == nil {
		return nil, nil, false
	}
	return s.config.Store.Threads(), s.config.Store.Messages(), true
}

// ExposeAgent makes an agent reachable at /agents/{id}/runs and
// /agents/{id}/runs/stream, and /agents/{id}/runs/ai-sdk/stream, where id is
// the agent's definition ID. Registering
// the same ID twice is an error, so a later registration cannot silently
// shadow an earlier one.
//
// Registrations must happen before the first call to Handler. The router is
// built once, on the first Handler call, so a later registration would not be
// reachable; ErrHandlerBuilt reports that rather than registering an agent that
// appears in listings but has no route.
func (s *Server) ExposeAgent(agent *lebro.Agent) error {
	if agent == nil {
		return errors.New("lebro/httpapi: agent is nil")
	}
	id := string(agent.Definition().ID)
	if id == "" {
		return errors.New("lebro/httpapi: agent definition ID is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handlerBuilt() {
		return fmt.Errorf("lebro/httpapi: cannot expose agent %q: %w", id, ErrHandlerBuilt)
	}
	if _, exists := s.agents[id]; exists {
		return fmt.Errorf("lebro/httpapi: agent %q is already exposed", id)
	}
	s.agents[id] = agent
	return nil
}

// ExposeWorkflow makes a workflow reachable at /workflows/{id}/runs, where id
// is the workflow's definition ID. Registering the same ID twice is an error.
//
// As with ExposeAgent, registrations must happen before the first Handler call.
func (s *Server) ExposeWorkflow(workflow *lebro.LinearWorkflow) error {
	if workflow == nil {
		return errors.New("lebro/httpapi: workflow is nil")
	}
	id := string(workflow.Definition().ID)
	if id == "" {
		return errors.New("lebro/httpapi: workflow definition ID is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handlerBuilt() {
		return fmt.Errorf("lebro/httpapi: cannot expose workflow %q: %w", id, ErrHandlerBuilt)
	}
	if _, exists := s.workflows[id]; exists {
		return fmt.Errorf("lebro/httpapi: workflow %q is already exposed", id)
	}
	s.workflows[id] = workflow
	return nil
}

// ErrHandlerBuilt is returned by ExposeAgent and ExposeWorkflow when the router
// has already been built by a call to Handler. A primitive registered after
// that point would be listed but unroutable, so the registration is refused
// instead.
var ErrHandlerBuilt = errors.New("lebro/httpapi: handler already built")

// handlerBuilt reports whether the router has been constructed. It must be
// called with s.mu held.
func (s *Server) handlerBuilt() bool { return s.handler != nil }

// Handler returns the HTTP handler serving every route in the table, wrapped in
// the configured middleware. The router is built once on the first call and
// reused afterwards, so the returned handler is stable and safe for concurrent
// use.
func (s *Server) Handler() http.Handler {
	s.handlerOnce.Do(func() {
		if s.configErr != nil {
			built := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeError(w, r, ErrorCodeInvalidRequest)
			})
			s.mu.Lock()
			s.handler = built
			s.mu.Unlock()
			return
		}
		built := s.buildRouter()
		s.mu.Lock()
		s.handler = built
		s.mu.Unlock()
	})
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.handler
}

// ServeHTTP lets a Server be used directly as an http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Handler().ServeHTTP(w, r)
}

// buildRouter constructs the mux from the route table and applies middleware.
// Middleware is applied in reverse so the first configured entry ends up
// outermost.
func (s *Server) buildRouter() http.Handler {
	mux := http.NewServeMux()
	// Index the methods each path serves so a request to a real path with the
	// wrong method can be answered 405 rather than 404. Without this the
	// mismatch falls through to the catch-all and tells the client the resource
	// does not exist, which is both misleading and leaves ErrorCodeMethodNotAllowed
	// unreachable.
	methodsByPath := map[string][]string{}
	for _, r := range routes() {
		mux.Handle(r.pattern(), s.handlerForRoute(r))
		methodsByPath[r.path] = append(methodsByPath[r.path], r.method)
		// net/http answers HEAD from a GET pattern automatically. Registering
		// it explicitly keeps the served surface equal to the documented one,
		// which is what lets the route-coverage test mean something.
		if r.method == http.MethodGet {
			mux.Handle(http.MethodHead+" "+r.path, s.handlerForRoute(r))
			methodsByPath[r.path] = append(methodsByPath[r.path], http.MethodHead)
		}
	}

	// A request that matches no route at all reaches the root pattern. Serving
	// a typed JSON body there keeps every response on this server shaped the
	// same way, rather than mixing in net/http's plain-text 404.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if allowed, ok := methodsByPath[matchedPath(methodsByPath, r.URL.EscapedPath())]; ok {
			sort.Strings(allowed)
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			writeError(w, r, ErrorCodeMethodNotAllowed)
			return
		}
		writeError(w, r, ErrorCodeNotFound)
	})

	var handler http.Handler = mux
	for i := len(s.config.Middleware) - 1; i >= 0; i-- {
		if s.config.Middleware[i] == nil {
			continue
		}
		handler = s.config.Middleware[i](handler)
	}
	return handler
}

// matchedPath returns the route template whose shape matches the concrete
// request path, or "" when none does. It is used only to distinguish a
// wrong-method request to a real path from a request to a path that does not
// exist; the mux itself does the real routing.
//
// path must be the escaped path (url.URL.EscapedPath), because that is what the
// mux splits on. Passing the decoded url.URL.Path instead would disagree with
// the mux wherever a segment contains an escaped separator: an agent whose ID
// is "team/assistant" is addressed as "/agents/team%2Fassistant/runs", which the
// mux routes as three segments while the decoded form has four. The classifier
// would then find no template and report 404 for a path the mux serves on
// another method.
//
// Matching must agree with the mux in the other direction too, or the
// classification is worse than none: "/health/" is not a path the mux serves,
// so reporting 405 there would tell a client that GET is not allowed on a route
// whose only method is GET. Trailing and interior empty segments are therefore
// significant and are compared rather than trimmed.
//
// A template matches when it has the same segments and every non-wildcard
// segment is equal. Wildcards match any single non-empty segment, mirroring
// net/http's "{id}". Literal segments are compared after unescaping so a client
// that percent-encodes an ordinary character — "/agents/%61lpha/runs" — is
// still recognized, matching how the mux resolves them.
func matchedPath(templates map[string][]string, path string) string {
	// Strip only the leading empty segment produced by the mandatory "/"
	// prefix; every later empty segment is a real, distinguishing part of the
	// request path.
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for template := range templates {
		candidate := strings.Split(strings.TrimPrefix(template, "/"), "/")
		if len(candidate) != len(segments) {
			continue
		}
		matched := true
		for i, segment := range candidate {
			if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
				if segments[i] == "" {
					matched = false
					break
				}
				continue
			}
			if segment != unescapeSegment(segments[i]) {
				matched = false
				break
			}
		}
		if matched {
			return template
		}
	}
	return ""
}

// unescapeSegment decodes one percent-encoded path segment, returning it
// unchanged when it is not valid encoding. A malformed segment cannot match a
// literal template segment anyway, so refusing to fail on it keeps the
// classifier total.
func unescapeSegment(segment string) string {
	decoded, err := url.PathUnescape(segment)
	if err != nil {
		return segment
	}
	return decoded
}

// handlerForRoute returns the handler for one table entry. The switch is on
// operationID so every route in the table must be given a handler here; a new
// entry with no case fails fast at construction rather than serving a nil
// handler at request time.
func (s *Server) handlerForRoute(r route) http.Handler {
	switch r.operationID {
	case "getHealth":
		return http.HandlerFunc(s.handleHealth)
	case "listAgents":
		return http.HandlerFunc(s.handleListAgents)
	case "createAgentRun":
		return http.HandlerFunc(s.handleAgentRun)
	case "streamAgentRun":
		return http.HandlerFunc(s.handleAgentStream)
	case "streamAgentRunAISDK":
		return http.HandlerFunc(s.handleAgentAISDKStream)
	case "listWorkflows":
		return http.HandlerFunc(s.handleListWorkflows)
	case "createWorkflowRun":
		return http.HandlerFunc(s.handleWorkflowRun)
	case "getThread":
		return http.HandlerFunc(s.handleGetThread)
	case "listThreadMessages":
		return http.HandlerFunc(s.handleListMessages)
	case "getOpenAPI":
		return http.HandlerFunc(s.handleOpenAPI)
	default:
		panic(fmt.Sprintf("lebro/httpapi: no handler for operation %q", r.operationID))
	}
}

// lookupAgent resolves an exposed agent by ID.
func (s *Server) lookupAgent(id string) (*lebro.Agent, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	agent, ok := s.agents[id]
	return agent, ok
}

// lookupWorkflow resolves an exposed workflow by ID.
func (s *Server) lookupWorkflow(id string) (*lebro.LinearWorkflow, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	workflow, ok := s.workflows[id]
	return workflow, ok
}

// agentSummaries lists exposed agents in stable ID order so a client polling
// the listing sees a consistent sequence rather than Go's randomized map order.
func (s *Server) agentSummaries() []AgentSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	summaries := make([]AgentSummary, 0, len(s.agents))
	for id, agent := range s.agents {
		definition := agent.Definition()
		summaries = append(summaries, AgentSummary{
			ID:          id,
			Name:        definition.Name,
			Description: definition.Description,
		})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].ID < summaries[j].ID })
	return summaries
}

// workflowSummaries lists exposed workflows in stable ID order.
func (s *Server) workflowSummaries() []WorkflowSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	summaries := make([]WorkflowSummary, 0, len(s.workflows))
	for id, workflow := range s.workflows {
		definition := workflow.Definition()
		summaries = append(summaries, WorkflowSummary{
			ID:          id,
			Name:        definition.Name,
			Description: definition.Description,
			Version:     definition.Version,
			InputSchema: workflow.InputSchema(),
		})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].ID < summaries[j].ID })
	return summaries
}

// counts reports how many primitives are exposed.
func (s *Server) counts() (agents int, workflows int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.agents), len(s.workflows)
}
