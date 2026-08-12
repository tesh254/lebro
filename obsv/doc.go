// Package obsv converts lebro run lifecycle events into a trace/span model and
// exports traces, logs, usage metrics, and feedback through pluggable
// exporters.
//
// The package is optional: nothing in the root lebro module imports it, so an
// application that does not export observability data never compiles it in. It
// pulls in no dependencies beyond the lebro module and the standard library,
// and it defines no wire format, so choosing a backend stays an application
// decision.
//
// An Observer implements lebro.RunListener, so it attaches wherever a listener
// already does:
//
//	observer, err := obsv.New(obsv.Config{Spans: exporter})
//	agent, err := lebro.NewAgent(lebro.AgentConfig{Listener: observer, /* ... */})
//
// # Correlation
//
// Spans are derived from the ordered events a run already emits rather than
// from separate instrumentation, so a trace cannot disagree with the run it
// describes. Paired start/finish events become spans: a run span roots the
// trace, with step, model, and tool spans beneath it. Nested runs started by a
// workflow step carry the parent run and step on every event, so a workflow,
// the agents it invokes, and their model and tool calls share one trace.
//
// Streaming deltas and tool-call requests become span events rather than spans:
// a long stream would otherwise produce one span per token. DefaultDeltaLimit
// bounds how many are retained per span.
//
// # Run isolation
//
// Export never changes a run's result. Exporters run on the Observer's own
// goroutine, not the caller's; a returned error, a panic, or a slow exporter
// cannot propagate into the run or block it. Errors reach the configured
// ErrorHandler, panics are recovered and reported as errors, and a full queue
// drops spans rather than applying backpressure to the run. Drops are counted
// and readable through Stats, so silent loss is observable.
//
// # Sensitive data
//
// Filters run inside the Observer before any exporter is handed a span, so an
// added exporter cannot bypass them. A nil Filter selects DefaultFilter, which
// drops model and tool payloads while keeping identifiers, timings, usage, and
// status: a zero-valued Config therefore exports less rather than more. Pass
// PassthroughFilter to opt out deliberately.
//
// Attribute keys carrying payload data are named with the SensitiveAttr prefix
// so a custom filter can find them without enumerating every producer.
//
// # Storage
//
// Observability data persists through Repository, which is deliberately
// separate from lebro.Store. Spans, logs, and feedback can therefore live in a
// different database from threads and workflow state, and an observability
// write can never join the transaction that persists a workflow step.
package obsv
