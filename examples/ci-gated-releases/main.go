// ci-gated-releases is the Braintrust-style release gate: a prompt/model change
// runs against a versioned evaluation dataset whose version is a content hash
// over the ordered cases, and Compare names the exact cases whose pass state
// moved before the deploy is allowed.
//
// The targets are real agents driven by fixture models, so the whole gate runs
// deterministically with no API key. Swap the fixture models for provider
// adapters and the same code gates a real deployment.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/evals"
	"github.com/tesh254/lebro/internal/testkit"
)

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(output io.Writer) error {
	ctx := context.Background()

	// 1. The dataset is versioned by content hash over its ordered cases, so
	// two experiments can only share a version by asking identical questions.
	dataset := evals.Dataset{
		ID:   "support-bot-regression",
		Name: "Support bot must-know answers",
		Cases: []evals.Case{
			{ID: "refund-window", Input: json.RawMessage(`"How long do I have to request a refund?"`), Expected: "You can request a refund within 30 days."},
			{ID: "express-shipping", Input: json.RawMessage(`"When does express delivery arrive?"`), Expected: "Express orders arrive the next business day."},
			{ID: "password-reset", Input: json.RawMessage(`"I forgot my password."`), Expected: "We will email you a reset link shortly."},
		},
	}
	writef(output, "dataset %s version %s\n\n", dataset.ID, shortVersion(dataset.Version()))

	scorers := []evals.Scorer{
		mustValue(evals.NewExactMatch(evals.ExactMatchConfig{TrimSpace: true})),
	}
	repository := evals.NewMemoryRepository()

	evaluate := func(name string, replies []string) (evals.ExperimentRecord, []evals.CaseResult, error) {
		agent := mustValue(lebro.NewAgent(lebro.AgentConfig{
			Definition: lebro.AgentDefinition{ID: lebro.AgentID("support-" + name), Name: "Support " + name},
			Model:      testkit.NewModel(textFixtures(replies)...),
		}))
		experiment := mustValue(evals.New(evals.ExperimentConfig{
			Name:       name,
			Dataset:    dataset,
			Target:     evals.NewAgentTarget(agent),
			Scorers:    scorers,
			Repository: repository,
			// The fixture model answers cases in dataset order, so evaluate
			// one case at a time; production targets are order-independent
			// and can leave this at the parallel default.
			Concurrency: -1,
		}))
		record, results, err := experiment.Run(ctx)
		return record, results, err
	}

	// 2. Baseline: the prompts currently in production.
	baselineRecord, baselineCases, err := evaluate("baseline", []string{
		"You can request a refund within 30 days.",
		"Express orders arrive the next business day.",
		"We will email you a reset link shortly.",
	})
	if err != nil {
		return err
	}
	report(output, "baseline", baselineRecord)

	// 3. Candidate: a prompt rewrite that silently broke one case while leaving
	// the aggregate close enough to miss by eye.
	candidateRecord, candidateCases, err := evaluate("candidate", []string{
		"You can request a refund within 30 days.",
		"Express orders arrive the next business day.",
		"Please contact support so an agent can restore your access.",
	})
	if err != nil {
		return err
	}
	report(output, "candidate", candidateRecord)

	// 4. The gate: Compare refuses records from different dataset versions and
	// names every case whose pass state moved.
	comparison, err := evals.Compare(baselineRecord, candidateRecord, baselineCases, candidateCases)
	if err != nil {
		return err
	}
	for _, regression := range comparison.Regressions {
		writef(output, "REGRESSED %s/%s: %s\n", regression.CaseID, regression.Scorer, regression.Reason)
	}
	for _, improvement := range comparison.Improvements {
		writef(output, "IMPROVED %s/%s\n", improvement.CaseID, improvement.Scorer)
	}
	if comparison.Regressed() {
		writef(output, "\nDEPLOY BLOCKED: regressions against dataset %s\n", dataset.ID)
		return nil
	}
	writef(output, "\ndeploy approved.\n")
	return nil
}

// textFixtures builds one canned reply per dataset case; cases run in order.
func textFixtures(replies []string) []testkit.Fixture {
	fixtures := make([]testkit.Fixture, len(replies))
	for i, reply := range replies {
		fixtures[i] = testkit.Text(reply)
	}
	return fixtures
}

func report(output io.Writer, label string, record evals.ExperimentRecord) {
	passed, scored := 0, 0
	for _, aggregate := range record.Scorers {
		passed += aggregate.Passed
		scored += aggregate.Scored
	}
	writef(output, "%s (%s): %d/%d passed\n", label, record.ID, passed, scored)
}

func writef(output io.Writer, format string, args ...any) {
	if _, err := fmt.Fprintf(output, format, args...); err != nil {
		panic(err)
	}
}

func shortVersion(version evals.DatasetVersion) string {
	if len(version) > 12 {
		return string(version[:12])
	}
	return string(version)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func mustValue[T any](value T, err error) T {
	must(err)
	return value
}
