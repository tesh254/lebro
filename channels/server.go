package channels

import (
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/tesh254/lebro"
)

// Config configures a Channels server. Store binds runs to durable threads and
// backs the default deduplicator; Mapper resolves an inbound conversation to a
// thread; Deduplicator drops redelivered messages. Sensible defaults are
// chosen for any field left unset, so a minimal server needs only a Store.
type Config struct {
	// Store binds agent runs to durable threads and, when no Deduplicator is
	// supplied, backs a persistent default deduplicator. A nil Store runs
	// agents statelessly and, absent an explicit Deduplicator, deduplicates in
	// memory only.
	Store lebro.Store
	// Mapper resolves an inbound message to its thread. When nil, a
	// NamespaceThreadMapper with an empty namespace is used.
	Mapper ThreadMapper
	// Deduplicator drops redelivered messages. When nil, a StoreDeduplicator is
	// used if Store is set, otherwise a MemoryDeduplicator; see NewServer.
	Deduplicator Deduplicator
	// TextFormat is the rendering applied to every reply chunk. The zero value
	// is FormatMarkdown.
	TextFormat TextFormat
	// Middleware wraps the router. The first element is outermost, matching the
	// httpapi convention; use it for logging, rate limiting, and tracing. Per-
	// adapter request authentication is the adapter's Verify, not middleware.
	Middleware []func(http.Handler) http.Handler
}

// Server routes inbound platform webhooks to explicitly registered agents and
// their channel adapters. Only registered agent-adapter pairs are reachable.
// The zero value is not usable; construct one with NewServer.
type Server struct {
	config       Config
	mapper       ThreadMapper
	deduplicator Deduplicator

	mu sync.RWMutex
	// routes maps a webhook path to the bound agent and adapter. A path is
	// /agents/{id}/channels/{platform}/webhook.
	routes map[string]binding

	handlerOnce sync.Once
	handler     http.Handler
}

// binding is one registered agent-adapter pair reachable at a webhook path.
type binding struct {
	agent   *lebro.Agent
	adapter Adapter
}

// NewServer creates a channel server. When Config.Deduplicator is nil it
// selects a StoreDeduplicator if a Store is configured — so deduplication
// survives a restart by default whenever persistence is available — and a
// MemoryDeduplicator otherwise. A nil Mapper selects a zero-namespace
// NamespaceThreadMapper.
func NewServer(config Config) (*Server, error) {
	mapper := config.Mapper
	if mapper == nil {
		mapper = NamespaceThreadMapper{}
	}

	deduplicator := config.Deduplicator
	if deduplicator == nil {
		if config.Store != nil {
			var err error
			deduplicator, err = NewStoreDeduplicator(StoreDeduplicatorConfig{Store: config.Store})
			if err != nil {
				return nil, err
			}
		} else {
			deduplicator = NewMemoryDeduplicator(DefaultDedupCapacity)
		}
	}

	return &Server{
		config:       config,
		mapper:       mapper,
		deduplicator: deduplicator,
		routes:       make(map[string]binding),
	}, nil
}

// ExposeAgent binds an agent to one or more channel adapters, each reachable at
// /agents/{id}/channels/{platform}/webhook where id is the agent's definition
// ID and platform is the adapter's Platform. Registering the same
// agent-platform pair twice, or two adapters reporting the same platform for
// one agent, is an error so a later registration cannot silently shadow an
// earlier one.
//
// Registrations must happen before the first Handler call, because the router
// is built once; a later registration would be unroutable and is refused with
// ErrHandlerBuilt.
func (s *Server) ExposeAgent(agent *lebro.Agent, adapters ...Adapter) error {
	if agent == nil {
		return errors.New("lebro/channels: agent is nil")
	}
	id := string(agent.Definition().ID)
	if id == "" {
		return errors.New("lebro/channels: agent definition ID is required")
	}
	if len(adapters) == 0 {
		return errors.New("lebro/channels: at least one adapter is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handlerBuilt() {
		return fmt.Errorf("lebro/channels: cannot expose agent %q: %w", id, ErrHandlerBuilt)
	}

	// Validate every adapter before registering any, so a bad adapter in the
	// list does not leave the agent half-registered.
	paths := make([]string, 0, len(adapters))
	for _, adapter := range adapters {
		if adapter == nil {
			return fmt.Errorf("lebro/channels: agent %q has a nil adapter", id)
		}
		platform := adapter.Platform()
		if platform == "" {
			return fmt.Errorf("lebro/channels: agent %q adapter has an empty platform", id)
		}
		path := webhookPath(id, platform)
		if _, exists := s.routes[path]; exists {
			return fmt.Errorf("lebro/channels: agent %q platform %q is already exposed", id, platform)
		}
		for _, seen := range paths {
			if seen == path {
				return fmt.Errorf("lebro/channels: agent %q has duplicate platform %q", id, platform)
			}
		}
		paths = append(paths, path)
	}

	for i, adapter := range adapters {
		s.routes[paths[i]] = binding{agent: agent, adapter: adapter}
	}
	return nil
}

// ErrHandlerBuilt is returned by ExposeAgent when the router has already been
// built by a call to Handler.
var ErrHandlerBuilt = errors.New("lebro/channels: handler already built")

// handlerBuilt reports whether the router has been constructed. Must be called
// with s.mu held.
func (s *Server) handlerBuilt() bool { return s.handler != nil }

// webhookPath returns the webhook route for an agent and platform.
func webhookPath(agentID, platform string) string {
	return "/agents/" + agentID + "/channels/" + platform + "/webhook"
}

// Handler returns the HTTP handler serving every registered webhook route,
// wrapped in the configured middleware. The router is built once on the first
// call and reused, so the returned handler is stable and safe for concurrent
// use.
func (s *Server) Handler() http.Handler {
	s.handlerOnce.Do(func() {
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

// buildRouter constructs the mux from the registered routes and applies
// middleware. Middleware is applied in reverse so the first configured entry
// ends up outermost.
func (s *Server) buildRouter() http.Handler {
	mux := http.NewServeMux()
	s.mu.RLock()
	for path, b := range s.routes {
		binding := b
		// A webhook is a POST from the platform; registering the method keeps a
		// GET probe from being routed to the receiver.
		mux.Handle(http.MethodPost+" "+path, s.webhookHandler(binding))
	}
	s.mu.RUnlock()

	var handler http.Handler = mux
	for i := len(s.config.Middleware) - 1; i >= 0; i-- {
		if s.config.Middleware[i] == nil {
			continue
		}
		handler = s.config.Middleware[i](handler)
	}
	return handler
}
