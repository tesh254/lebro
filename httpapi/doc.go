// Package httpapi exposes registered lebro agents and workflows over HTTP and
// publishes the resulting contract as an OpenAPI 3.1 document.
//
// The package is optional: nothing in the root lebro module imports it, so an
// application that does not serve HTTP never compiles it in. It pulls in no
// dependencies beyond the lebro module and the standard library.
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
// ended rather than the connection dropping. The terminal event carries the
// run's total token usage, summed across every model call, so a multi-step tool
// conversation reports what it actually cost.
//
// Cancellation is the client's to trigger: closing the connection cancels the
// request context, which cancels the run, drains the provider stream, and
// releases the run goroutine.
//
// # Typed client
//
// Client calls a server of this shape with the same result and stream
// contracts, so an application that moves an agent out of process changes how
// it constructs the call rather than how it reads the answer:
//
//	client, err := httpapi.NewClient(httpapi.ClientConfig{
//	    BaseURL: "https://api.example.com",
//	    Header:  func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) },
//	})
//	result, err := client.Run(ctx, "assistant", httpapi.RunRequest{
//	    Messages: []httpapi.MessageInput{{Content: "hello"}},
//	})
//
// The client decodes the wire types this package defines, so the contract it
// speaks cannot drift from the one the server serves. Streamed runs return a
// ClientStream whose Events, Cancel, Wait, and Drain mirror lebro.StreamRun;
// Cancel closes the connection, which the server observes as a disconnect and
// turns into a cancelled run.
//
// Failures reach the caller as *APIError, carrying the server's ErrorCode and
// unwrapping to the lebro sentinel that classifies it: a remote tool failure
// matches errors.Is(err, lebro.ErrAgentToolFailure) exactly as a local one
// does. Codes with no runtime counterpart — a malformed request, a method
// mismatch — carry the code alone rather than claiming a sentinel that cannot
// occur locally.
//
// ContractVersion names the wire contract both sides speak.
// Client.CheckCompatibility compares it against the server's published version
// and reports ErrIncompatibleContract on a major mismatch. It is not called
// automatically: a run should not pay for a round trip it did not ask for.
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
