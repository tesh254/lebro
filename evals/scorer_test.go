package evals_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/tesh254/lebro/evals"
)

func TestExactMatch(t *testing.T) {
	tests := []struct {
		name     string
		config   evals.ExactMatchConfig
		expected string
		output   evals.Output
		want     bool
	}{
		{
			name:     "identical text passes",
			expected: "Paris",
			output:   evals.Output{Text: "Paris"},
			want:     true,
		},
		{
			name:     "different text fails",
			expected: "Paris",
			output:   evals.Output{Text: "Lyon"},
			want:     false,
		},
		{
			name:     "case difference fails by default",
			expected: "Paris",
			output:   evals.Output{Text: "paris"},
			want:     false,
		},
		{
			name:     "case difference passes when ignored",
			config:   evals.ExactMatchConfig{IgnoreCase: true},
			expected: "Paris",
			output:   evals.Output{Text: "paris"},
			want:     true,
		},
		{
			name:     "surrounding space fails by default",
			expected: "Paris",
			output:   evals.Output{Text: "  Paris\n"},
			want:     false,
		},
		{
			name:     "surrounding space passes when trimmed",
			config:   evals.ExactMatchConfig{TrimSpace: true},
			expected: "Paris",
			output:   evals.Output{Text: "  Paris\n"},
			want:     true,
		},
		{
			name:     "structured selector reads structured payload",
			config:   evals.ExactMatchConfig{Selector: evals.SelectStructured},
			expected: `{"city":"Paris"}`,
			output:   evals.Output{Text: "ignored", Structured: json.RawMessage(`{"city":"Paris"}`)},
			want:     true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scorer, err := evals.NewExactMatch(test.config)
			if err != nil {
				t.Fatalf("NewExactMatch() = %v", err)
			}
			score, err := scorer.Score(context.Background(), evals.Case{ID: "c1", Expected: test.expected}, test.output)
			if err != nil {
				t.Fatalf("Score() = %v", err)
			}
			assertScore(t, score, test.want)
			if score.CaseID != "c1" {
				t.Fatalf("CaseID = %q, want %q", score.CaseID, "c1")
			}
		})
	}
}

func TestContains(t *testing.T) {
	tests := []struct {
		name     string
		config   evals.ContainsConfig
		expected string
		output   evals.Output
		want     bool
	}{
		{
			name:   "substring present passes",
			config: evals.ContainsConfig{Substring: "capital"},
			output: evals.Output{Text: "The capital is Paris"},
			want:   true,
		},
		{
			name:   "substring absent fails",
			config: evals.ContainsConfig{Substring: "capital"},
			output: evals.Output{Text: "Paris"},
			want:   false,
		},
		{
			name:     "falls back to the case expectation",
			expected: "Paris",
			output:   evals.Output{Text: "The capital is Paris"},
			want:     true,
		},
		{
			name:   "negate passes when absent",
			config: evals.ContainsConfig{Substring: "sorry", Negate: true},
			output: evals.Output{Text: "The capital is Paris"},
			want:   true,
		},
		{
			name:   "negate fails when present",
			config: evals.ContainsConfig{Substring: "sorry", Negate: true},
			output: evals.Output{Text: "I am sorry"},
			want:   false,
		},
		{
			name:   "ignore case matches",
			config: evals.ContainsConfig{Substring: "CAPITAL", IgnoreCase: true},
			output: evals.Output{Text: "The capital is Paris"},
			want:   true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scorer, err := evals.NewContains(test.config)
			if err != nil {
				t.Fatalf("NewContains() = %v", err)
			}
			score, err := scorer.Score(context.Background(), evals.Case{ID: "c1", Expected: test.expected}, test.output)
			if err != nil {
				t.Fatalf("Score() = %v", err)
			}
			assertScore(t, score, test.want)
		})
	}
}

// TestContainsWithoutNeedleFails pins the deliberate refusal to score: an empty
// needle would make strings.Contains return true for every output, silently
// passing a dataset that measures nothing.
func TestContainsWithoutNeedleFails(t *testing.T) {
	scorer, err := evals.NewContains(evals.ContainsConfig{})
	if err != nil {
		t.Fatalf("NewContains() = %v", err)
	}
	if _, err := scorer.Score(context.Background(), evals.Case{ID: "c1"}, evals.Output{Text: "anything"}); !errors.Is(err, evals.ErrInvalidScorer) {
		t.Fatalf("Score() = %v, want ErrInvalidScorer", err)
	}
}

func TestRegexp(t *testing.T) {
	scorer, err := evals.NewRegexp(evals.RegexpConfig{Pattern: `^\d{4}-\d{2}-\d{2}$`})
	if err != nil {
		t.Fatalf("NewRegexp() = %v", err)
	}
	matching, err := scorer.Score(context.Background(), evals.Case{ID: "c1"}, evals.Output{Text: "2026-08-12"})
	if err != nil {
		t.Fatalf("Score() = %v", err)
	}
	assertScore(t, matching, true)

	failing, err := scorer.Score(context.Background(), evals.Case{ID: "c2"}, evals.Output{Text: "August 12"})
	if err != nil {
		t.Fatalf("Score() = %v", err)
	}
	assertScore(t, failing, false)
	if failing.Reason == "" {
		t.Fatal("failing score carries no reason")
	}
}

func TestRegexpNegate(t *testing.T) {
	scorer, err := evals.NewRegexp(evals.RegexpConfig{Pattern: `(?i)as an ai`, Negate: true})
	if err != nil {
		t.Fatalf("NewRegexp() = %v", err)
	}
	clean, err := scorer.Score(context.Background(), evals.Case{ID: "c1"}, evals.Output{Text: "Paris."})
	if err != nil {
		t.Fatalf("Score() = %v", err)
	}
	assertScore(t, clean, true)

	hedged, err := scorer.Score(context.Background(), evals.Case{ID: "c2"}, evals.Output{Text: "As an AI, I cannot."})
	if err != nil {
		t.Fatalf("Score() = %v", err)
	}
	assertScore(t, hedged, false)
}

func TestRegexpRejectsInvalidConfig(t *testing.T) {
	if _, err := evals.NewRegexp(evals.RegexpConfig{}); !errors.Is(err, evals.ErrInvalidScorer) {
		t.Fatalf("NewRegexp(empty) = %v, want ErrInvalidScorer", err)
	}
	if _, err := evals.NewRegexp(evals.RegexpConfig{Pattern: `([`}); !errors.Is(err, evals.ErrInvalidScorer) {
		t.Fatalf("NewRegexp(bad pattern) = %v, want ErrInvalidScorer", err)
	}
}

func TestJSONEquals(t *testing.T) {
	scorer, err := evals.NewJSONEquals(evals.JSONEqualsConfig{})
	if err != nil {
		t.Fatalf("NewJSONEquals() = %v", err)
	}

	tests := []struct {
		name     string
		expected string
		output   evals.Output
		want     bool
	}{
		{
			name:     "key order and whitespace are ignored",
			expected: `{"a":1,"b":[2,3]}`,
			output:   evals.Output{Structured: json.RawMessage("{\n \"b\": [2, 3],\n \"a\": 1\n}")},
			want:     true,
		},
		{
			name:     "different value fails",
			expected: `{"a":1}`,
			output:   evals.Output{Structured: json.RawMessage(`{"a":2}`)},
			want:     false,
		},
		{
			name:     "different type fails",
			expected: `{"a":1}`,
			output:   evals.Output{Structured: json.RawMessage(`{"a":"1"}`)},
			want:     false,
		},
		{
			name:     "array order matters",
			expected: `[1,2]`,
			output:   evals.Output{Structured: json.RawMessage(`[2,1]`)},
			want:     false,
		},
		{
			name:     "invalid output JSON is a measured mismatch",
			expected: `{"a":1}`,
			output:   evals.Output{Structured: json.RawMessage(`{"a":`)},
			want:     false,
		},
		{
			// canonicalJSON must decode the whole input, not just its first
			// top-level value, or trailing garbage after a matching value would
			// score a pass.
			name:     "trailing garbage after a matching value is a measured mismatch",
			expected: `{"a":1}`,
			output:   evals.Output{Structured: json.RawMessage(`{"a":1} {"b":2}`)},
			want:     false,
		},
		{
			name:     "empty output against a non-empty expectation is a measured mismatch",
			expected: `{"a":1}`,
			output:   evals.Output{Structured: json.RawMessage(``)},
			want:     false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			score, err := scorer.Score(context.Background(), evals.Case{ID: "c1", Expected: test.expected}, test.output)
			if err != nil {
				t.Fatalf("Score() = %v, want a measured result rather than an error", err)
			}
			assertScore(t, score, test.want)
		})
	}
}

// TestJSONEqualsRejectsEmptyOnBothSides pins that empty is never valid JSON, even
// when it appears on both sides: a case with no Expected value paired with a
// target that produced no output is a measured mismatch rather than a vacuous
// pass, because neither side actually asserted anything comparable.
func TestJSONEqualsRejectsEmptyOnBothSides(t *testing.T) {
	scorer, err := evals.NewJSONEquals(evals.JSONEqualsConfig{})
	if err != nil {
		t.Fatalf("NewJSONEquals() = %v", err)
	}
	score, err := scorer.Score(context.Background(), evals.Case{ID: "c1", Expected: ""}, evals.Output{})
	if err != nil {
		t.Fatalf("Score() = %v, want a measured mismatch rather than an error", err)
	}
	assertScore(t, score, false)
}

func TestNumericRange(t *testing.T) {
	min, max := 0.0, 10.0
	scorer, err := evals.NewNumericRange(evals.NumericRangeConfig{Min: &min, Max: &max})
	if err != nil {
		t.Fatalf("NewNumericRange() = %v", err)
	}

	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{name: "inside range passes", output: "7.5", want: true},
		{name: "lower bound is inclusive", output: "0", want: true},
		{name: "upper bound is inclusive", output: "10", want: true},
		{name: "below range fails", output: "-1", want: false},
		{name: "above range fails", output: "10.1", want: false},
		{name: "non-numeric fails", output: "seven", want: false},
		{name: "surrounding space is tolerated", output: " 5 ", want: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			score, err := scorer.Score(context.Background(), evals.Case{ID: "c1"}, evals.Output{Text: test.output})
			if err != nil {
				t.Fatalf("Score() = %v", err)
			}
			assertScore(t, score, test.want)
		})
	}
}

func TestNumericRangeRejectsInvalidConfig(t *testing.T) {
	if _, err := evals.NewNumericRange(evals.NumericRangeConfig{}); !errors.Is(err, evals.ErrInvalidScorer) {
		t.Fatalf("NewNumericRange(no bounds) = %v, want ErrInvalidScorer", err)
	}
	min, max := 10.0, 1.0
	if _, err := evals.NewNumericRange(evals.NumericRangeConfig{Min: &min, Max: &max}); !errors.Is(err, evals.ErrInvalidScorer) {
		t.Fatalf("NewNumericRange(inverted) = %v, want ErrInvalidScorer", err)
	}
	nan := math.NaN()
	if _, err := evals.NewNumericRange(evals.NumericRangeConfig{Min: &nan}); !errors.Is(err, evals.ErrInvalidScorer) {
		t.Fatalf("NewNumericRange(NaN min) = %v, want ErrInvalidScorer", err)
	}
	if _, err := evals.NewNumericRange(evals.NumericRangeConfig{Max: &nan}); !errors.Is(err, evals.ErrInvalidScorer) {
		t.Fatalf("NewNumericRange(NaN max) = %v, want ErrInvalidScorer", err)
	}
}

// TestNumericRangeRejectsNaNValue pins that NaN compares false against every
// bound — a naive min/max check would let it satisfy any range, including one
// with no upper bound at all.
func TestNumericRangeRejectsNaNValue(t *testing.T) {
	min := 0.0
	scorer, err := evals.NewNumericRange(evals.NumericRangeConfig{Min: &min})
	if err != nil {
		t.Fatalf("NewNumericRange() = %v", err)
	}
	score, err := scorer.Score(context.Background(), evals.Case{ID: "c1"}, evals.Output{Text: "NaN"})
	if err != nil {
		t.Fatalf("Score() = %v, want a measured mismatch rather than an error", err)
	}
	assertScore(t, score, false)
}

// TestScorersHonorCancelledContext pins that every built-in scorer checks the
// context before judging: a scorer's own Scorer-interface contract commits to
// honoring cancellation, and a pre-cancelled context reaching Score should not
// silently produce a verdict.
func TestScorersHonorCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	testCase := evals.Case{ID: "c1", Expected: "x"}
	output := evals.Output{Text: "x", Structured: json.RawMessage(`"x"`)}

	exact, err := evals.NewExactMatch(evals.ExactMatchConfig{})
	if err != nil {
		t.Fatalf("NewExactMatch() = %v", err)
	}
	contains, err := evals.NewContains(evals.ContainsConfig{Substring: "x"})
	if err != nil {
		t.Fatalf("NewContains() = %v", err)
	}
	pattern, err := evals.NewRegexp(evals.RegexpConfig{Pattern: "x"})
	if err != nil {
		t.Fatalf("NewRegexp() = %v", err)
	}
	jsonEquals, err := evals.NewJSONEquals(evals.JSONEqualsConfig{Selector: evals.SelectStructured})
	if err != nil {
		t.Fatalf("NewJSONEquals() = %v", err)
	}
	min := 0.0
	numeric, err := evals.NewNumericRange(evals.NumericRangeConfig{Min: &min})
	if err != nil {
		t.Fatalf("NewNumericRange() = %v", err)
	}

	for _, scorer := range []evals.Scorer{exact, contains, pattern, jsonEquals, numeric} {
		if _, err := scorer.Score(ctx, testCase, output); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s.Score(cancelled ctx) = %v, want context.Canceled", scorer.Name(), err)
		}
	}
}

// TestNumericRangeCopiesBounds pins that a caller mutating the pointed-to bound
// after construction cannot change how later cases are judged.
func TestNumericRangeCopiesBounds(t *testing.T) {
	max := 10.0
	scorer, err := evals.NewNumericRange(evals.NumericRangeConfig{Max: &max})
	if err != nil {
		t.Fatalf("NewNumericRange() = %v", err)
	}
	max = 1.0

	score, err := scorer.Score(context.Background(), evals.Case{ID: "c1"}, evals.Output{Text: "5"})
	if err != nil {
		t.Fatalf("Score() = %v", err)
	}
	assertScore(t, score, true)
}

func TestScorersRejectUnknownSelector(t *testing.T) {
	bad := evals.TextSelector("elsewhere")
	if _, err := evals.NewExactMatch(evals.ExactMatchConfig{Selector: bad}); !errors.Is(err, evals.ErrInvalidScorer) {
		t.Fatalf("NewExactMatch = %v, want ErrInvalidScorer", err)
	}
	if _, err := evals.NewContains(evals.ContainsConfig{Substring: "x", Selector: bad}); !errors.Is(err, evals.ErrInvalidScorer) {
		t.Fatalf("NewContains = %v, want ErrInvalidScorer", err)
	}
	if _, err := evals.NewRegexp(evals.RegexpConfig{Pattern: "x", Selector: bad}); !errors.Is(err, evals.ErrInvalidScorer) {
		t.Fatalf("NewRegexp = %v, want ErrInvalidScorer", err)
	}
	if _, err := evals.NewJSONEquals(evals.JSONEqualsConfig{Selector: bad}); !errors.Is(err, evals.ErrInvalidScorer) {
		t.Fatalf("NewJSONEquals = %v, want ErrInvalidScorer", err)
	}
	min := 0.0
	if _, err := evals.NewNumericRange(evals.NumericRangeConfig{Min: &min, Selector: bad}); !errors.Is(err, evals.ErrInvalidScorer) {
		t.Fatalf("NewNumericRange = %v, want ErrInvalidScorer", err)
	}
}

func TestScorerFuncStampsIdentity(t *testing.T) {
	scorer := evals.ScorerFunc{
		ScorerName: "half",
		Fn: func(context.Context, evals.Case, evals.Output) (evals.Score, error) {
			return evals.Score{Value: 0.5, Passed: true}, nil
		},
	}
	score, err := scorer.Score(context.Background(), evals.Case{ID: "c9"}, evals.Output{})
	if err != nil {
		t.Fatalf("Score() = %v", err)
	}
	if score.Scorer != "half" || score.CaseID != "c9" {
		t.Fatalf("identity = (%q, %q), want (half, c9)", score.Scorer, score.CaseID)
	}
}

func assertScore(t *testing.T, score evals.Score, wantPassed bool) {
	t.Helper()
	if score.Passed != wantPassed {
		t.Fatalf("Passed = %t, want %t (reason %q)", score.Passed, wantPassed, score.Reason)
	}
	wantValue := 0.0
	if wantPassed {
		wantValue = 1
	}
	if score.Value != wantValue {
		t.Fatalf("Value = %v, want %v", score.Value, wantValue)
	}
	if !wantPassed && score.Reason == "" {
		t.Fatal("failing score carries no reason")
	}
}
