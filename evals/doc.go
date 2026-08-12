// Package evals runs a versioned dataset against an agent or workflow, scores
// each case with deterministic or model-graded scorers, and persists the
// per-case results so two runs can be compared.
//
// The package is optional: nothing in the root lebro module imports it, so an
// application that does not evaluate never compiles it in. It pulls in no
// dependencies beyond the lebro module and the standard library, and it defines
// no provider or storage coupling, so choosing a model adapter and a results
// backend stays an application decision.
//
// A dataset runs against anything satisfying Target. AgentTarget adapts any
// lebro.Workflow — which *lebro.Agent implements — and WorkflowTarget adapts a
// JSON-step workflow such as *lebro.LinearWorkflow:
//
//	experiment, err := evals.New(evals.ExperimentConfig{
//		Dataset: dataset,
//		Target:  evals.NewAgentTarget(agent),
//		Scorers: []evals.Scorer{evals.NewExactMatch(evals.ExactMatchConfig{})},
//	})
//	record, err := experiment.Run(ctx)
//
// # Dataset versions
//
// A Dataset's Version is a content hash over its ordered, normalized cases
// rather than a caller-supplied label, so "the same dataset version" is a
// verifiable fact. Editing a case, adding one, or reordering them changes the
// version; reformatting a case's JSON input does not, because inputs are
// canonicalized before hashing. Compare refuses to compare records whose
// dataset ID or version differ, which makes a misleading comparison a returned
// error instead of a plausible-looking delta.
//
// # Scorer failures are not target failures
//
// A scorer that errors or panics is recorded in CaseResult.ScorerFailures and
// leaves the target's own Status and Output untouched. The two outcomes answer
// different questions — "did the thing under test work?" and "could we measure
// it?" — and conflating them would let a broken judge read as a broken agent.
// ExperimentRecord counts them separately for the same reason. A panicking
// scorer is recovered so one bad scorer cannot abandon an entire run.
//
// # Determinism
//
// Cases are dispatched across a bounded worker pool, but results are always
// ordered by the case's position in the dataset, so a record does not depend on
// worker scheduling. Supply Clock and IDs to fix timestamps and identifiers
// when a test needs byte-identical records.
//
// # Storage
//
// Results persist through Repository, which is deliberately separate from
// lebro.Store. Experiment records and case results can therefore live in a
// different database from threads and workflow state, and an evaluation write
// can never join the transaction that persists a workflow step. The package
// ships MemoryRepository; a database-backed implementation is an application
// concern.
package evals
