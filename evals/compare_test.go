package evals_test

import (
	"errors"
	"testing"

	"github.com/tesh254/lebro/evals"
)

// Distinct means and pass rates per side, so a delta computed with the operands
// swapped produces the wrong sign rather than the same number.
func comparableRecords() (baseline, candidate evals.ExperimentRecord) {
	baseline = evals.ExperimentRecord{
		ID:             "exp-base",
		DatasetID:      "capitals",
		DatasetVersion: "v1",
		TargetFailures: 1,
		ScorerFailures: 0,
		Scorers: []evals.ScorerAggregate{
			{Scorer: "exact_match", Scored: 4, Passed: 2, Mean: 0.5, PassRate: 0.5},
		},
	}
	candidate = evals.ExperimentRecord{
		ID:             "exp-candidate",
		DatasetID:      "capitals",
		DatasetVersion: "v1",
		TargetFailures: 3,
		ScorerFailures: 2,
		Scorers: []evals.ScorerAggregate{
			{Scorer: "exact_match", Scored: 4, Passed: 3, Mean: 0.75, PassRate: 0.75},
		},
	}
	return baseline, candidate
}

func TestCompareReportsAggregateDeltas(t *testing.T) {
	baseline, candidate := comparableRecords()
	comparison, err := evals.Compare(baseline, candidate, nil, nil)
	if err != nil {
		t.Fatalf("Compare() = %v", err)
	}
	if comparison.BaselineID != "exp-base" || comparison.CandidateID != "exp-candidate" {
		t.Fatalf("identity = (%q, %q)", comparison.BaselineID, comparison.CandidateID)
	}
	if len(comparison.Scorers) != 1 {
		t.Fatalf("got %d scorer deltas, want 1", len(comparison.Scorers))
	}
	delta := comparison.Scorers[0]
	if delta.MeanDelta != 0.25 {
		t.Fatalf("MeanDelta = %v, want 0.25 (candidate minus baseline)", delta.MeanDelta)
	}
	if delta.PassRateDelta != 0.25 {
		t.Fatalf("PassRateDelta = %v, want 0.25", delta.PassRateDelta)
	}
	if !delta.Present {
		t.Fatal("Present = false for a scorer in both records")
	}
	if comparison.TargetFailureDelta != 2 {
		t.Fatalf("TargetFailureDelta = %d, want 2", comparison.TargetFailureDelta)
	}
	if comparison.ScorerFailureDelta != 2 {
		t.Fatalf("ScorerFailureDelta = %d, want 2", comparison.ScorerFailureDelta)
	}
}

// TestCompareRejectsDifferentDatasetVersions is the guard that makes the second
// acceptance criterion meaningful: two runs must describe the same dataset
// version, or the delta compares different questions.
func TestCompareRejectsDifferentDatasetVersions(t *testing.T) {
	baseline, candidate := comparableRecords()
	candidate.DatasetVersion = "v2"
	if _, err := evals.Compare(baseline, candidate, nil, nil); !errors.Is(err, evals.ErrDatasetVersionMismatch) {
		t.Fatalf("Compare() = %v, want ErrDatasetVersionMismatch", err)
	}
}

func TestCompareRejectsDifferentDatasets(t *testing.T) {
	baseline, candidate := comparableRecords()
	candidate.DatasetID = "other"
	if _, err := evals.Compare(baseline, candidate, nil, nil); !errors.Is(err, evals.ErrDatasetVersionMismatch) {
		t.Fatalf("Compare() = %v, want ErrDatasetVersionMismatch", err)
	}
}

// TestCompareMarksAddedAndRemovedScorers pins that a scorer present on one side
// only reports Present false rather than a zero delta that reads as "no change".
func TestCompareMarksAddedAndRemovedScorers(t *testing.T) {
	baseline, candidate := comparableRecords()
	baseline.Scorers = append(baseline.Scorers, evals.ScorerAggregate{
		Scorer: "removed", Scored: 4, Passed: 4, Mean: 1, PassRate: 1,
	})
	candidate.Scorers = append(candidate.Scorers, evals.ScorerAggregate{
		Scorer: "added", Scored: 4, Passed: 1, Mean: 0.25, PassRate: 0.25,
	})

	comparison, err := evals.Compare(baseline, candidate, nil, nil)
	if err != nil {
		t.Fatalf("Compare() = %v", err)
	}
	byName := make(map[string]evals.ScorerDelta, len(comparison.Scorers))
	for _, delta := range comparison.Scorers {
		byName[delta.Scorer] = delta
	}
	if len(byName) != 3 {
		t.Fatalf("got %d deltas, want 3", len(byName))
	}
	if byName["removed"].Present {
		t.Fatal("a baseline-only scorer reports Present true")
	}
	if byName["added"].Present {
		t.Fatal("a candidate-only scorer reports Present true")
	}
	// A scorer measured on only one side must report a zero delta. Subtracting
	// the missing side's zero-valued aggregate would otherwise report the whole
	// of the present side's mean or pass rate as a quality change, when nothing
	// was actually compared.
	if got := byName["removed"]; got.MeanDelta != 0 || got.PassRateDelta != 0 {
		t.Fatalf("baseline-only scorer deltas = (%v, %v), want (0, 0)", got.MeanDelta, got.PassRateDelta)
	}
	if got := byName["added"]; got.MeanDelta != 0 || got.PassRateDelta != 0 {
		t.Fatalf("candidate-only scorer deltas = (%v, %v), want (0, 0)", got.MeanDelta, got.PassRateDelta)
	}
	if !byName["exact_match"].Present {
		t.Fatal("a shared scorer reports Present false")
	}
	// Order follows the baseline's configuration, with candidate-only scorers last.
	wantOrder := []string{"exact_match", "removed", "added"}
	for i, delta := range comparison.Scorers {
		if delta.Scorer != wantOrder[i] {
			t.Fatalf("delta %d is %q, want %q", i, delta.Scorer, wantOrder[i])
		}
	}
}

func TestCompareIdentifiesPerCaseChanges(t *testing.T) {
	baseline, candidate := comparableRecords()
	baselineCases := []evals.CaseResult{
		{CaseID: "france", Index: 0, Scores: []evals.Score{{Scorer: "exact_match", Value: 1, Passed: true}}},
		{CaseID: "japan", Index: 1, Scores: []evals.Score{{Scorer: "exact_match", Value: 0, Passed: false}}},
		{CaseID: "peru", Index: 2, Scores: []evals.Score{{Scorer: "exact_match", Value: 1, Passed: true}}},
	}
	candidateCases := []evals.CaseResult{
		// france regressed, japan improved, peru unchanged.
		{CaseID: "france", Index: 0, Scores: []evals.Score{{Scorer: "exact_match", Value: 0, Passed: false, Reason: "expected \"Paris\", got \"Lyon\""}}},
		{CaseID: "japan", Index: 1, Scores: []evals.Score{{Scorer: "exact_match", Value: 1, Passed: true}}},
		{CaseID: "peru", Index: 2, Scores: []evals.Score{{Scorer: "exact_match", Value: 1, Passed: true}}},
	}

	comparison, err := evals.Compare(baseline, candidate, baselineCases, candidateCases)
	if err != nil {
		t.Fatalf("Compare() = %v", err)
	}
	if !comparison.Regressed() {
		t.Fatal("Regressed() = false despite a regression")
	}
	if len(comparison.Regressions) != 1 || comparison.Regressions[0].CaseID != "france" {
		t.Fatalf("Regressions = %+v, want one for france", comparison.Regressions)
	}
	if comparison.Regressions[0].Delta != -1 {
		t.Fatalf("regression Delta = %v, want -1", comparison.Regressions[0].Delta)
	}
	if comparison.Regressions[0].Reason == "" {
		t.Fatal("regression carries no reason from the candidate's score")
	}
	if len(comparison.Improvements) != 1 || comparison.Improvements[0].CaseID != "japan" {
		t.Fatalf("Improvements = %+v, want one for japan", comparison.Improvements)
	}
	if comparison.Improvements[0].Delta != 1 {
		t.Fatalf("improvement Delta = %v, want 1", comparison.Improvements[0].Delta)
	}
}

// TestCompareSkipsUnpairedCases pins that a case or scorer measured on only one
// side is not reported as a change: there is no pair, and inventing one would
// report a regression where no measurement existed.
func TestCompareSkipsUnpairedCases(t *testing.T) {
	baseline, candidate := comparableRecords()
	baselineCases := []evals.CaseResult{
		{CaseID: "france", Scores: []evals.Score{{Scorer: "exact_match", Value: 1, Passed: true}}},
	}
	candidateCases := []evals.CaseResult{
		// A different case, and a different scorer on the shared case.
		{CaseID: "france", Scores: []evals.Score{{Scorer: "other_scorer", Value: 0, Passed: false}}},
		{CaseID: "brazil", Scores: []evals.Score{{Scorer: "exact_match", Value: 0, Passed: false}}},
	}

	comparison, err := evals.Compare(baseline, candidate, baselineCases, candidateCases)
	if err != nil {
		t.Fatalf("Compare() = %v", err)
	}
	if len(comparison.Regressions) != 0 || len(comparison.Improvements) != 0 {
		t.Fatalf("unpaired measurements produced changes: %+v / %+v",
			comparison.Regressions, comparison.Improvements)
	}
}

// TestCompareOrdersChangesStably pins that a comparison does not depend on the
// order results were stored in, so two runs of the same comparison read the same.
func TestCompareOrdersChangesStably(t *testing.T) {
	baseline, candidate := comparableRecords()
	baselineCases := []evals.CaseResult{
		{CaseID: "zeta", Scores: []evals.Score{{Scorer: "exact_match", Value: 1, Passed: true}}},
		{CaseID: "alpha", Scores: []evals.Score{{Scorer: "exact_match", Value: 1, Passed: true}}},
		{CaseID: "mid", Scores: []evals.Score{{Scorer: "exact_match", Value: 1, Passed: true}}},
	}
	candidateCases := []evals.CaseResult{
		{CaseID: "zeta", Scores: []evals.Score{{Scorer: "exact_match", Value: 0, Passed: false}}},
		{CaseID: "alpha", Scores: []evals.Score{{Scorer: "exact_match", Value: 0, Passed: false}}},
		{CaseID: "mid", Scores: []evals.Score{{Scorer: "exact_match", Value: 0, Passed: false}}},
	}

	comparison, err := evals.Compare(baseline, candidate, baselineCases, candidateCases)
	if err != nil {
		t.Fatalf("Compare() = %v", err)
	}
	want := []evals.CaseID{"alpha", "mid", "zeta"}
	if len(comparison.Regressions) != len(want) {
		t.Fatalf("got %d regressions, want %d", len(comparison.Regressions), len(want))
	}
	for i, regression := range comparison.Regressions {
		if regression.CaseID != want[i] {
			t.Fatalf("regression %d is %q, want %q", i, regression.CaseID, want[i])
		}
	}
}

// TestCompareEndToEndAcrossExperimentRuns exercises the second acceptance
// criterion against records the experiment runner actually produced, rather than
// hand-built ones.
func TestCompareEndToEndAcrossExperimentRuns(t *testing.T) {
	scorer, err := evals.NewExactMatch(evals.ExactMatchConfig{})
	if err != nil {
		t.Fatalf("NewExactMatch() = %v", err)
	}
	repository := evals.NewMemoryRepository()

	run := func(id evals.ExperimentID, answers map[evals.CaseID]string) (evals.ExperimentRecord, []evals.CaseResult) {
		experiment, err := evals.New(evals.ExperimentConfig{
			Dataset:    answerDataset(),
			Target:     answerTarget{answers: answers},
			Scorers:    []evals.Scorer{scorer},
			Repository: repository,
			IDs:        evals.NewFixedExperimentIDSource(id),
		})
		if err != nil {
			t.Fatalf("New() = %v", err)
		}
		record, results, err := experiment.Run(t.Context())
		if err != nil {
			t.Fatalf("Run() = %v", err)
		}
		return record, results
	}

	// The baseline gets japan wrong; the candidate fixes japan and breaks peru.
	baseline, baselineCases := run("exp-base", map[evals.CaseID]string{
		"france": "Paris", "japan": "Kyoto", "peru": "Lima",
	})
	candidate, candidateCases := run("exp-candidate", map[evals.CaseID]string{
		"france": "Paris", "japan": "Tokyo", "peru": "Cusco",
	})

	if baseline.DatasetVersion != candidate.DatasetVersion {
		t.Fatal("two runs of one dataset produced different versions")
	}
	comparison, err := evals.Compare(baseline, candidate, baselineCases, candidateCases)
	if err != nil {
		t.Fatalf("Compare() = %v", err)
	}
	// Both runs score 2/3, so the aggregate mean is unchanged — which is exactly
	// why per-case changes are reported: the offsetting swap is invisible in the
	// mean alone.
	if comparison.Scorers[0].MeanDelta != 0 {
		t.Fatalf("MeanDelta = %v, want 0", comparison.Scorers[0].MeanDelta)
	}
	if len(comparison.Regressions) != 1 || comparison.Regressions[0].CaseID != "peru" {
		t.Fatalf("Regressions = %+v, want one for peru", comparison.Regressions)
	}
	if len(comparison.Improvements) != 1 || comparison.Improvements[0].CaseID != "japan" {
		t.Fatalf("Improvements = %+v, want one for japan", comparison.Improvements)
	}
}
