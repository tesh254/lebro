// Package studio serves a local, Studio-style developer UI for exercising
// registered lebro agents, tools, workflows, threads, and run traces without
// writing one-off debugging programs.
//
// The package is optional and off by default: nothing in the root lebro module
// imports it, so an application that never serves the UI does not compile it in,
// and the UI is unreachable until a caller explicitly builds a Handler or calls
// Start. UI state is never a runtime requirement — the agents and workflows a
// program runs behave identically whether or not a Studio is attached.
//
// A Studio composes two existing lebro surfaces rather than reimplementing them.
// Agent runs, workflow runs, streaming, and thread reads are served by the
// httpapi package, mounted under /api. Ordered run events — the run, step,
// model, and tool spans that record what an agent did in what order, including
// tool calls and their results and the path a workflow took — are read from an
// observability Repository through the small TraceLister contract this package
// defines. The web UI itself is served as a static bundle embedded at build
// time; when no bundle is embedded a minimal placeholder page is served so the
// API is still usable.
//
// # Security considerations
//
// Studio is a local development tool. It exposes the same run and thread routes
// as httpapi, whose request bodies accept only user text and caller metadata,
// and it adds read-only trace routes over already-recorded spans. It ships no
// authentication: bind it to a loopback address, and wrap Config.Middleware to
// enforce a scheme before serving it anywhere a browser other than the
// developer's own can reach it. Trace bodies can contain model and tool output
// captured during a run, so the same caution that applies to a debugger's view
// of process memory applies here.
//
// # Quick start
//
//	studio := studio.New(studio.Config{
//	    Agents:     []*lebro.Agent{assistant},
//	    Workflows:  []*lebro.LinearWorkflow{pipeline},
//	    Store:      store,
//	    Traces:     repository, // an obsv.MemoryRepository, say
//	})
//	_ = http.ListenAndServe("127.0.0.1:4111", studio.Handler())
//
// Or let Start own the listener and honor a context for shutdown:
//
//	_ = studio.Start(ctx, "127.0.0.1:4111", studio.Config{ /* ... */ })
package studio
