package evals_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/evals"
)

// answerTarget answers each case from a lookup table, failing cases with no
// entry so a test can mix successful and failing targets in one dataset.
type answerTarget struct {
	answers map[evals.CaseID]string
	fail    map[evals.CaseID]error
}

func (t answerTarget) Name() string { return "answer" }

func (t answerTarget) Invoke(_ context.Context, testCase evals.Case) (evals.Output, error) {
	if err, failing := t.fail[testCase.ID]; failing {
		return evals.Output{Status: lebro.RunStatusFailed}, err
	}
	return evals.Output{
		Text:   t.answers[testCase.ID],
		Status: lebro.RunStatusSucceeded,
		RunID:  lebro.RunID("run-" + string(testCase.ID)),
	}, nil
}

// Fixture answers differ per case and expectations differ per case, so a
// result-to-case mapping bug produces a wrong pass state rather than passing
// coincidentally.
func answerDataset() evals.Dataset {
	return evals.Dataset{
		ID: "capitals",
		Cases: []evals.Case{
			{ID: "france", Input: json.RawMessage(`"France"`), Expected: "Paris"},
			{ID: "japan", Input: json.RawMessage(`"Japan"`), Expected: "Tokyo"},
			{ID: "peru", Input: json.RawMessage(`"Peru"`), Expected: "Lima"},
		},
	}
}

func TestExperimentRetainsPerCaseResults(t *testing.T) {
	target := answerTarget{answers: map[evals.CaseID]string{
		"france": "Paris",
		"japan":  "Kyoto",
		"peru":   "Lima",
	}}
	scorer, err := evals.NewExactMatch(evals.ExactMatchConfig{})
	if err != nil {
		t.Fatalf("NewExactMatch() = %v", err)
	}
	experiment, err := evals.New(evals.ExperimentConfig{
		Dataset: answerDataset(),
		Target:  target,
		Scorers: []evals.Scorer{scorer},
		IDs:     evals.NewFixedExperimentIDSource("exp-1"),
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	record, results, err := experiment.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if record.ID != "exp-1" || record.Cases != 3 {
		t.Fatalf("record = (%q, %d cases), want (exp-1, 3)", record.ID, record.Cases)
	}
	if record.DatasetVersion != answerDataset().Version() {
		t.Fatalf("DatasetVersion = %q, want the dataset's own version", record.DatasetVersion)
	}
	if record.TargetName != "answer" {
		t.Fatalf("TargetName = %q, want answer", record.TargetName)
	}

	// Per-case results are retained in dataset order with each case's own verdict.
	wantPassed := map[evals.CaseID]bool{"france": true, "japan": false, "peru": true}
	wantOrder := []evals.CaseID{"france", "japan", "peru"}
	if len(results) != len(wantOrder) {
		t.Fatalf("got %d results, want %d", len(results), len(wantOrder))
	}
	for i, result := range results {
		if result.CaseID != wantOrder[i] {
			t.Fatalf("result %d is case %q, want %q", i, result.CaseID, wantOrder[i])
		}
		if result.Index != i {
			t.Fatalf("case %q has index %d, want %d", result.CaseID, result.Index, i)
		}
		if len(result.Scores) != 1 {
			t.Fatalf("case %q has %d scores, want 1", result.CaseID, len(result.Scores))
		}
		if got := result.Scores[0].Passed; got != wantPassed[result.CaseID] {
			t.Fatalf("case %q passed = %t, want %t", result.CaseID, got, wantPassed[result.CaseID])
		}
		if result.Scores[0].CaseID != result.CaseID {
			t.Fatalf("score on case %q carries case ID %q", result.CaseID, result.Scores[0].CaseID)
		}
	}

	aggregate, ok := record.Aggregate("exact_match")
	if !ok {
		t.Fatal("record carries no exact_match aggregate")
	}
	if aggregate.Scored != 3 || aggregate.Passed != 2 {
		t.Fatalf("aggregate = (%d scored, %d passed), want (3, 2)", aggregate.Scored, aggregate.Passed)
	}
	if aggregate.Mean != 2.0/3.0 || aggregate.PassRate != 2.0/3.0 {
		t.Fatalf("aggregate mean/pass rate = (%v, %v), want (%v, %v)",
			aggregate.Mean, aggregate.PassRate, 2.0/3.0, 2.0/3.0)
	}
}

// TestExperimentReportsScorerFailureSeparately is the ticket's third acceptance
// criterion: a scorer that cannot measure must not be recorded as a target that
// did not work.
func TestExperimentReportsScorerFailureSeparately(t *testing.T) {
	target := answerTarget{answers: map[evals.CaseID]string{
		"france": "Paris",
		"japan":  "Tokyo",
		"peru":   "Lima",
	}}
	working, err := evals.NewExactMatch(evals.ExactMatchConfig{})
	if err != nil {
		t.Fatalf("NewExactMatch() = %v", err)
	}
	broken := evals.ScorerFunc{
		ScorerName: "broken",
		Fn: func(_ context.Context, testCase evals.Case, _ evals.Output) (evals.Score, error) {
			if testCase.ID == "japan" {
				return evals.Score{}, errors.New("grader unavailable")
			}
			return evals.Score{Value: 1, Passed: true}, nil
		},
	}
	experiment, err := evals.New(evals.ExperimentConfig{
		Dataset: answerDataset(),
		Target:  target,
		Scorers: []evals.Scorer{working, broken},
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	record, results, err := experiment.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() = %v, want a scorer failure to be recorded rather than returned", err)
	}
	if record.ScorerFailures != 1 {
		t.Fatalf("ScorerFailures = %d, want 1", record.ScorerFailures)
	}
	if record.TargetFailures != 0 {
		t.Fatalf("TargetFailures = %d, want 0 — a scorer failure is not a target failure", record.TargetFailures)
	}

	japan := findResult(t, results, "japan")
	if !japan.TargetSucceeded() {
		t.Fatalf("case japan reports target failure %q despite a working target", japan.Failure)
	}
	if japan.Status != lebro.RunStatusSucceeded {
		t.Fatalf("case japan status = %q, want succeeded", japan.Status)
	}
	if len(japan.ScorerFailures) != 1 || japan.ScorerFailures[0].Scorer != "broken" {
		t.Fatalf("ScorerFailures = %+v, want one failure from broken", japan.ScorerFailures)
	}
	if !strings.Contains(japan.ScorerFailures[0].Message, "grader unavailable") {
		t.Fatalf("failure message = %q, want the scorer's cause", japan.ScorerFailures[0].Message)
	}
	// The other scorer still measured this case: one broken judge does not blind
	// the rest.
	if len(japan.Scores) != 1 || japan.Scores[0].Scorer != "exact_match" {
		t.Fatalf("Scores = %+v, want the working scorer's judgment", japan.Scores)
	}

	// The failing scorer's aggregate reports what it managed to measure and how
	// often it could not, rather than a mean diluted by absent measurements.
	aggregate, ok := record.Aggregate("broken")
	if !ok {
		t.Fatal("record carries no broken aggregate")
	}
	if aggregate.Scored != 2 || aggregate.Failures != 1 || aggregate.Mean != 1 {
		t.Fatalf("aggregate = (%d scored, %d failures, mean %v), want (2, 1, 1)",
			aggregate.Scored, aggregate.Failures, aggregate.Mean)
	}
}

// TestExperimentRecoversScorerPanic pins that one panicking judge cannot abandon
// the run.
func TestExperimentRecoversScorerPanic(t *testing.T) {
	panicking := evals.ScorerFunc{
		ScorerName: "panicking",
		Fn: func(_ context.Context, testCase evals.Case, _ evals.Output) (evals.Score, error) {
			if testCase.ID == "japan" {
				panic("nil map write")
			}
			return evals.Score{Value: 1, Passed: true}, nil
		},
	}
	experiment, err := evals.New(evals.ExperimentConfig{
		Dataset: answerDataset(),
		Target:  answerTarget{answers: map[evals.CaseID]string{"france": "Paris", "japan": "Tokyo", "peru": "Lima"}},
		Scorers: []evals.Scorer{panicking},
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	record, results, err := experiment.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() = %v, want a recovered panic", err)
	}
	if record.Cases != 3 {
		t.Fatalf("Cases = %d, want the run to complete all 3", record.Cases)
	}
	japan := findResult(t, results, "japan")
	if len(japan.ScorerFailures) != 1 || !japan.ScorerFailures[0].Panicked {
		t.Fatalf("ScorerFailures = %+v, want one panicked failure", japan.ScorerFailures)
	}
	if japan.ScorerFailures[0].Stack == "" {
		t.Fatal("panicked failure carries no stack")
	}
	if !japan.TargetSucceeded() {
		t.Fatal("a panicking scorer was recorded as a target failure")
	}
	if !errors.Is(japan.ScorerFailures[0], evals.ErrScorerFailed) {
		t.Fatal("ScorerFailure does not unwrap to ErrScorerFailed")
	}
}

// TestExperimentRecordsTargetFailureWithoutScoring pins the other side of the
// separation: when the target produced no answer there is nothing to measure, so
// no scores are invented.
func TestExperimentRecordsTargetFailureWithoutScoring(t *testing.T) {
	wantErr := errors.New("provider unavailable")
	scored := 0
	var mu sync.Mutex
	counting := evals.ScorerFunc{
		ScorerName: "counting",
		Fn: func(context.Context, evals.Case, evals.Output) (evals.Score, error) {
			mu.Lock()
			scored++
			mu.Unlock()
			return evals.Score{Value: 1, Passed: true}, nil
		},
	}
	experiment, err := evals.New(evals.ExperimentConfig{
		Dataset: answerDataset(),
		Target: answerTarget{
			answers: map[evals.CaseID]string{"france": "Paris", "peru": "Lima"},
			fail:    map[evals.CaseID]error{"japan": wantErr},
		},
		Scorers: []evals.Scorer{counting},
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	record, results, err := experiment.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() = %v, want a target failure to be recorded rather than returned", err)
	}
	if record.TargetFailures != 1 || record.ScorerFailures != 0 {
		t.Fatalf("record = (%d target failures, %d scorer failures), want (1, 0)",
			record.TargetFailures, record.ScorerFailures)
	}
	japan := findResult(t, results, "japan")
	if japan.TargetSucceeded() {
		t.Fatal("failed case reports a successful target")
	}
	if !strings.Contains(japan.Failure, "provider unavailable") {
		t.Fatalf("Failure = %q, want the target's cause", japan.Failure)
	}
	if len(japan.Scores) != 0 || len(japan.ScorerFailures) != 0 {
		t.Fatalf("failed case carries scores %+v and failures %+v, want neither", japan.Scores, japan.ScorerFailures)
	}
	if scored != 2 {
		t.Fatalf("scorer ran %d times, want 2 (only the successful cases)", scored)
	}
}

// TestExperimentResultOrderIsIndependentOfCompletionOrder pins determinism: the
// slowest case is first in the dataset, so a result slice built in completion
// order would come back reordered.
func TestExperimentResultOrderIsIndependentOfCompletionOrder(t *testing.T) {
	dataset := evals.Dataset{ID: "ordered", Cases: []evals.Case{
		{ID: "slow", Input: json.RawMessage(`"a"`), Expected: "a"},
		{ID: "fast-1", Input: json.RawMessage(`"b"`), Expected: "b"},
		{ID: "fast-2", Input: json.RawMessage(`"c"`), Expected: "c"},
		{ID: "fast-3", Input: json.RawMessage(`"d"`), Expected: "d"},
	}}
	target := evals.TargetFunc{
		TargetName: "delayed",
		Fn: func(_ context.Context, testCase evals.Case) (evals.Output, error) {
			if testCase.ID == "slow" {
				time.Sleep(20 * time.Millisecond)
			}
			return evals.Output{Text: testCase.Expected, Status: lebro.RunStatusSucceeded}, nil
		},
	}
	scorer, err := evals.NewExactMatch(evals.ExactMatchConfig{})
	if err != nil {
		t.Fatalf("NewExactMatch() = %v", err)
	}
	experiment, err := evals.New(evals.ExperimentConfig{
		Dataset:     dataset,
		Target:      target,
		Scorers:     []evals.Scorer{scorer},
		Concurrency: 4,
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	_, results, err := experiment.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	want := []evals.CaseID{"slow", "fast-1", "fast-2", "fast-3"}
	for i, result := range results {
		if result.CaseID != want[i] {
			t.Fatalf("result %d is %q, want %q — results must follow dataset order", i, result.CaseID, want[i])
		}
		if !result.Scores[0].Passed {
			t.Fatalf("case %q failed, so results were mapped to the wrong cases", result.CaseID)
		}
	}
}

// TestExperimentIsDeterministicAcrossConcurrency runs the same dataset at several
// worker counts and requires identical records, which is what makes a stored
// record comparable across machines.
func TestExperimentIsDeterministicAcrossConcurrency(t *testing.T) {
	scorer, err := evals.NewExactMatch(evals.ExactMatchConfig{})
	if err != nil {
		t.Fatalf("NewExactMatch() = %v", err)
	}
	target := answerTarget{answers: map[evals.CaseID]string{"france": "Paris", "japan": "Kyoto", "peru": "Lima"}}

	var baseline []evals.CaseResult
	for _, concurrency := range []int{-1, 1, 2, 8} {
		experiment, err := evals.New(evals.ExperimentConfig{
			Dataset:     answerDataset(),
			Target:      target,
			Scorers:     []evals.Scorer{scorer},
			Concurrency: concurrency,
			Clock:       lebro.NewFixedClock(time.Unix(0, 0).UTC()),
			IDs:         evals.NewFixedExperimentIDSource("exp-fixed"),
		})
		if err != nil {
			t.Fatalf("New(concurrency=%d) = %v", concurrency, err)
		}
		_, results, err := experiment.Run(context.Background())
		if err != nil {
			t.Fatalf("Run(concurrency=%d) = %v", concurrency, err)
		}
		if baseline == nil {
			baseline = results
			continue
		}
		if got, want := mustJSON(t, results), mustJSON(t, baseline); got != want {
			t.Fatalf("concurrency=%d produced different results:\n got %s\nwant %s", concurrency, got, want)
		}
	}
}

func TestExperimentPersistsThroughRepository(t *testing.T) {
	repository := evals.NewMemoryRepository()
	scorer, err := evals.NewExactMatch(evals.ExactMatchConfig{})
	if err != nil {
		t.Fatalf("NewExactMatch() = %v", err)
	}
	experiment, err := evals.New(evals.ExperimentConfig{
		Dataset:    answerDataset(),
		Target:     answerTarget{answers: map[evals.CaseID]string{"france": "Paris", "japan": "Tokyo", "peru": "Lima"}},
		Scorers:    []evals.Scorer{scorer},
		Repository: repository,
		IDs:        evals.NewFixedExperimentIDSource("exp-stored"),
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	if _, _, err := experiment.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v", err)
	}

	stored, err := repository.GetExperiment(context.Background(), "exp-stored")
	if err != nil {
		t.Fatalf("GetExperiment() = %v", err)
	}
	if stored.Cases != 3 {
		t.Fatalf("stored Cases = %d, want 3", stored.Cases)
	}
	results, err := repository.CaseResultsByExperiment(context.Background(), "exp-stored")
	if err != nil {
		t.Fatalf("CaseResultsByExperiment() = %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("stored %d case results, want 3", len(results))
	}
	for i, result := range results {
		if result.Index != i {
			t.Fatalf("stored result %d has index %d", i, result.Index)
		}
	}
}

// TestExperimentReportsRepositoryFailure pins that a failed write is the one
// storage outcome a caller must not miss: a record that was never persisted must
// not read as a completed experiment.
func TestExperimentReportsRepositoryFailure(t *testing.T) {
	wantErr := errors.New("disk full")
	scorer, err := evals.NewExactMatch(evals.ExactMatchConfig{})
	if err != nil {
		t.Fatalf("NewExactMatch() = %v", err)
	}
	experiment, err := evals.New(evals.ExperimentConfig{
		Dataset:    answerDataset(),
		Target:     answerTarget{answers: map[evals.CaseID]string{"france": "Paris"}},
		Scorers:    []evals.Scorer{scorer},
		Repository: failingRepository{err: wantErr},
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	if _, _, err := experiment.Run(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Run() = %v, want %v", err, wantErr)
	}
}

func TestExperimentReportsCancellation(t *testing.T) {
	scorer, err := evals.NewExactMatch(evals.ExactMatchConfig{})
	if err != nil {
		t.Fatalf("NewExactMatch() = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	experiment, err := evals.New(evals.ExperimentConfig{
		Dataset: answerDataset(),
		Target: evals.TargetFunc{TargetName: "cancelling", Fn: func(_ context.Context, testCase evals.Case) (evals.Output, error) {
			cancel()
			return evals.Output{Text: testCase.Expected, Status: lebro.RunStatusSucceeded}, nil
		}},
		Scorers:     []evals.Scorer{scorer},
		Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	defer cancel()

	if _, _, err := experiment.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want context.Canceled", err)
	}
}

func TestExperimentRejectsAlreadyCancelledContext(t *testing.T) {
	scorer, err := evals.NewExactMatch(evals.ExactMatchConfig{})
	if err != nil {
		t.Fatalf("NewExactMatch() = %v", err)
	}
	experiment, err := evals.New(evals.ExperimentConfig{
		Dataset: answerDataset(),
		Target:  answerTarget{},
		Scorers: []evals.Scorer{scorer},
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := experiment.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want context.Canceled", err)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	valid, err := evals.NewExactMatch(evals.ExactMatchConfig{})
	if err != nil {
		t.Fatalf("NewExactMatch() = %v", err)
	}
	unnamed := evals.ScorerFunc{Fn: func(context.Context, evals.Case, evals.Output) (evals.Score, error) {
		return evals.Score{}, nil
	}}

	tests := []struct {
		name   string
		config evals.ExperimentConfig
		want   error
	}{
		{
			name:   "invalid dataset",
			config: evals.ExperimentConfig{Target: answerTarget{}, Scorers: []evals.Scorer{valid}},
			want:   evals.ErrInvalidDataset,
		},
		{
			name:   "no target",
			config: evals.ExperimentConfig{Dataset: answerDataset(), Scorers: []evals.Scorer{valid}},
			want:   evals.ErrNoTarget,
		},
		{
			name:   "no scorers",
			config: evals.ExperimentConfig{Dataset: answerDataset(), Target: answerTarget{}},
			want:   evals.ErrInvalidScorer,
		},
		{
			name: "nil scorer",
			config: evals.ExperimentConfig{
				Dataset: answerDataset(), Target: answerTarget{}, Scorers: []evals.Scorer{nil},
			},
			want: evals.ErrInvalidScorer,
		},
		{
			name: "unnamed scorer",
			config: evals.ExperimentConfig{
				Dataset: answerDataset(), Target: answerTarget{}, Scorers: []evals.Scorer{unnamed},
			},
			want: evals.ErrInvalidScorer,
		},
		{
			name: "duplicate scorer names",
			config: evals.ExperimentConfig{
				Dataset: answerDataset(), Target: answerTarget{}, Scorers: []evals.Scorer{valid, valid},
			},
			want: evals.ErrInvalidScorer,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := evals.New(test.config); !errors.Is(err, test.want) {
				t.Fatalf("New() = %v, want %v", err, test.want)
			}
		})
	}
}

// TestNewCopiesDatasetSoLaterEditsDoNotChangeTheRun pins that a caller mutating
// the dataset after construction cannot make the record's version disagree with
// the cases that were actually run.
func TestNewCopiesDatasetSoLaterEditsDoNotChangeTheRun(t *testing.T) {
	dataset := answerDataset()
	scorer, err := evals.NewExactMatch(evals.ExactMatchConfig{})
	if err != nil {
		t.Fatalf("NewExactMatch() = %v", err)
	}
	experiment, err := evals.New(evals.ExperimentConfig{
		Dataset: dataset,
		Target:  answerTarget{answers: map[evals.CaseID]string{"france": "Paris", "japan": "Tokyo", "peru": "Lima"}},
		Scorers: []evals.Scorer{scorer},
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	original := dataset.Version()
	dataset.Cases[0].Expected = "Lyon"

	record, results, err := experiment.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if record.DatasetVersion != original {
		t.Fatalf("DatasetVersion = %q, want the version captured at construction %q", record.DatasetVersion, original)
	}
	if france := findResult(t, results, "france"); !france.Scores[0].Passed {
		t.Fatal("run used the caller's later edit rather than the captured dataset")
	}
}

// TestFixedExperimentIDSourceFallsBackToIndexedIDs covers the source's behavior
// past the scripted IDs, so a reused experiment does not repeat an ID.
func TestFixedExperimentIDSourceFallsBackToIndexedIDs(t *testing.T) {
	source := evals.NewFixedExperimentIDSource("first")
	if got := source.NewExperimentID(); got != "first" {
		t.Fatalf("first ID = %q, want first", got)
	}
	second := source.NewExperimentID()
	if second == "first" || second == "" {
		t.Fatalf("second ID = %q, want a distinct generated ID", second)
	}
}

// TestExperimentDerivesIDWithoutSource covers the default ID path.
func TestExperimentDerivesIDWithoutSource(t *testing.T) {
	scorer, err := evals.NewExactMatch(evals.ExactMatchConfig{})
	if err != nil {
		t.Fatalf("NewExactMatch() = %v", err)
	}
	experiment, err := evals.New(evals.ExperimentConfig{
		Dataset: answerDataset(),
		Target:  answerTarget{answers: map[evals.CaseID]string{"france": "Paris"}},
		Scorers: []evals.Scorer{scorer},
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	record, _, err := experiment.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if !strings.HasPrefix(string(record.ID), "capitals-") {
		t.Fatalf("ID = %q, want a dataset-derived identifier", record.ID)
	}
}

type failingRepository struct{ err error }

func (r failingRepository) SaveExperiment(context.Context, evals.ExperimentRecord) error {
	return r.err
}

func (r failingRepository) AppendCaseResults(context.Context, evals.ExperimentID, []evals.CaseResult) error {
	return r.err
}

func (r failingRepository) GetExperiment(context.Context, evals.ExperimentID) (evals.ExperimentRecord, error) {
	return evals.ExperimentRecord{}, r.err
}

func (r failingRepository) CaseResultsByExperiment(context.Context, evals.ExperimentID) ([]evals.CaseResult, error) {
	return nil, r.err
}

func (r failingRepository) ListExperiments(context.Context, evals.DatasetID, evals.DatasetVersion) ([]evals.ExperimentRecord, error) {
	return nil, r.err
}

func findResult(t *testing.T, results []evals.CaseResult, id evals.CaseID) evals.CaseResult {
	t.Helper()
	for _, result := range results {
		if result.CaseID == id {
			return result
		}
	}
	t.Fatalf("no result for case %q", id)
	return evals.CaseResult{}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(encoded)
}
