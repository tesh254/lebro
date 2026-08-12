package evals

import (
	"errors"
	"reflect"
)

// isNilInterface reports whether an interface value holds a nil pointer, map,
// slice, func, channel, or interface. A typed nil is non-nil as an interface, so
// `value == nil` misses it and the first method call panics; construction-time
// validation checks for both.
func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return reflected.IsNil()
	default:
		return false
	}
}

var (
	// ErrInvalidDataset is returned when a dataset cannot be evaluated: it has
	// no ID, no cases, or duplicate case IDs.
	ErrInvalidDataset = errors.New("lebro/evals: invalid dataset")
	// ErrInvalidCase is returned when a case carries neither a JSON input nor
	// messages, or has no ID.
	ErrInvalidCase = errors.New("lebro/evals: invalid case")
	// ErrInvalidScorer is returned when a scorer's configuration cannot produce
	// a meaningful score, or when two scorers in one experiment share a name.
	ErrInvalidScorer = errors.New("lebro/evals: invalid scorer")
	// ErrNoTarget is returned when an experiment is configured without a target
	// to evaluate.
	ErrNoTarget = errors.New("lebro/evals: experiment requires a target")
	// ErrDatasetVersionMismatch is returned by Compare when two experiment
	// records do not describe the same dataset at the same version. Comparing
	// them would produce a delta between different questions.
	ErrDatasetVersionMismatch = errors.New("lebro/evals: experiment records describe different dataset versions")
	// ErrScorerFailed wraps the cause recorded in a ScorerFailure. A scorer
	// failure is reported separately from the target's run outcome, so it never
	// reaches the caller of Experiment.Run as an error.
	ErrScorerFailed = errors.New("lebro/evals: scorer failed")
	// ErrTargetUnsupportedCase is returned by a Target when a case does not
	// carry the input shape the target needs — a message-centric target given a
	// case with only JSON input, or the reverse.
	ErrTargetUnsupportedCase = errors.New("lebro/evals: target cannot invoke case")
	// ErrNotFound is returned by a Repository when it holds no record for an
	// identifier.
	ErrNotFound = errors.New("lebro/evals: record not found")
)
