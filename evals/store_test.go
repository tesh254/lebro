package evals_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/tesh254/lebro/evals"
)

func storedRecord(id evals.ExperimentID, version evals.DatasetVersion) evals.ExperimentRecord {
	return evals.ExperimentRecord{
		ID:             id,
		DatasetID:      "capitals",
		DatasetVersion: version,
		Cases:          2,
		Metadata:       map[string]string{"commit": "abc123"},
		Scorers:        []evals.ScorerAggregate{{Scorer: "exact_match", Scored: 2, Passed: 1, Mean: 0.5, PassRate: 0.5}},
	}
}

func TestMemoryRepositoryRoundTrip(t *testing.T) {
	repository := evals.NewMemoryRepository()
	ctx := t.Context()

	if err := repository.SaveExperiment(ctx, storedRecord("exp-1", "v1")); err != nil {
		t.Fatalf("SaveExperiment() = %v", err)
	}
	results := []evals.CaseResult{
		{CaseID: "france", Index: 0, Scores: []evals.Score{{Scorer: "exact_match", Value: 1, Passed: true}}},
		{CaseID: "japan", Index: 1, ScorerFailures: []evals.ScorerFailure{{Scorer: "exact_match", Message: "boom"}}},
	}
	if err := repository.AppendCaseResults(ctx, "exp-1", results); err != nil {
		t.Fatalf("AppendCaseResults() = %v", err)
	}

	record, err := repository.GetExperiment(ctx, "exp-1")
	if err != nil {
		t.Fatalf("GetExperiment() = %v", err)
	}
	if record.Cases != 2 || record.Metadata["commit"] != "abc123" {
		t.Fatalf("record = %+v, want the stored values", record)
	}

	stored, err := repository.CaseResultsByExperiment(ctx, "exp-1")
	if err != nil {
		t.Fatalf("CaseResultsByExperiment() = %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("got %d results, want 2", len(stored))
	}
	if len(stored[0].Scores) != 1 || len(stored[1].ScorerFailures) != 1 {
		t.Fatalf("results lost their scores or failures: %+v", stored)
	}
}

// TestMemoryRepositoryOrdersResultsByIndex pins that stored results read back in
// dataset order even when appended out of order, which a chunked writer may do.
func TestMemoryRepositoryOrdersResultsByIndex(t *testing.T) {
	repository := evals.NewMemoryRepository()
	ctx := t.Context()
	if err := repository.SaveExperiment(ctx, storedRecord("exp-1", "v1")); err != nil {
		t.Fatalf("SaveExperiment() = %v", err)
	}
	if err := repository.AppendCaseResults(ctx, "exp-1", []evals.CaseResult{{CaseID: "third", Index: 2}}); err != nil {
		t.Fatalf("AppendCaseResults() = %v", err)
	}
	if err := repository.AppendCaseResults(ctx, "exp-1", []evals.CaseResult{
		{CaseID: "first", Index: 0},
		{CaseID: "second", Index: 1},
	}); err != nil {
		t.Fatalf("AppendCaseResults() = %v", err)
	}

	stored, err := repository.CaseResultsByExperiment(ctx, "exp-1")
	if err != nil {
		t.Fatalf("CaseResultsByExperiment() = %v", err)
	}
	want := []evals.CaseID{"first", "second", "third"}
	for i, result := range stored {
		if result.CaseID != want[i] {
			t.Fatalf("result %d is %q, want %q", i, result.CaseID, want[i])
		}
	}
}

func TestMemoryRepositoryReportsNotFound(t *testing.T) {
	repository := evals.NewMemoryRepository()
	ctx := t.Context()
	if _, err := repository.GetExperiment(ctx, "missing"); !errors.Is(err, evals.ErrNotFound) {
		t.Fatalf("GetExperiment() = %v, want ErrNotFound", err)
	}
	if _, err := repository.CaseResultsByExperiment(ctx, "missing"); !errors.Is(err, evals.ErrNotFound) {
		t.Fatalf("CaseResultsByExperiment() = %v, want ErrNotFound", err)
	}
}

// TestMemoryRepositoryDistinguishesEmptyFromMissing pins the difference between a
// stored experiment with no results yet and an experiment that does not exist.
func TestMemoryRepositoryDistinguishesEmptyFromMissing(t *testing.T) {
	repository := evals.NewMemoryRepository()
	ctx := t.Context()
	if err := repository.SaveExperiment(ctx, storedRecord("exp-empty", "v1")); err != nil {
		t.Fatalf("SaveExperiment() = %v", err)
	}
	results, err := repository.CaseResultsByExperiment(ctx, "exp-empty")
	if err != nil {
		t.Fatalf("CaseResultsByExperiment() = %v, want nil for a stored experiment with no results", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0", len(results))
	}
}

// TestMemoryRepositoryReplacesRecordByID pins that re-saving one experiment does
// not accumulate duplicates, which would make ListExperiments misleading.
func TestMemoryRepositoryReplacesRecordByID(t *testing.T) {
	repository := evals.NewMemoryRepository()
	ctx := t.Context()
	first := storedRecord("exp-1", "v1")
	if err := repository.SaveExperiment(ctx, first); err != nil {
		t.Fatalf("SaveExperiment() = %v", err)
	}
	updated := first
	updated.Cases = 99
	if err := repository.SaveExperiment(ctx, updated); err != nil {
		t.Fatalf("SaveExperiment() = %v", err)
	}

	if got := len(repository.Experiments()); got != 1 {
		t.Fatalf("stored %d records, want 1", got)
	}
	record, err := repository.GetExperiment(ctx, "exp-1")
	if err != nil {
		t.Fatalf("GetExperiment() = %v", err)
	}
	if record.Cases != 99 {
		t.Fatalf("Cases = %d, want the replaced value 99", record.Cases)
	}
}

func TestMemoryRepositoryListFiltersByDatasetAndVersion(t *testing.T) {
	repository := evals.NewMemoryRepository()
	ctx := t.Context()
	for _, record := range []evals.ExperimentRecord{
		storedRecord("exp-1", "v1"),
		storedRecord("exp-2", "v1"),
		storedRecord("exp-3", "v2"),
	} {
		if err := repository.SaveExperiment(ctx, record); err != nil {
			t.Fatalf("SaveExperiment() = %v", err)
		}
	}
	other := storedRecord("exp-4", "v1")
	other.DatasetID = "other"
	if err := repository.SaveExperiment(ctx, other); err != nil {
		t.Fatalf("SaveExperiment() = %v", err)
	}

	all, err := repository.ListExperiments(ctx, "capitals", "")
	if err != nil {
		t.Fatalf("ListExperiments() = %v", err)
	}
	// Newest first, and the other dataset is excluded.
	wantAll := []evals.ExperimentID{"exp-3", "exp-2", "exp-1"}
	if len(all) != len(wantAll) {
		t.Fatalf("got %d records, want %d", len(all), len(wantAll))
	}
	for i, record := range all {
		if record.ID != wantAll[i] {
			t.Fatalf("record %d is %q, want %q", i, record.ID, wantAll[i])
		}
	}

	versioned, err := repository.ListExperiments(ctx, "capitals", "v1")
	if err != nil {
		t.Fatalf("ListExperiments(v1) = %v", err)
	}
	if len(versioned) != 2 {
		t.Fatalf("got %d v1 records, want 2", len(versioned))
	}
	for _, record := range versioned {
		if record.DatasetVersion != "v1" {
			t.Fatalf("record %q has version %q, want v1", record.ID, record.DatasetVersion)
		}
	}
}

// TestMemoryRepositoryReturnsCallerOwnedCopies pins the ownership contract: a
// caller mutating what it read must not corrupt the store, and a caller mutating
// what it wrote must not corrupt it either.
func TestMemoryRepositoryReturnsCallerOwnedCopies(t *testing.T) {
	repository := evals.NewMemoryRepository()
	ctx := t.Context()
	if err := repository.SaveExperiment(ctx, storedRecord("exp-1", "v1")); err != nil {
		t.Fatalf("SaveExperiment() = %v", err)
	}
	written := []evals.CaseResult{{
		CaseID: "france",
		Index:  0,
		Output: evals.Output{
			Structured: json.RawMessage(`{"a":1}`),
			Metadata:   map[string]string{"tag": "smoke"},
		},
		Scores: []evals.Score{{Scorer: "exact_match", Value: 1, Passed: true, Metadata: map[string]string{"k": "v"}}},
	}}
	if err := repository.AppendCaseResults(ctx, "exp-1", written); err != nil {
		t.Fatalf("AppendCaseResults() = %v", err)
	}

	// Mutating what was written must not reach the store.
	written[0].Scores[0].Value = 0
	written[0].Output.Metadata["tag"] = "changed"
	written[0].Output.Structured[2] = 'X'

	// Mutating what was read must not reach the store either.
	first, err := repository.CaseResultsByExperiment(ctx, "exp-1")
	if err != nil {
		t.Fatalf("CaseResultsByExperiment() = %v", err)
	}
	first[0].Scores[0].Value = 42
	first[0].Scores[0].Metadata["k"] = "mutated"
	first[0].Output.Metadata["tag"] = "mutated"

	second, err := repository.CaseResultsByExperiment(ctx, "exp-1")
	if err != nil {
		t.Fatalf("CaseResultsByExperiment() = %v", err)
	}
	if second[0].Scores[0].Value != 1 {
		t.Fatalf("stored score Value = %v, want 1", second[0].Scores[0].Value)
	}
	if second[0].Scores[0].Metadata["k"] != "v" {
		t.Fatalf("stored score metadata = %q, want v", second[0].Scores[0].Metadata["k"])
	}
	if second[0].Output.Metadata["tag"] != "smoke" {
		t.Fatalf("stored output metadata = %q, want smoke", second[0].Output.Metadata["tag"])
	}
	if string(second[0].Output.Structured) != `{"a":1}` {
		t.Fatalf("stored structured output = %s, want {\"a\":1}", second[0].Output.Structured)
	}

	record, err := repository.GetExperiment(ctx, "exp-1")
	if err != nil {
		t.Fatalf("GetExperiment() = %v", err)
	}
	record.Metadata["commit"] = "mutated"
	record.Scorers[0].Mean = 99
	again, err := repository.GetExperiment(ctx, "exp-1")
	if err != nil {
		t.Fatalf("GetExperiment() = %v", err)
	}
	if again.Metadata["commit"] != "abc123" {
		t.Fatalf("stored record metadata = %q, want abc123", again.Metadata["commit"])
	}
	if again.Scorers[0].Mean != 0.5 {
		t.Fatalf("stored aggregate Mean = %v, want 0.5", again.Scorers[0].Mean)
	}
}

func TestMemoryRepositoryRejectsRecordWithoutID(t *testing.T) {
	repository := evals.NewMemoryRepository()
	if err := repository.SaveExperiment(t.Context(), evals.ExperimentRecord{DatasetID: "capitals"}); err == nil {
		t.Fatal("SaveExperiment(no ID) = nil, want an error")
	}
	if err := repository.AppendCaseResults(t.Context(), "", []evals.CaseResult{{CaseID: "a"}}); err == nil {
		t.Fatal("AppendCaseResults(no ID) = nil, want an error")
	}
}

// TestMemoryRepositoryZeroValueIsUsable pins the documented zero-value contract.
func TestMemoryRepositoryZeroValueIsUsable(t *testing.T) {
	var repository evals.MemoryRepository
	ctx := t.Context()
	if err := repository.SaveExperiment(ctx, storedRecord("exp-1", "v1")); err != nil {
		t.Fatalf("SaveExperiment() = %v", err)
	}
	if err := repository.AppendCaseResults(ctx, "exp-1", []evals.CaseResult{{CaseID: "a"}}); err != nil {
		t.Fatalf("AppendCaseResults() = %v", err)
	}
	if _, err := repository.GetExperiment(ctx, "exp-1"); err != nil {
		t.Fatalf("GetExperiment() = %v", err)
	}
}

// TestMemoryRepositoryIsSafeForConcurrentUse exercises the documented concurrency
// contract under -race.
func TestMemoryRepositoryIsSafeForConcurrentUse(t *testing.T) {
	repository := evals.NewMemoryRepository()
	ctx := context.Background()
	done := make(chan struct{})

	for i := range 8 {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			id := evals.ExperimentID(string(rune('a' + i)))
			if err := repository.SaveExperiment(ctx, storedRecord(id, "v1")); err != nil {
				t.Errorf("SaveExperiment() = %v", err)
				return
			}
			if err := repository.AppendCaseResults(ctx, id, []evals.CaseResult{{CaseID: "c", Index: 0}}); err != nil {
				t.Errorf("AppendCaseResults() = %v", err)
				return
			}
			if _, err := repository.ListExperiments(ctx, "capitals", ""); err != nil {
				t.Errorf("ListExperiments() = %v", err)
			}
		}(i)
	}
	for range 8 {
		<-done
	}
	if got := len(repository.Experiments()); got != 8 {
		t.Fatalf("stored %d records, want 8", got)
	}
}
