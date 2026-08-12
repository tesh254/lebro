// Package httpapi exposes registered lebro agents and workflows over HTTP and
// publishes the resulting contract as an OpenAPI 3.1 document.
//
// The package is optional: nothing in the root lebro module imports it, so an
// application that does not serve HTTP never compiles it in. It depends only on
// the standard library.
//
// Only explicitly registered primitives are routable. NewServer returns a
// server with no agents and no workflows; every primitive must be registered
// with ExposeAgent or ExposeWorkflow, and thread reads require a Store on the
// configuration. A primitive that is not registered has no route, so it cannot
// be reached by guessing an identifier.
//
// # Security considerations
//
// Request bodies accept only user text and caller metadata. Message roles are
// fixed to user, so a client cannot inject a system prompt or forge an
// assistant turn: the agent's configured instructions remain the only system
// message. Thread identifiers come from the request path and never from a
// request body, so one caller cannot address another caller's durable
// conversation by naming it in a payload.
//
// Runtime failures are translated to stable public error codes rather than
// exposing internal error text. The wire body carries an ErrorCode and a fixed
// message for that code; the originating Go error stays server-side.
//
// Streamed deltas pass through a Redactor before they are serialized. A nil
// Redactor selects DefaultRedactor rather than disabling redaction, so a
// zero-valued configuration streams less rather than more.
//
// Authentication and authorization are deliberately absent. Wrap the handler
// with ServerConfig.Middleware to enforce them; the package stays neutral about
// the scheme, as the rest of lebro does for concerns it cannot own.
//
// Workflow resume is intentionally not exposed, matching the MCP server: a
// durable atomic resume claim does not exist yet, so a resume route would let
// two callers resume the same run.
//
// # Quick start
//
// Create a server, expose an agent, and serve it:
//
//	server := httpapi.NewServer(httpapi.ServerConfig{
//	    Title:   "my-app",
//	    Version: "1.0.0",
//	})
//	_ = server.ExposeAgent(agent)
//	_ = http.ListenAndServe(":8080", server.Handler())
//
// The handler serves POST /agents/{id}/runs for a complete run, POST
// /agents/{id}/runs/stream for a Server-Sent Events stream of the same run, and
// GET /openapi.json for the generated contract.
//
// # Streaming
//
// The stream route emits Server-Sent Events. Each event's name is the
// RunEventType vocabulary the runtime already uses ("model_delta",
// "run_succeeded", and so on), and its data is a JSON StreamEvent. The stream
// always terminates with exactly one terminal event, so a client knows the run
// ended rather than the connection dropping.
//
// Cancellation is the client's to trigger: closing the connection cancels the
// request context, which cancels the run, drains the provider stream, and
// releases the run goroutine.
//
// # Generated contract
//
// OpenAPI returns an OpenAPI 3.1 document generated from the same route table
// that builds the mux, so a route cannot exist without appearing in the
// document. Registered agents and workflows are enumerated in the description
// of their operations, and a workflow's declared input schema is embedded in
// its request body so the published contract reflects what the workflow
// actually accepts.
package httpapi
