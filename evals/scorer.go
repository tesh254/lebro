package evals

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Score is one scorer's judgment of one output.
//
// Value is the scorer's numeric result, conventionally in [0,1] for rule
// scorers, and Passed is its boolean reading of the same judgment. Both are
// recorded because aggregate reporting wants a mean and a pass rate, and
// deriving one from the other would force every scorer into the same threshold.
// Reason explains the judgment; rule scorers fill it on failure so a regression
// report says what differed.
type Score struct {
	Scorer   string            `json:"scorer"`
	CaseID   CaseID            `json:"case_id,omitempty"`
	Value    float64           `json:"value"`
	Passed   bool              `json:"passed"`
	Reason   string            `json:"reason,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// Clone returns a deep copy of the score.
func (s Score) Clone() Score {
	s.Metadata = cloneMetadata(s.Metadata)
	return s
}

// Scorer judges one output against its case. Implementations must be safe for
// concurrent use because an experiment scores cases in parallel, and must honor
// context cancellation.
//
// A returned error means the scorer could not measure the output — a grader model
// was unavailable, a pattern could not be applied. It is recorded as a
// ScorerFailure, separately from the target's outcome. A scorer that measured the
// output and found it wrong returns a Score with Passed false and a nil error;
// returning an error for a low-quality output would misreport a working
// measurement as a broken one.
type Scorer interface {
	Name() string
	Score(context.Context, Case, Output) (Score, error)
}

// ScorerFunc adapts an ordinary function into a Scorer.
type ScorerFunc struct {
	ScorerName string
	Fn         func(context.Context, Case, Output) (Score, error)
}

var _ Scorer = ScorerFunc{}

// Name returns the configured name.
func (f ScorerFunc) Name() string { return f.ScorerName }

// Score calls Fn and stamps the scorer's name and case ID on the result so a
// function-backed scorer produces the same record shape as a built-in one.
func (f ScorerFunc) Score(ctx context.Context, testCase Case, output Output) (Score, error) {
	if f.Fn == nil {
		return Score{}, fmt.Errorf("%w: scorer %q has no function", ErrInvalidScorer, f.ScorerName)
	}
	score, err := f.Fn(ctx, testCase, output)
	if err != nil {
		return Score{}, err
	}
	if score.Scorer == "" {
		score.Scorer = f.ScorerName
	}
	score.CaseID = testCase.ID
	return score, nil
}

// TextSelector chooses which part of an Output a text scorer reads.
type TextSelector string

const (
	// SelectText reads Output.Text, the target's answer as text. It is the
	// default.
	SelectText TextSelector = "text"
	// SelectStructured reads Output.Structured as a raw JSON string.
	SelectStructured TextSelector = "structured"
)

// selectText resolves the configured selector against an output.
func selectText(selector TextSelector, output Output) string {
	if selector == SelectStructured {
		return string(output.Structured)
	}
	return output.Text
}

func validSelector(selector TextSelector) bool {
	switch selector {
	case "", SelectText, SelectStructured:
		return true
	default:
		return false
	}
}

// normalizeText applies the comparison options shared by the text scorers.
func normalizeText(value string, ignoreCase, trimSpace bool) string {
	if trimSpace {
		value = strings.TrimSpace(value)
	}
	if ignoreCase {
		value = strings.ToLower(value)
	}
	return value
}

// ExactMatchConfig configures an ExactMatch scorer. A zero value compares
// Output.Text to Case.Expected verbatim under the name "exact_match".
type ExactMatchConfig struct {
	Name       string
	Selector   TextSelector
	IgnoreCase bool
	TrimSpace  bool
}

// ExactMatch scores 1 when the selected output text equals the case's Expected
// value and 0 otherwise.
type ExactMatch struct {
	name       string
	selector   TextSelector
	ignoreCase bool
	trimSpace  bool
}

// NewExactMatch returns an ExactMatch scorer. It reports ErrInvalidScorer for an
// unrecognized selector.
func NewExactMatch(config ExactMatchConfig) (*ExactMatch, error) {
	if !validSelector(config.Selector) {
		return nil, fmt.Errorf("%w: unknown selector %q", ErrInvalidScorer, config.Selector)
	}
	name := config.Name
	if name == "" {
		name = "exact_match"
	}
	return &ExactMatch{
		name:       name,
		selector:   config.Selector,
		ignoreCase: config.IgnoreCase,
		trimSpace:  config.TrimSpace,
	}, nil
}

var _ Scorer = (*ExactMatch)(nil)

// Name returns the scorer's name.
func (s *ExactMatch) Name() string { return s.name }

// Score compares the selected text to the case's Expected value.
func (s *ExactMatch) Score(ctx context.Context, testCase Case, output Output) (Score, error) {
	if err := ctx.Err(); err != nil {
		return Score{}, err
	}
	actual := normalizeText(selectText(s.selector, output), s.ignoreCase, s.trimSpace)
	expected := normalizeText(testCase.Expected, s.ignoreCase, s.trimSpace)
	score := Score{Scorer: s.name, CaseID: testCase.ID}
	if actual == expected {
		score.Value, score.Passed = 1, true
		return score, nil
	}
	score.Reason = fmt.Sprintf("expected %q, got %q", expected, actual)
	return score, nil
}

// ContainsConfig configures a Contains scorer. Substring defaults to the case's
// Expected value, which lets one scorer serve a dataset whose cases each expect
// different text.
type ContainsConfig struct {
	Name       string
	Substring  string
	Selector   TextSelector
	IgnoreCase bool
	Negate     bool
}

// Contains scores 1 when the selected output text contains the substring. With
// Negate set, it scores 1 when the substring is absent, which expresses a
// must-not-say expectation.
type Contains struct {
	name       string
	substring  string
	selector   TextSelector
	ignoreCase bool
	negate     bool
}

// NewContains returns a Contains scorer.
func NewContains(config ContainsConfig) (*Contains, error) {
	if !validSelector(config.Selector) {
		return nil, fmt.Errorf("%w: unknown selector %q", ErrInvalidScorer, config.Selector)
	}
	name := config.Name
	if name == "" {
		name = "contains"
	}
	return &Contains{
		name:       name,
		substring:  config.Substring,
		selector:   config.Selector,
		ignoreCase: config.IgnoreCase,
		negate:     config.Negate,
	}, nil
}

var _ Scorer = (*Contains)(nil)

// Name returns the scorer's name.
func (s *Contains) Name() string { return s.name }

// Score reports whether the selected text contains the configured substring, or
// the case's Expected value when no substring was configured. A case with
// neither is a scorer failure: there is nothing to look for, and scoring 1 for
// "contains the empty string" would silently pass every output.
func (s *Contains) Score(ctx context.Context, testCase Case, output Output) (Score, error) {
	if err := ctx.Err(); err != nil {
		return Score{}, err
	}
	needle := s.substring
	if needle == "" {
		needle = testCase.Expected
	}
	if needle == "" {
		return Score{}, fmt.Errorf("%w: scorer %q has no substring and case %q has no expected value",
			ErrInvalidScorer, s.name, testCase.ID)
	}
	actual := normalizeText(selectText(s.selector, output), s.ignoreCase, false)
	comparable := normalizeText(needle, s.ignoreCase, false)
	found := strings.Contains(actual, comparable)
	score := Score{Scorer: s.name, CaseID: testCase.ID}
	if found != s.negate {
		score.Value, score.Passed = 1, true
		return score, nil
	}
	if s.negate {
		score.Reason = fmt.Sprintf("expected output not to contain %q", needle)
	} else {
		score.Reason = fmt.Sprintf("expected output to contain %q", needle)
	}
	return score, nil
}

// RegexpConfig configures a Regexp scorer. Pattern is required and is compiled
// once at construction, so an invalid pattern is a configuration error rather
// than a per-case scorer failure.
type RegexpConfig struct {
	Name     string
	Pattern  string
	Selector TextSelector
	Negate   bool
}

// Regexp scores 1 when the selected output text matches the pattern. With Negate
// set, it scores 1 when the pattern does not match.
type Regexp struct {
	name     string
	pattern  *regexp.Regexp
	selector TextSelector
	negate   bool
}

// NewRegexp compiles the pattern and returns a Regexp scorer.
func NewRegexp(config RegexpConfig) (*Regexp, error) {
	if !validSelector(config.Selector) {
		return nil, fmt.Errorf("%w: unknown selector %q", ErrInvalidScorer, config.Selector)
	}
	if config.Pattern == "" {
		return nil, fmt.Errorf("%w: regexp scorer requires a pattern", ErrInvalidScorer)
	}
	compiled, err := regexp.Compile(config.Pattern)
	if err != nil {
		return nil, fmt.Errorf("%w: compile pattern: %v", ErrInvalidScorer, err)
	}
	name := config.Name
	if name == "" {
		name = "regexp"
	}
	return &Regexp{name: name, pattern: compiled, selector: config.Selector, negate: config.Negate}, nil
}

var _ Scorer = (*Regexp)(nil)

// Name returns the scorer's name.
func (s *Regexp) Name() string { return s.name }

// Score reports whether the selected text matches the pattern.
func (s *Regexp) Score(ctx context.Context, testCase Case, output Output) (Score, error) {
	if err := ctx.Err(); err != nil {
		return Score{}, err
	}
	matched := s.pattern.MatchString(selectText(s.selector, output))
	score := Score{Scorer: s.name, CaseID: testCase.ID}
	if matched != s.negate {
		score.Value, score.Passed = 1, true
		return score, nil
	}
	if s.negate {
		score.Reason = fmt.Sprintf("expected output not to match %q", s.pattern.String())
	} else {
		score.Reason = fmt.Sprintf("expected output to match %q", s.pattern.String())
	}
	return score, nil
}

// JSONEqualsConfig configures a JSONEquals scorer. A zero value compares
// Output.Structured against the case's Expected value parsed as JSON.
type JSONEqualsConfig struct {
	Name     string
	Selector TextSelector
}

// JSONEquals scores 1 when the selected output, parsed as JSON, is semantically
// equal to the case's Expected value parsed as JSON. Comparison is on
// canonicalized bytes, so key order and whitespace do not matter while values and
// types do.
type JSONEquals struct {
	name     string
	selector TextSelector
}

// NewJSONEquals returns a JSONEquals scorer. Its default selector is
// SelectStructured, because a JSON comparison against a target's plain text is
// almost never what a caller means.
func NewJSONEquals(config JSONEqualsConfig) (*JSONEquals, error) {
	if !validSelector(config.Selector) {
		return nil, fmt.Errorf("%w: unknown selector %q", ErrInvalidScorer, config.Selector)
	}
	selector := config.Selector
	if selector == "" {
		selector = SelectStructured
	}
	name := config.Name
	if name == "" {
		name = "json_equals"
	}
	return &JSONEquals{name: name, selector: selector}, nil
}

var _ Scorer = (*JSONEquals)(nil)

// Name returns the scorer's name.
func (s *JSONEquals) Name() string { return s.name }

// Score compares canonicalized JSON. Unparseable JSON on either side is a
// measured mismatch rather than a scorer failure: a target that emitted invalid
// JSON is a result worth recording, not a broken measurement.
func (s *JSONEquals) Score(ctx context.Context, testCase Case, output Output) (Score, error) {
	if err := ctx.Err(); err != nil {
		return Score{}, err
	}
	score := Score{Scorer: s.name, CaseID: testCase.ID}
	actualRaw := selectText(s.selector, output)
	actual, actualErr := canonicalJSON([]byte(actualRaw))
	if actualErr != nil {
		score.Reason = fmt.Sprintf("output is not valid JSON: %v", actualErr)
		return score, nil
	}
	expected, expectedErr := canonicalJSON([]byte(testCase.Expected))
	if expectedErr != nil {
		score.Reason = fmt.Sprintf("expected value is not valid JSON: %v", expectedErr)
		return score, nil
	}
	if bytes.Equal(actual, expected) {
		score.Value, score.Passed = 1, true
		return score, nil
	}
	score.Reason = fmt.Sprintf("expected %s, got %s", expected, actual)
	return score, nil
}

// NumericRangeConfig configures a NumericRange scorer. At least one bound must be
// set; a scorer with neither would pass every parseable number, which is a
// configuration mistake rather than a useful assertion.
type NumericRangeConfig struct {
	Name     string
	Selector TextSelector
	Min      *float64
	Max      *float64
}

// NumericRange parses the selected output text as a number and scores 1 when it
// falls within the configured inclusive bounds.
type NumericRange struct {
	name     string
	selector TextSelector
	min      *float64
	max      *float64
}

// NewNumericRange returns a NumericRange scorer. It reports ErrInvalidScorer when
// neither bound is set or when Min exceeds Max, because no output could satisfy
// an inverted range.
func NewNumericRange(config NumericRangeConfig) (*NumericRange, error) {
	if !validSelector(config.Selector) {
		return nil, fmt.Errorf("%w: unknown selector %q", ErrInvalidScorer, config.Selector)
	}
	if config.Min == nil && config.Max == nil {
		return nil, fmt.Errorf("%w: numeric range scorer requires a minimum or a maximum", ErrInvalidScorer)
	}
	if config.Min != nil && math.IsNaN(*config.Min) {
		return nil, fmt.Errorf("%w: numeric range minimum must be a number", ErrInvalidScorer)
	}
	if config.Max != nil && math.IsNaN(*config.Max) {
		return nil, fmt.Errorf("%w: numeric range maximum must be a number", ErrInvalidScorer)
	}
	if config.Min != nil && config.Max != nil && *config.Min > *config.Max {
		return nil, fmt.Errorf("%w: numeric range minimum %v exceeds maximum %v",
			ErrInvalidScorer, *config.Min, *config.Max)
	}
	name := config.Name
	if name == "" {
		name = "numeric_range"
	}
	scorer := &NumericRange{name: name, selector: config.Selector}
	// Copy the bounds so a caller mutating the pointed-to values after
	// construction cannot change how the scorer judges later cases.
	if config.Min != nil {
		min := *config.Min
		scorer.min = &min
	}
	if config.Max != nil {
		max := *config.Max
		scorer.max = &max
	}
	return scorer, nil
}

var _ Scorer = (*NumericRange)(nil)

// Name returns the scorer's name.
func (s *NumericRange) Name() string { return s.name }

// Score parses the selected text and checks it against the bounds. Text that is
// not a number is a measured failure, not a scorer failure.
func (s *NumericRange) Score(ctx context.Context, testCase Case, output Output) (Score, error) {
	if err := ctx.Err(); err != nil {
		return Score{}, err
	}
	score := Score{Scorer: s.name, CaseID: testCase.ID}
	raw := strings.TrimSpace(selectText(s.selector, output))
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		score.Reason = fmt.Sprintf("output %q is not a number", raw)
		return score, nil
	}
	// NaN compares false against every bound, so without this it would satisfy
	// any range. It is not a number in range; it is not a number at all.
	if math.IsNaN(value) {
		score.Reason = fmt.Sprintf("output %q is not a finite number", raw)
		return score, nil
	}
	if s.min != nil && value < *s.min {
		score.Reason = fmt.Sprintf("value %v is below minimum %v", value, *s.min)
		return score, nil
	}
	if s.max != nil && value > *s.max {
		score.Reason = fmt.Sprintf("value %v is above maximum %v", value, *s.max)
		return score, nil
	}
	score.Value, score.Passed = 1, true
	return score, nil
}
