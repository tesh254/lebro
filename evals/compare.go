package evals

import (
	"fmt"
	"sort"
)

// ScorerDelta is one scorer's change between two experiment runs.
//
// MeanDelta and PassRateDelta are candidate minus baseline, so a positive value
// is an improvement for a scorer where higher is better. Present reports whether
// both records carried this scorer: a scorer added or removed between runs yields
// a delta of zero that must not be read as "no change".
type ScorerDelta struct {
	Scorer            string  `json:"scorer"`
	BaselineMean      float64 `json:"baseline_mean"`
	CandidateMean     float64 `json:"candidate_mean"`
	MeanDelta         float64 `json:"mean_delta"`
	BaselinePassRate  float64 `json:"baseline_pass_rate"`
	CandidatePassRate float64 `json:"candidate_pass_rate"`
	PassRateDelta     float64 `json:"pass_rate_delta"`
	BaselineScored    int     `json:"baseline_scored"`
	CandidateScored   int     `json:"candidate_scored"`
	Present           bool    `json:"present"`
}

// CaseDelta is one case's change under one scorer.
type CaseDelta struct {
	CaseID          CaseID  `json:"case_id"`
	Scorer          string  `json:"scorer"`
	BaselineValue   float64 `json:"baseline_value"`
	CandidateValue  float64 `json:"candidate_value"`
	Delta           float64 `json:"delta"`
	BaselinePassed  bool    `json:"baseline_passed"`
	CandidatePassed bool    `json:"candidate_passed"`
	// Reason is the candidate's reason for its judgment, which is what explains a
	// regression.
	Reason string `json:"reason,omitempty"`
}

// Comparison is the difference between two experiment runs of the same dataset
// version.
//
// Regressions and Improvements list per-case pass-state changes, which is the
// actionable part of a comparison: a mean that moved says something changed,
// while a named case that started failing says what. TargetFailureDelta and
// ScorerFailureDelta stay separate so a run that got worse because the target
// broke is distinguishable from one that only lost its ability to measure.
type Comparison struct {
	DatasetID          DatasetID      `json:"dataset_id"`
	DatasetVersion     DatasetVersion `json:"dataset_version"`
	BaselineID         ExperimentID   `json:"baseline_id"`
	CandidateID        ExperimentID   `json:"candidate_id"`
	Scorers            []ScorerDelta  `json:"scorers,omitempty"`
	TargetFailureDelta int            `json:"target_failure_delta"`
	ScorerFailureDelta int            `json:"scorer_failure_delta"`
	Regressions        []CaseDelta    `json:"regressions,omitempty"`
	Improvements       []CaseDelta    `json:"improvements,omitempty"`
}

// Regressed reports whether any case's pass state got worse.
func (c Comparison) Regressed() bool { return len(c.Regressions) > 0 }

// Compare reports the difference between two experiment records.
//
// It returns ErrDatasetVersionMismatch when the records do not describe the same
// dataset at the same version. Comparing two different question sets would
// produce a delta that looks like a quality change but is really a change of
// subject, so the mismatch is an error rather than a caller's responsibility to
// notice.
//
// Aggregate deltas come from the records alone. Per-case regressions require the
// case results, which are passed separately because a record does not embed
// them; pass nil for either to get aggregate deltas only.
func Compare(baseline, candidate ExperimentRecord, baselineCases, candidateCases []CaseResult) (Comparison, error) {
	if baseline.DatasetID != candidate.DatasetID {
		return Comparison{}, fmt.Errorf("%w: baseline dataset %q, candidate dataset %q",
			ErrDatasetVersionMismatch, baseline.DatasetID, candidate.DatasetID)
	}
	if baseline.DatasetVersion != candidate.DatasetVersion {
		return Comparison{}, fmt.Errorf("%w: dataset %q baseline version %q, candidate version %q",
			ErrDatasetVersionMismatch, baseline.DatasetID, baseline.DatasetVersion, candidate.DatasetVersion)
	}

	comparison := Comparison{
		DatasetID:          baseline.DatasetID,
		DatasetVersion:     baseline.DatasetVersion,
		BaselineID:         baseline.ID,
		CandidateID:        candidate.ID,
		TargetFailureDelta: candidate.TargetFailures - baseline.TargetFailures,
		ScorerFailureDelta: candidate.ScorerFailures - baseline.ScorerFailures,
		Scorers:            compareAggregates(baseline, candidate),
	}
	comparison.Regressions, comparison.Improvements = compareCases(baselineCases, candidateCases)
	return comparison, nil
}

// compareAggregates builds one delta per scorer present in either record,
// ordered by the baseline's scorer order with candidate-only scorers appended, so
// a comparison reads in the order the baseline was configured.
func compareAggregates(baseline, candidate ExperimentRecord) []ScorerDelta {
	names := make([]string, 0, len(baseline.Scorers)+len(candidate.Scorers))
	seen := make(map[string]struct{}, len(names))
	for _, aggregate := range baseline.Scorers {
		if _, exists := seen[aggregate.Scorer]; !exists {
			seen[aggregate.Scorer] = struct{}{}
			names = append(names, aggregate.Scorer)
		}
	}
	for _, aggregate := range candidate.Scorers {
		if _, exists := seen[aggregate.Scorer]; !exists {
			seen[aggregate.Scorer] = struct{}{}
			names = append(names, aggregate.Scorer)
		}
	}

	deltas := make([]ScorerDelta, 0, len(names))
	for _, name := range names {
		baseAggregate, inBaseline := baseline.Aggregate(name)
		candidateAggregate, inCandidate := candidate.Aggregate(name)
		deltas = append(deltas, ScorerDelta{
			Scorer:            name,
			BaselineMean:      baseAggregate.Mean,
			CandidateMean:     candidateAggregate.Mean,
			MeanDelta:         candidateAggregate.Mean - baseAggregate.Mean,
			BaselinePassRate:  baseAggregate.PassRate,
			CandidatePassRate: candidateAggregate.PassRate,
			PassRateDelta:     candidateAggregate.PassRate - baseAggregate.PassRate,
			BaselineScored:    baseAggregate.Scored,
			CandidateScored:   candidateAggregate.Scored,
			Present:           inBaseline && inCandidate,
		})
	}
	return deltas
}

// compareCases pairs case results by case ID and scorer name, and reports the
// pairs whose pass state changed. A case or scorer present on only one side is
// skipped: there is no pair to compare, and inventing one would report a
// regression where the measurement simply did not exist.
func compareCases(baselineCases, candidateCases []CaseResult) (regressions, improvements []CaseDelta) {
	if len(baselineCases) == 0 || len(candidateCases) == 0 {
		return nil, nil
	}
	type key struct {
		caseID CaseID
		scorer string
	}
	baselineScores := make(map[key]Score, len(baselineCases))
	for _, result := range baselineCases {
		for _, score := range result.Scores {
			baselineScores[key{result.CaseID, score.Scorer}] = score
		}
	}

	for _, result := range candidateCases {
		for _, score := range result.Scores {
			prior, ok := baselineScores[key{result.CaseID, score.Scorer}]
			if !ok || prior.Passed == score.Passed {
				continue
			}
			delta := CaseDelta{
				CaseID:          result.CaseID,
				Scorer:          score.Scorer,
				BaselineValue:   prior.Value,
				CandidateValue:  score.Value,
				Delta:           score.Value - prior.Value,
				BaselinePassed:  prior.Passed,
				CandidatePassed: score.Passed,
				Reason:          score.Reason,
			}
			if prior.Passed && !score.Passed {
				regressions = append(regressions, delta)
			} else {
				improvements = append(improvements, delta)
			}
		}
	}
	sortCaseDeltas(regressions)
	sortCaseDeltas(improvements)
	return regressions, improvements
}

// sortCaseDeltas orders deltas by case then scorer so a comparison is stable
// regardless of the order results were stored in.
func sortCaseDeltas(deltas []CaseDelta) {
	sort.SliceStable(deltas, func(i, j int) bool {
		if deltas[i].CaseID != deltas[j].CaseID {
			return deltas[i].CaseID < deltas[j].CaseID
		}
		return deltas[i].Scorer < deltas[j].Scorer
	})
}
