package evals

import (
	"context"
	"fmt"
	"sync"
)

// Repository persists experiment records and their case results.
//
// It is deliberately separate from lebro.Store: evaluation results can live in a
// different database from threads and workflow state, and an evaluation write can
// never join the transaction that persists a workflow step.
//
// Implementations must be safe for concurrent use. The slices passed to
// AppendCaseResults are owned by the implementation and may be retained; the
// slices returned by the read methods are owned by the caller.
type Repository interface {
	// SaveExperiment stores an experiment record, replacing any record with the
	// same ID.
	SaveExperiment(ctx context.Context, record ExperimentRecord) error
	// AppendCaseResults stores case results for an experiment. It may be called
	// more than once for one experiment.
	AppendCaseResults(ctx context.Context, id ExperimentID, results []CaseResult) error
	// GetExperiment returns one record. It reports ErrNotFound when the ID is
	// unknown.
	GetExperiment(ctx context.Context, id ExperimentID) (ExperimentRecord, error)
	// CaseResultsByExperiment returns the experiment's case results in dataset
	// order. It reports ErrNotFound when the experiment is unknown, which is
	// distinguishable from a stored experiment that has no results yet.
	CaseResultsByExperiment(ctx context.Context, id ExperimentID) ([]CaseResult, error)
	// ListExperiments returns records for a dataset, newest first. An empty
	// version matches every version of the dataset.
	ListExperiments(ctx context.Context, datasetID DatasetID, version DatasetVersion) ([]ExperimentRecord, error)
}

// MemoryRepository is an in-memory Repository. It requires no database, making it
// suitable for tests, local development, and CI runs that only compare against
// the previous experiment in the same process. It is safe for concurrent use.
//
// Its zero value is ready for use. NewMemoryRepository is a convenience for
// callers who prefer an explicit constructor.
type MemoryRepository struct {
	mu          sync.RWMutex
	experiments []ExperimentRecord
	byID        map[ExperimentID]int
	results     map[ExperimentID][]CaseResult
}

// NewMemoryRepository returns an empty in-memory repository.
func NewMemoryRepository() *MemoryRepository { return &MemoryRepository{} }

var _ Repository = (*MemoryRepository)(nil)

// SaveExperiment stores a copy of the record, replacing any record with the same
// ID so a re-run of one experiment does not accumulate duplicates.
func (r *MemoryRepository) SaveExperiment(_ context.Context, record ExperimentRecord) error {
	if r == nil {
		return nil
	}
	if record.ID == "" {
		return fmt.Errorf("%w: experiment record requires an ID", ErrInvalidDataset)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byID == nil {
		r.byID = make(map[ExperimentID]int)
	}
	if index, exists := r.byID[record.ID]; exists {
		r.experiments[index] = record.Clone()
		return nil
	}
	r.byID[record.ID] = len(r.experiments)
	r.experiments = append(r.experiments, record.Clone())
	return nil
}

// AppendCaseResults stores copies of the case results.
func (r *MemoryRepository) AppendCaseResults(_ context.Context, id ExperimentID, results []CaseResult) error {
	if r == nil || len(results) == 0 {
		return nil
	}
	if id == "" {
		return fmt.Errorf("%w: case results require an experiment ID", ErrInvalidDataset)
	}
	stored := make([]CaseResult, len(results))
	for i, result := range results {
		stored[i] = result.Clone()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.results == nil {
		r.results = make(map[ExperimentID][]CaseResult)
	}
	r.results[id] = append(r.results[id], stored...)
	return nil
}

// GetExperiment returns a caller-owned copy of the record.
func (r *MemoryRepository) GetExperiment(_ context.Context, id ExperimentID) (ExperimentRecord, error) {
	if r == nil {
		return ExperimentRecord{}, ErrNotFound
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	index, exists := r.byID[id]
	if !exists {
		return ExperimentRecord{}, fmt.Errorf("%w: experiment %q", ErrNotFound, id)
	}
	return r.experiments[index].Clone(), nil
}

// CaseResultsByExperiment returns caller-owned copies of the results in dataset
// order.
func (r *MemoryRepository) CaseResultsByExperiment(_ context.Context, id ExperimentID) ([]CaseResult, error) {
	if r == nil {
		return nil, ErrNotFound
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, exists := r.byID[id]; !exists {
		return nil, fmt.Errorf("%w: experiment %q", ErrNotFound, id)
	}
	stored := r.results[id]
	results := make([]CaseResult, len(stored))
	for i, result := range stored {
		results[i] = result.Clone()
	}
	sortCaseResults(results)
	return results, nil
}

// ListExperiments returns caller-owned copies of the dataset's records, newest
// first. An empty version matches every version.
func (r *MemoryRepository) ListExperiments(_ context.Context, datasetID DatasetID, version DatasetVersion) ([]ExperimentRecord, error) {
	if r == nil {
		return nil, nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var matched []ExperimentRecord
	// Iterate in reverse insertion order so the newest record comes first without
	// depending on timestamps, which a fixed clock may repeat across records.
	for i := len(r.experiments) - 1; i >= 0; i-- {
		record := r.experiments[i]
		if record.DatasetID != datasetID {
			continue
		}
		if version != "" && record.DatasetVersion != version {
			continue
		}
		matched = append(matched, record.Clone())
	}
	return matched, nil
}

// Experiments returns caller-owned copies of every stored record in insertion
// order.
func (r *MemoryRepository) Experiments() []ExperimentRecord {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	records := make([]ExperimentRecord, len(r.experiments))
	for i, record := range r.experiments {
		records[i] = record.Clone()
	}
	return records
}
