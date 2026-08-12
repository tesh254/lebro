// evals-dataset demonstrates the evaluation contracts: a versioned dataset runs
// against a target, deterministic and model-graded scorers judge each case, the
// per-case results persist through a dedicated evaluation repository, and two
// runs of the same dataset version are compared.
//
// The target and the grader model here are deterministic local stand-ins so the
// example runs with no API key. Swap the target for lebro.NewAgentTarget over a
// real agent and the grader for any lebro.Model; nothing below depends on which
// target or provider is in use, because the evals package reduces both to an
// Output and a Score.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/evals"
)

func main() {
	ctx := context.Background()

	// 1. Define the dataset. Its version is a content hash over the ordered
	//    cases, so two runs can only claim the same version by running the same
	//    questions.
	dataset := evals.Dataset{
		ID:   "capitals",
		Name: "Capital city question answering",
		Cases: []evals.Case{
			{ID: "france", Input: json.RawMessage(`"What is the capital of France?"`), Expected: "Paris"},
			{ID: "japan", Input: json.RawMessage(`"What is the capital of Japan?"`), Expected: "Tokyo"},
			{ID: "peru", Input: json.RawMessage(`"What is the capital of Peru?"`), Expected: "Lima"},
		},
	}
	fmt.Printf("dataset %s version %s\n\n", dataset.ID, shortVersion(dataset.Version()))

	// 2. Assemble the scorers. Rule scorers are deterministic and need no
	//    provider; the model scorer goes through the same lebro.Model protocol an
	//    agent uses, so the caller supplies the adapter.
	exact := mustValue(evals.NewExactMatch(evals.ExactMatchConfig{TrimSpace: true}))
	noHedging := mustValue(evals.NewRegexp(evals.RegexpConfig{
		Name:    "no_hedging",
		Pattern: `(?i)as an ai|i cannot`,
		Negate:  true,
	}))
	graded := mustValue(evals.NewModelScorer(evals.ModelScorerConfig{
		Name:  "model_graded",
		Model: localGrader{},
	}))
	scorers := []evals.Scorer{exact, noHedging, graded}

	// 3. Run the baseline. Results persist through the evaluation repository,
	//    which is deliberately separate from lebro.Store.
	repository := evals.NewMemoryRepository()
	baseline, baselineCases := run(ctx, runConfig{
		id:         "exp-baseline",
		name:       "baseline",
		dataset:    dataset,
		scorers:    scorers,
		repository: repository,
		answers: map[evals.CaseID]string{
			"france": "Paris",
			"japan":  "As an AI, I cannot be certain, but likely Kyoto.",
			"peru":   "Lima",
		},
	})
	report("baseline", baseline, baselineCases)

	// 4. Run the candidate over the same dataset version.
	candidate, candidateCases := run(ctx, runConfig{
		id:         "exp-candidate",
		name:       "candidate",
		dataset:    dataset,
		scorers:    scorers,
		repository: repository,
		answers: map[evals.CaseID]string{
			"france": "Paris",
			"japan":  "Tokyo",
			// A regression the aggregate mean alone would hide, because japan
			// improved by the same amount.
			"peru": "Cusco",
		},
	})
	report("candidate", candidate, candidateCases)

	// 5. Compare. Compare refuses records from different dataset versions, so a
	//    delta always describes the same questions.
	comparison, err := evals.Compare(baseline, candidate, baselineCases, candidateCases)
	must(err)

	fmt.Println("comparison")
	for _, delta := range comparison.Scorers {
		fmt.Printf("  %-14s mean %+.2f  pass rate %+.2f\n", delta.Scorer, delta.MeanDelta, delta.PassRateDelta)
	}
	for _, regression := range comparison.Regressions {
		fmt.Printf("  REGRESSED %s/%s: %s\n", regression.CaseID, regression.Scorer, regression.Reason)
	}
	for _, improvement := range comparison.Improvements {
		fmt.Printf("  IMPROVED  %s/%s\n", improvement.CaseID, improvement.Scorer)
	}

	// A mean that did not move is not proof that nothing changed: two offsetting
	// per-case changes cancel out in the aggregate.
	fmt.Printf("\nregressed: %t\n", comparison.Regressed())

	// 6. Stored records are readable back for a later comparison against a future
	//    run of the same dataset version.
	stored, err := repository.ListExperiments(ctx, dataset.ID, dataset.Version())
	must(err)
	fmt.Printf("stored experiments for this dataset version: %d\n", len(stored))
}

type runConfig struct {
	id         evals.ExperimentID
	name       string
	dataset    evals.Dataset
	scorers    []evals.Scorer
	repository evals.Repository
	answers    map[evals.CaseID]string
}

func run(ctx context.Context, config runConfig) (evals.ExperimentRecord, []evals.CaseResult) {
	experiment := mustValue(evals.New(evals.ExperimentConfig{
		Name:       config.name,
		Dataset:    config.dataset,
		Target:     lookupTarget{answers: config.answers},
		Scorers:    config.scorers,
		Repository: config.repository,
		IDs:        evals.NewFixedExperimentIDSource(config.id),
	}))
	record, results, err := experiment.Run(ctx)
	must(err)
	return record, results
}

func report(label string, record evals.ExperimentRecord, results []evals.CaseResult) {
	fmt.Printf("%s (%s)\n", label, record.ID)
	for _, aggregate := range record.Scorers {
		fmt.Printf("  %-14s mean %.2f  pass %d/%d  scorer failures %d\n",
			aggregate.Scorer, aggregate.Mean, aggregate.Passed, aggregate.Scored, aggregate.Failures)
	}
	// A target failure and a scorer failure are reported separately, because
	// "the agent broke" and "we could not measure it" are different findings.
	fmt.Printf("  target failures %d, scorer failures %d\n", record.TargetFailures, record.ScorerFailures)
	for _, result := range results {
		for _, score := range result.Scores {
			if !score.Passed {
				fmt.Printf("  FAIL %s/%s: %s\n", result.CaseID, score.Scorer, score.Reason)
			}
		}
		for _, failure := range result.ScorerFailures {
			fmt.Printf("  UNMEASURED %s/%s: %s\n", result.CaseID, failure.Scorer, failure.Message)
		}
	}
	fmt.Println()
}

// lookupTarget answers each case from a table. A real evaluation would wrap an
// agent with evals.NewAgentTarget or a JSON-step workflow with
// evals.NewWorkflowTarget; both reduce to the same Output the scorers read.
type lookupTarget struct {
	answers map[evals.CaseID]string
}

func (t lookupTarget) Name() string { return "lookup" }

func (t lookupTarget) Invoke(_ context.Context, testCase evals.Case) (evals.Output, error) {
	answer, found := t.answers[testCase.ID]
	if !found {
		return evals.Output{Status: lebro.RunStatusFailed}, fmt.Errorf("no answer for case %q", testCase.ID)
	}
	return evals.Output{
		Text:   answer,
		Status: lebro.RunStatusSucceeded,
		RunID:  lebro.RunID("run-" + string(testCase.ID)),
	}, nil
}

// localGrader is a deterministic stand-in for a grader model. It implements the
// same lebro.Model interface a real provider adapter does, so swapping in
// openai.New changes nothing about how the scorer is configured or read.
type localGrader struct{}

func (localGrader) Generate(_ context.Context, request lebro.ModelRequest) (lebro.ModelResponse, error) {
	// The grading prompt carries the expectation and the output; score by whether
	// the expected value appears in the graded answer.
	prompt := request.Messages[len(request.Messages)-1].Content
	expectation, output := splitPrompt(prompt)

	verdict := map[string]any{"score": 0.1, "reason": "output does not state the expected answer"}
	if expectation != "" && strings.Contains(strings.ToLower(output), strings.ToLower(expectation)) {
		verdict = map[string]any{"score": 0.95, "reason": "output states the expected answer"}
	}
	encoded, err := json.Marshal(verdict)
	if err != nil {
		return lebro.ModelResponse{}, err
	}
	return lebro.ModelResponse{
		Message: lebro.Message{
			Role:             lebro.RoleAssistant,
			StructuredOutput: lebro.NewModelStructuredOutput(encoded),
		},
		FinishReason: lebro.FinishReasonStop,
	}, nil
}

// splitPrompt reads the expectation and output back out of the grading prompt the
// ModelScorer rendered.
func splitPrompt(prompt string) (expectation, output string) {
	sections := strings.Split(prompt, "Output:\n")
	if len(sections) == 2 {
		output = strings.TrimSpace(sections[1])
	}
	if _, rest, found := strings.Cut(sections[0], "Expectation:\n"); found {
		expectation = strings.TrimSpace(rest)
	}
	return expectation, output
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
