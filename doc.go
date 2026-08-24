// Package lebro provides stable public contracts for building composable AI
// agents and workflows in Go.
//
// The root package is the dependency-light application API. It contains model,
// tool, agent, workflow, persistence, scheduling, authorization, and RAG
// contracts plus their constructors. Optional integrations live in their own
// packages: [github.com/tesh254/lebro/httpapi],
// [github.com/tesh254/lebro/mcp], [github.com/tesh254/lebro/channels],
// [github.com/tesh254/lebro/voice], [github.com/tesh254/lebro/obsv],
// [github.com/tesh254/lebro/evals], and provider adapters. None is imported by
// this package.
//
// Constructors validate configuration up front. Values crossing a model, tool,
// workflow, or storage boundary are schema-checked where a schema is declared;
// callers retain context cancellation and choose policy, provider, and storage
// implementations. See docs/stability.md for compatibility commitments and
// docs/migrations.md before changing persisted or wire-visible deployments.
// Runtime implementation is organized under internal/runtime.
package lebro
