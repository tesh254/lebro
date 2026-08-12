package evals

import (
	"context"
	"fmt"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tesh254/lebro"
)

// DefaultConcurrency is the number of cases evaluated in parallel when
// ExperimentConfig.Concurrency is zero.
const DefaultConcurrency = 4

// ScorerFailure records a scorer that could not measure an output.
//
// It is stored alongside — never instead of — the case's target outcome, so a
// broken judge never reads as a broken target. Panicked marks a recovered panic,
// with Stack retained because a panicking scorer is a defect worth debugging
// rather than a result worth aggregating.
type ScorerFailure struct {
	Scorer   string `json:"scorer"`
	CaseID   CaseID `json:"case_id,omitempty"`
	Message  string `json:"message"`
	Panicked bool   `json:"panicked,omitempty"`
	Stack    string `json:"stack,omitempty"`
}

// CaseResult is the outcome of one case: what the target produced, and what each
// scorer made of it.
//
// Status and Failure describe the target only. Scores and ScorerFailures describe
// the measurement. A case can have a successful target run and a failed scorer, a
// failed target run and no scores at all, or any other combination — which is the
// point of keeping them apart.
type CaseResult struct {
	CaseID CaseID `json:"case_id"`
	// Index is the case's position in the dataset. Results are ordered by it, so
	// a record does not depend on worker scheduling.
	Index  int             `json:"index"`
	Output Output          `json:"output,omitzero"`
	Status lebro.RunStatus `json:"status,omitempty"`
	// Failure is the target's error message, empty when the target succeeded. A
	// scorer error never appears here.
	Failure        string          `json:"failure,omitempty"`
	Scores         []Score         `json:"scores,omitempty"`
	ScorerFailures []ScorerFailure `json:"scorer_failures,omitempty"`
	StartedAt      time.Time       `json:"started_at"`
	FinishedAt     time.Time       `json:"finished_at"`
}

// Clone returns a deep copy of the case result.
func (r CaseResult) Clone() CaseResult {
	r.Output = r.Output.Clone()
	if r.Scores != nil {
		scores := make([]Score, len(r.Scores))
		for i, score := range r.Scores {
			scores[i] = score.Clone()
		}
		r.Scores = scores
	}
	if r.ScorerFailures != nil {
		r.ScorerFailures = append([]ScorerFailure(nil), r.ScorerFailures...)
	}
	return r
}

// TargetSucceeded reports whether the target produced an answer for this case.
func (r CaseResult) TargetSucceeded() bool { return r.Failure == "" }

// ScorerAggregate summarizes one scorer across every case it measured.
//
// Mean is over the cases the scorer actually scored, so a scorer that failed on
// half the dataset reports the mean of the half it measured rather than a value
// diluted by absent measurements. Failures counts the cases it could not measure,
// which is what makes a partial Mean readable rather than misleading.
type ScorerAggregate struct {
	Scorer   string  `json:"scorer"`
	Scored   int     `json:"scored"`
	Passed   int     `json:"passed"`
	Failures int     `json:"failures"`
	Mean     float64 `json:"mean"`
	PassRate float64 `json:"pass_rate"`
}

// ExperimentRecord is the durable result of running a dataset against a target.
//
// DatasetVersion is the hash of the cases as run, which is what makes a later
// comparison trustworthy. TargetFailures and ScorerFailures are counted
// separately for the same reason they are stored separately on each case.
type ExperimentRecord struct {
	ID             ExperimentID      `json:"id"`
	Name           string            `json:"name,omitempty"`
	DatasetID      DatasetID         `json:"dataset_id"`
	DatasetVersion DatasetVersion    `json:"dataset_version"`
	TargetName     string            `json:"target_name,omitempty"`
	Cases          int               `json:"cases"`
	TargetFailures int               `json:"target_failures"`
	ScorerFailures int               `json:"scorer_failures"`
	Scorers        []ScorerAggregate `json:"scorers,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	StartedAt      time.Time         `json:"started_at"`
	FinishedAt     time.Time         `json:"finished_at"`
}

// Clone returns a deep copy of the record.
func (r ExperimentRecord) Clone() ExperimentRecord {
	if r.Scorers != nil {
		r.Scorers = append([]ScorerAggregate(nil), r.Scorers...)
	}
	r.Metadata = cloneMetadata(r.Metadata)
	return r
}

// Aggregate returns the named scorer's aggregate and whether it was present.
func (r ExperimentRecord) Aggregate(name string) (ScorerAggregate, bool) {
	for _, aggregate := range r.Scorers {
		if aggregate.Scorer == name {
			return aggregate, true
		}
	}
	return ScorerAggregate{}, false
}

// ExperimentConfig assembles an Experiment. Dataset, Target, and at least one
// Scorer are required.
type ExperimentConfig struct {
	// Name labels the experiment in stored records — typically a build or commit
	// reference, so two records can be told apart by what produced them.
	Name    string
	Dataset Dataset
	Target  Target
	Scorers []Scorer
	// Repository persists the record and its case results. Nil runs the
	// experiment and returns the record without storing it.
	Repository Repository
	// Concurrency bounds cases evaluated in parallel. Zero selects
	// DefaultConcurrency; a negative value evaluates one case at a time.
	Concurrency int
	// Metadata is stored verbatim on the record.
	Metadata map[string]string
	// Clock supplies timestamps. Nil uses the system clock.
	Clock lebro.Clock
	// IDs generates the experiment ID. Nil derives one from the dataset ID,
	// version, and start time.
	IDs ExperimentIDSource
}

// ExperimentIDSource generates experiment identifiers. Implementations must be
// safe for concurrent use.
type ExperimentIDSource interface {
	NewExperimentID() ExperimentID
}

// Experiment runs a dataset against a target and scores every case.
//
// The zero value is not usable; construct one with New. An Experiment is
// reusable: each Run produces an independent record.
type Experiment struct {
	name        string
	dataset     Dataset
	version     DatasetVersion
	target      Target
	scorers     []Scorer
	repository  Repository
	concurrency int
	metadata    map[string]string
	clock       lebro.Clock
	ids         ExperimentIDSource
	runSeq      atomic.Uint64
}

// New validates the configuration and returns an Experiment. It rejects an
// invalid dataset, a missing target, an empty scorer list, a nil scorer, an
// unnamed scorer, and two scorers sharing a name — duplicate names would make an
// aggregate ambiguous and a comparison meaningless.
func New(config ExperimentConfig) (*Experiment, error) {
	if err := config.Dataset.Validate(); err != nil {
		return nil, err
	}
	if isNilInterface(config.Target) {
		return nil, ErrNoTarget
	}
	if len(config.Scorers) == 0 {
		return nil, fmt.Errorf("%w: experiment requires at least one scorer", ErrInvalidScorer)
	}
	seen := make(map[string]struct{}, len(config.Scorers))
	for i, scorer := range config.Scorers {
		// A typed-nil scorer is non-nil as an interface, so it would pass a plain
		// nil check and panic on the Name() call below.
		if isNilInterface(scorer) {
			return nil, fmt.Errorf("%w: scorer %d is nil", ErrInvalidScorer, i)
		}
		name := scorer.Name()
		if name == "" {
			return nil, fmt.Errorf("%w: scorer %d has no name", ErrInvalidScorer, i)
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("%w: duplicate scorer name %q", ErrInvalidScorer, name)
		}
		seen[name] = struct{}{}
	}

	concurrency := config.Concurrency
	if concurrency == 0 {
		concurrency = DefaultConcurrency
	}
	if concurrency < 1 {
		concurrency = 1
	}
	clock := config.Clock
	if clock == nil {
		clock = systemClock{}
	}

	dataset := config.Dataset.Clone()
	return &Experiment{
		name:        config.Name,
		dataset:     dataset,
		version:     dataset.Version(),
		target:      config.Target,
		scorers:     append([]Scorer(nil), config.Scorers...),
		repository:  config.Repository,
		concurrency: concurrency,
		metadata:    cloneMetadata(config.Metadata),
		clock:       clock,
		ids:         config.IDs,
	}, nil
}

// DatasetVersion returns the version of the dataset this experiment runs.
func (e *Experiment) DatasetVersion() DatasetVersion { return e.version }

// Run evaluates every case and returns the aggregated record.
//
// Run reports an error only when the experiment itself could not proceed: a
// cancelled context, or a repository write that failed. A target failure and a
// scorer failure are both results — they are recorded on the case and counted on
// the record, and neither makes Run return an error, because a dataset that
// exposes a regression has done its job rather than malfunctioned.
func (e *Experiment) Run(ctx context.Context) (ExperimentRecord, []CaseResult, error) {
	if err := ctx.Err(); err != nil {
		return ExperimentRecord{}, nil, err
	}
	startedAt := e.clock.Now()
	results := e.evaluateCases(ctx, startedAt)

	// A cancelled context is the caller's decision, not a target verdict; report
	// it rather than a record that silently covers fewer cases than the dataset.
	if err := ctx.Err(); err != nil {
		return ExperimentRecord{}, nil, err
	}

	record := ExperimentRecord{
		ID:             e.newExperimentID(startedAt),
		Name:           e.name,
		DatasetID:      e.dataset.ID,
		DatasetVersion: e.version,
		TargetName:     e.target.Name(),
		Cases:          len(results),
		Metadata:       cloneMetadata(e.metadata),
		StartedAt:      startedAt,
		FinishedAt:     e.clock.Now(),
	}
	record.TargetFailures, record.ScorerFailures, record.Scorers = summarize(e.scorers, results)

	if e.repository != nil {
		if err := e.repository.SaveExperiment(ctx, record); err != nil {
			return ExperimentRecord{}, nil, fmt.Errorf("save experiment: %w", err)
		}
		stored := make([]CaseResult, len(results))
		for i, result := range results {
			stored[i] = result.Clone()
		}
		if err := e.repository.AppendCaseResults(ctx, record.ID, stored); err != nil {
			return ExperimentRecord{}, nil, fmt.Errorf("append case results: %w", err)
		}
	}
	return record, results, nil
}

// evaluateCases runs every case across a bounded worker pool. Results are written
// to a preallocated slice at each case's own index, so the returned order is the
// dataset's order regardless of which worker finished first.
func (e *Experiment) evaluateCases(ctx context.Context, startedAt time.Time) []CaseResult {
	results := make([]CaseResult, len(e.dataset.Cases))
	indexes := make(chan int)
	var wg sync.WaitGroup

	workers := min(e.concurrency, len(e.dataset.Cases))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range indexes {
				results[index] = e.evaluateCase(ctx, index, e.dataset.Cases[index])
			}
		}()
	}

dispatch:
	for index := range e.dataset.Cases {
		select {
		case indexes <- index:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(indexes)
	wg.Wait()

	// Stamp a start time on cases that were never dispatched so a cancelled run's
	// partial results are still self-describing.
	for i := range results {
		if results[i].StartedAt.IsZero() {
			results[i].CaseID = e.dataset.Cases[i].ID
			results[i].Index = i
			results[i].StartedAt = startedAt
			results[i].FinishedAt = startedAt
		}
	}
	return results
}

// evaluateCase invokes the target and, when it produced an answer, every scorer.
func (e *Experiment) evaluateCase(ctx context.Context, index int, testCase Case) CaseResult {
	result := CaseResult{CaseID: testCase.ID, Index: index, StartedAt: e.clock.Now()}
	// The case is copied because a target may retain or mutate what it is given,
	// and the captured dataset is reused by every later run and every scorer.
	output, err := e.target.Invoke(ctx, testCase.Clone())
	result.Output = output
	result.Status = output.Status
	if err != nil {
		result.Failure = err.Error()
		if result.Status == "" {
			result.Status = lebro.RunStatusFailed
		}
		result.FinishedAt = e.clock.Now()
		return result
	}
	if result.Status == "" {
		result.Status = lebro.RunStatusSucceeded
	}

	for _, scorer := range e.scorers {
		score, scoreErr := safeScore(ctx, scorer, testCase, output)
		if scoreErr != nil {
			result.ScorerFailures = append(result.ScorerFailures, *scoreErr)
			continue
		}
		result.Scores = append(result.Scores, score)
	}
	result.FinishedAt = e.clock.Now()
	return result
}

// safeScore calls a scorer and converts an error or a panic into a
// ScorerFailure. A panicking scorer is recovered so one bad judge cannot abandon
// the run: the remaining scorers still measure this case, and the remaining cases
// still run.
func safeScore(ctx context.Context, scorer Scorer, testCase Case, output Output) (score Score, failure *ScorerFailure) {
	name := scorer.Name()
	defer func() {
		if recovered := recover(); recovered != nil {
			score = Score{}
			failure = &ScorerFailure{
				Scorer:   name,
				CaseID:   testCase.ID,
				Message:  fmt.Sprintf("scorer panicked: %v", recovered),
				Panicked: true,
				Stack:    string(debug.Stack()),
			}
		}
	}()

	// The case and output are copied so a scorer that retains or mutates either
	// cannot corrupt the record or another scorer's view of the same case.
	scored, err := scorer.Score(ctx, testCase.Clone(), output.Clone())
	if err != nil {
		return Score{}, &ScorerFailure{Scorer: name, CaseID: testCase.ID, Message: err.Error()}
	}
	// The configured name always wins: a scorer that reported a different name
	// would be silently dropped from its own aggregate.
	scored.Scorer = name
	scored.CaseID = testCase.ID
	return scored, nil
}

// summarize counts failures and builds one aggregate per configured scorer, in
// the order the scorers were configured. A scorer that measured nothing still
// gets an aggregate, with a zero Mean and its failure count, so a comparison can
// tell "scored zero" from "was not run".
func summarize(scorers []Scorer, results []CaseResult) (targetFailures, scorerFailures int, aggregates []ScorerAggregate) {
	totals := make(map[string]*ScorerAggregate, len(scorers))
	aggregates = make([]ScorerAggregate, 0, len(scorers))
	for _, scorer := range scorers {
		aggregates = append(aggregates, ScorerAggregate{Scorer: scorer.Name()})
	}
	for i := range aggregates {
		totals[aggregates[i].Scorer] = &aggregates[i]
	}

	sums := make(map[string]float64, len(scorers))
	for _, result := range results {
		if !result.TargetSucceeded() {
			targetFailures++
		}
		for _, score := range result.Scores {
			aggregate, ok := totals[score.Scorer]
			if !ok {
				continue
			}
			aggregate.Scored++
			if score.Passed {
				aggregate.Passed++
			}
			sums[score.Scorer] += score.Value
		}
		for _, failure := range result.ScorerFailures {
			scorerFailures++
			if aggregate, ok := totals[failure.Scorer]; ok {
				aggregate.Failures++
			}
		}
	}

	for i := range aggregates {
		if aggregates[i].Scored > 0 {
			aggregates[i].Mean = sums[aggregates[i].Scorer] / float64(aggregates[i].Scored)
			aggregates[i].PassRate = float64(aggregates[i].Passed) / float64(aggregates[i].Scored)
		}
	}
	return targetFailures, scorerFailures, aggregates
}

// newExperimentID returns the configured source's ID, or a deterministic one
// derived from the dataset and start time.
func (e *Experiment) newExperimentID(startedAt time.Time) ExperimentID {
	if e.ids != nil {
		return e.ids.NewExperimentID()
	}
	version := string(e.version)
	if len(version) > 12 {
		version = version[:12]
	}
	// A monotonic sequence disambiguates repeated runs: a fixed or coarse clock
	// would otherwise hand two runs the same ID and silently overwrite history.
	sequence := e.runSeq.Add(1)
	return ExperimentID(fmt.Sprintf("%s-%s-%d-%d", e.dataset.ID, version, startedAt.UnixNano(), sequence))
}

// systemClock reads the wall clock.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// FixedExperimentIDSource returns the given IDs in order, then falls back to
// indexed IDs derived from the last one. It makes a test's records
// byte-identical across runs.
type FixedExperimentIDSource struct {
	mu  sync.Mutex
	ids []ExperimentID
	seq int
}

// NewFixedExperimentIDSource returns a source that hands out ids in order.
func NewFixedExperimentIDSource(ids ...ExperimentID) *FixedExperimentIDSource {
	return &FixedExperimentIDSource{ids: append([]ExperimentID(nil), ids...)}
}

var _ ExperimentIDSource = (*FixedExperimentIDSource)(nil)

// NewExperimentID returns the next scripted ID.
func (s *FixedExperimentIDSource) NewExperimentID() ExperimentID {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	if s.seq <= len(s.ids) {
		return s.ids[s.seq-1]
	}
	return ExperimentID(fmt.Sprintf("experiment-%d", s.seq))
}

// sortCaseResults orders results by dataset index. Repositories use it so stored
// results read back in dataset order even if they were appended out of order.
func sortCaseResults(results []CaseResult) {
	sort.SliceStable(results, func(i, j int) bool { return results[i].Index < results[j].Index })
}

// Error renders the failure so a stored record can be surfaced as an ordinary
// error.
func (f ScorerFailure) Error() string {
	return fmt.Sprintf("%s: %s", f.Scorer, f.Message)
}

// Unwrap ties a ScorerFailure to ErrScorerFailed so a caller inspecting stored
// failures can classify them with errors.Is.
func (f ScorerFailure) Unwrap() error { return ErrScorerFailed }

var _ error = ScorerFailure{}
