package studio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/httpapi"
	"github.com/tesh254/lebro/obsv"
)

// Config configures a Studio developer UI over a set of lebro primitives. The
// zero value serves the UI shell with no agents, no workflows, and empty trace
// and thread views; construct a usable Studio with New.
type Config struct {
	// Title labels the UI and the underlying OpenAPI document. When empty,
	// "lebro studio" is used.
	Title string
	// Agents are exposed for running and inspecting. Each must have a non-empty
	// definition ID; a duplicate ID is a construction error surfaced by New.
	Agents []*lebro.Agent
	// Workflows are exposed for running with custom JSON input. Each must have a
	// non-empty definition ID.
	Workflows []*lebro.LinearWorkflow
	// Store backs the thread views and lets a run bind to a durable thread. When
	// nil, thread routes report not-found, matching httpapi.
	Store lebro.Store
	// Traces reads recorded observability spans for the trace views. When nil,
	// the trace views are present but empty.
	Traces TraceLister
	// Middleware wraps the whole handler, outermost first. Studio ships no
	// authentication; use this to add one before serving beyond loopback.
	Middleware []func(http.Handler) http.Handler
}

// Studio serves the developer UI and its backing API. It is safe to build its
// Handler concurrently and repeatedly. Construct one with New; the zero value
// is not usable.
type Studio struct {
	config Config
	api    *httpapi.Server

	handlerOnce sync.Once
	handler     http.Handler
}

// New builds a Studio from a Config. It exposes each agent and workflow on an
// embedded httpapi server and returns the first registration error, so a
// duplicate or unidentified primitive fails here rather than at request time.
func New(config Config) (*Studio, error) {
	if config.Title == "" {
		config.Title = "lebro studio"
	}

	api := httpapi.NewServer(httpapi.ServerConfig{
		Title:   config.Title,
		Version: "0.0.0",
		Store:   config.Store,
	})
	for _, agent := range config.Agents {
		if err := api.ExposeAgent(agent); err != nil {
			return nil, fmt.Errorf("lebro/studio: expose agent: %w", err)
		}
	}
	for _, workflow := range config.Workflows {
		if err := api.ExposeWorkflow(workflow); err != nil {
			return nil, fmt.Errorf("lebro/studio: expose workflow: %w", err)
		}
	}

	return &Studio{config: config, api: api}, nil
}

// Handler returns the http.Handler that serves the UI and its API. The API is
// mounted under /api: httpapi's routes at /api (so /api/agents, /api/workflows,
// /api/threads/{id}) and Studio's read-only trace routes at /api/studio. The UI
// bundle is served at the root. The router is built once and cached, so Handler
// is cheap to call repeatedly.
func (s *Studio) Handler() http.Handler {
	s.handlerOnce.Do(func() {
		s.handler = s.buildHandler()
	})
	return s.handler
}

// ServeHTTP lets a Studio be used directly as an http.Handler.
func (s *Studio) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Handler().ServeHTTP(w, r)
}

func (s *Studio) buildHandler() http.Handler {
	mux := http.NewServeMux()

	// httpapi owns runs, streaming, and thread reads. Strip the /api prefix so
	// its routes, which are declared without one, match. StripPrefix leaves the
	// path empty for a bare "/api" request; httpapi answers that with its typed
	// 404, which is the shape Studio wants anyway.
	mux.Handle("/api/", http.StripPrefix("/api", s.api.Handler()))

	// Studio's own trace routes live under /api/studio so they never collide
	// with an httpapi route, present or future.
	mux.HandleFunc("GET /api/studio/traces", s.handleListTraces)
	mux.HandleFunc("GET /api/studio/traces/{id}", s.handleGetTrace)

	// The UI bundle is served at the root as a catch-all, so client-side
	// routing works: an unknown UI path falls back to the app shell rather than
	// 404ing.
	mux.Handle("/", s.assetHandler())

	var handler http.Handler = mux
	for i := len(s.config.Middleware) - 1; i >= 0; i-- {
		if s.config.Middleware[i] == nil {
			continue
		}
		handler = s.config.Middleware[i](handler)
	}
	return handler
}

// tracesOrEmpty returns the configured TraceLister or an empty stand-in, so the
// trace handlers never nil-check.
func (s *Studio) tracesOrEmpty() TraceLister {
	if s.config.Traces == nil {
		return emptyTraceLister{}
	}
	return s.config.Traces
}

// writeJSON serializes value as the response body. It mirrors httpapi's helper
// so Studio's own routes answer in the same shape.
func writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		writeStudioError(w, http.StatusInternalServerError, "internal")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeStudioError writes a minimal typed error body for Studio's own routes.
// The httpapi routes keep their own richer error vocabulary; Studio's read-only
// routes have only a couple of failure modes and do not need it.
func writeStudioError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":{"code":%q}}`, code)
}

// emptyTraceLister stands in for an unconfigured Traces, serving empty views.
type emptyTraceLister struct{}

func (emptyTraceLister) Spans() []obsv.Span { return nil }
func (emptyTraceLister) SpansByTrace(context.Context, obsv.TraceID) ([]obsv.Span, error) {
	return nil, nil
}
