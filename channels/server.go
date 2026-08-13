package channels

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
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
	// building is set under mu once router construction starts, so an
	// ExposeAgent that blocks on mu during the build is refused rather than
	// registering a route the snapshot has already missed.
	building bool
}

// binding is one registered agent-adapter pair reachable at a webhook path.
type binding struct {
	agentID string
	agent   *lebro.Agent
	adapter Adapter
}

// dedupKey scopes a provider message ID to this binding's agent and platform so
// a shared deduplicator does not conflate equal IDs from different routes. The
// components are length-prefixed so no component's contents can shift a boundary
// and alias a different scoping.
func (b binding) dedupKey(providerMessageID string) string {
	return string(lengthPrefixed(b.agentID, b.adapter.Platform(), providerMessageID))
}

// NewServer creates a channel server. When Config.Deduplicator is nil it
// selects a StoreDeduplicator if a Store is configured — so deduplication
// survives a restart by default whenever persistence is available — and a
// MemoryDeduplicator otherwise. A nil Mapper selects a zero-namespace
// NamespaceThreadMapper.
func NewServer(config Config) (*Server, error) {
	// Interface-valued config fields are treated as unset when they are nil or a
	// typed nil. Retaining a typed nil here would defeat the default selection
	// below and panic on the first request instead.
	if isNilInterface(config.Store) {
		config.Store = nil
	}

	mapper := config.Mapper
	if isNilInterface(mapper) {
		mapper = NamespaceThreadMapper{}
	}

	deduplicator := config.Deduplicator
	if isNilInterface(deduplicator) {
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
	// The agent ID is a path segment in the webhook route. Reject a value that
	// is not a plain segment so an ID carrying ServeMux pattern syntax (for
	// example "{id}") cannot register a wildcard route that captures other
	// agents' paths.
	if err := validateRouteSegment(id); err != nil {
		return fmt.Errorf("lebro/channels: agent ID %q is not a valid route segment: %w", id, err)
	}
	if len(adapters) == 0 {
		return errors.New("lebro/channels: at least one adapter is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handlerBuilt() || s.building {
		return fmt.Errorf("lebro/channels: cannot expose agent %q: %w", id, ErrHandlerBuilt)
	}

	// Validate every adapter before registering any, so a bad adapter in the
	// list does not leave the agent half-registered.
	paths := make([]string, 0, len(adapters))
	for _, adapter := range adapters {
		// A typed-nil adapter is non-nil as an interface, so the plain nil check
		// alone would pass and Platform() would then panic. Reflect to reject it.
		if isNilAdapter(adapter) {
			return fmt.Errorf("lebro/channels: agent %q has a nil adapter", id)
		}
		platform := adapter.Platform()
		if platform == "" {
			return fmt.Errorf("lebro/channels: agent %q adapter has an empty platform", id)
		}
		if err := validateRouteSegment(platform); err != nil {
			return fmt.Errorf("lebro/channels: platform %q is not a valid route segment: %w", platform, err)
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
		s.routes[paths[i]] = binding{agentID: id, agent: agent, adapter: adapter}
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
		// Snapshot the routes and assign the handler under one uninterrupted
		// hold of the write lock. Taking the lock here — rather than snapshotting
		// under a separate read lock and assigning under a later write lock —
		// closes the race where an ExposeAgent that already passed its
		// handler-built check writes a route between the snapshot and the
		// assignment, returning nil while leaving that route unrouted. A
		// concurrent ExposeAgent either commits its route before this lock (and
		// is included) or blocks and then observes s.building and is refused.
		s.mu.Lock()
		s.building = true
		s.handler = s.buildRouterLocked()
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

// buildRouterLocked constructs the mux from the registered routes and applies
// middleware. Middleware is applied in reverse so the first configured entry
// ends up outermost. It must be called with s.mu held.
func (s *Server) buildRouterLocked() http.Handler {
	mux := http.NewServeMux()
	for path, b := range s.routes {
		binding := b
		// A webhook is a POST from the platform; registering the method keeps a
		// GET probe from being routed to the receiver.
		mux.Handle(http.MethodPost+" "+path, s.webhookHandler(binding))
	}

	var handler http.Handler = mux
	for i := len(s.config.Middleware) - 1; i >= 0; i-- {
		if s.config.Middleware[i] == nil {
			continue
		}
		handler = s.config.Middleware[i](handler)
	}
	return handler
}

// validateRouteSegment reports whether s is usable as a single webhook path
// segment. It rejects an empty value, a value containing a slash (which would
// add or cross segment boundaries), and a value containing net/http ServeMux
// pattern metacharacters ('{' or '}', which introduce a wildcard). A segment
// that passes maps to exactly the intended route.
func validateRouteSegment(s string) error {
	if s == "" {
		return errors.New("segment is empty")
	}
	if strings.ContainsAny(s, "/{}") {
		return errors.New("segment contains '/', '{', or '}'")
	}
	return nil
}

// isNilAdapter reports whether an Adapter is nil or a typed nil. A typed-nil
// interface value is non-nil under a plain == nil comparison, so a method call
// on it would panic; reflect distinguishes it.
func isNilAdapter(a Adapter) bool { return isNilInterface(a) }

// isNilInterface reports whether an interface value is nil or wraps a nil
// pointer, map, slice, channel, or function. It lets configuration validation
// treat a typed nil as unset rather than panicking on first use.
func isNilInterface(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func, reflect.Interface:
		return rv.IsNil()
	default:
		return false
	}
}
