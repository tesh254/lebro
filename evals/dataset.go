package evals

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"

	"github.com/tesh254/lebro"
)

// Stable identifiers keep evaluation records portable across result backends.
type (
	// DatasetID names a dataset independently of its contents. Two datasets
	// with the same ID and different cases are different versions of one
	// question.
	DatasetID string
	// CaseID identifies one case within a dataset. It must be unique in its
	// dataset because case results are correlated by it.
	CaseID string
	// ExperimentID identifies one run of a dataset against a target.
	ExperimentID string
)

// DatasetVersion is a content hash over a dataset's ordered, normalized cases.
// It is derived rather than supplied so that two experiment records claiming the
// same version cannot disagree about what was actually run.
type DatasetVersion string

// Case is one input to evaluate, with the expectation to score it against.
//
// A case carries both input shapes so one dataset can be run against either
// kind of target: Input is the raw JSON a JSON-step workflow consumes, and
// Messages is the conversation a message-centric agent consumes. At least one
// must be present. A Target reads the field it needs and returns
// ErrTargetUnsupportedCase when the case does not carry it.
//
// Expected is the reference the scorers compare against. Its meaning is the
// scorer's business: ExactMatch reads it as text, JSONEquals reads it as JSON,
// and ModelScorer passes it to the grader as context. A case with no Expected is
// valid — some scorers judge an output on its own terms.
type Case struct {
	ID       CaseID            `json:"id"`
	Input    json.RawMessage   `json:"input,omitempty"`
	Messages []lebro.Message   `json:"messages,omitempty"`
	Expected string            `json:"expected,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// Validate reports whether a case can be evaluated.
func (c Case) Validate() error {
	if c.ID == "" {
		return fmt.Errorf("%w: case requires an ID", ErrInvalidCase)
	}
	if len(c.Input) == 0 && len(c.Messages) == 0 {
		return fmt.Errorf("%w: case %q requires an input or messages", ErrInvalidCase, c.ID)
	}
	if len(c.Input) > 0 && !json.Valid(c.Input) {
		return fmt.Errorf("%w: case %q input must be valid JSON", ErrInvalidCase, c.ID)
	}
	for i, message := range c.Messages {
		if err := message.Validate(); err != nil {
			return fmt.Errorf("%w: case %q message %d: %v", ErrInvalidCase, c.ID, i, err)
		}
	}
	return nil
}

// Clone returns a deep copy of the case. Input and Messages are copied so a
// stored case cannot be mutated through the slice a caller still holds.
func (c Case) Clone() Case {
	c.Input = cloneRawJSON(c.Input)
	if c.Messages != nil {
		c.Messages = append([]lebro.Message(nil), c.Messages...)
	}
	c.Metadata = cloneMetadata(c.Metadata)
	return c
}

// Dataset is an ordered, named collection of cases.
//
// Order is part of the dataset's identity: case results are reported in dataset
// order, and reordering cases produces a new Version. Metadata is
// caller-defined and does not participate in the version, so annotating a
// dataset does not invalidate comparisons against earlier runs of the same
// cases.
type Dataset struct {
	ID       DatasetID         `json:"id"`
	Name     string            `json:"name,omitempty"`
	Cases    []Case            `json:"cases"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// Validate reports whether a dataset can be evaluated. It rejects a dataset
// with no ID, no cases, a duplicate case ID, or an invalid case.
func (d Dataset) Validate() error {
	if d.ID == "" {
		return fmt.Errorf("%w: dataset requires an ID", ErrInvalidDataset)
	}
	if len(d.Cases) == 0 {
		return fmt.Errorf("%w: dataset %q requires at least one case", ErrInvalidDataset, d.ID)
	}
	seen := make(map[CaseID]struct{}, len(d.Cases))
	for i, testCase := range d.Cases {
		if err := testCase.Validate(); err != nil {
			return fmt.Errorf("dataset %q case %d: %w", d.ID, i, err)
		}
		if _, exists := seen[testCase.ID]; exists {
			return fmt.Errorf("%w: dataset %q contains duplicate case ID %q", ErrInvalidDataset, d.ID, testCase.ID)
		}
		seen[testCase.ID] = struct{}{}
	}
	return nil
}

// Clone returns a deep copy of the dataset.
func (d Dataset) Clone() Dataset {
	if d.Cases != nil {
		cases := make([]Case, len(d.Cases))
		for i, testCase := range d.Cases {
			cases[i] = testCase.Clone()
		}
		d.Cases = cases
	}
	d.Metadata = cloneMetadata(d.Metadata)
	return d
}

// Version returns the content hash identifying this exact set of cases in this
// exact order.
//
// The hash covers the dataset ID and, for each case in order, its ID, canonical
// JSON input, messages, expected value, and metadata. JSON inputs are
// canonicalized first, so reformatting an input or reordering its object keys
// leaves the version unchanged while changing a value does not. Dataset Name and
// Metadata are excluded: they describe the dataset rather than the question it
// asks.
//
// Version is defined on an invalid dataset too — it hashes whatever is present —
// so callers must Validate separately rather than treating a version as proof of
// validity.
func (d Dataset) Version() DatasetVersion {
	hash := sha256.New()
	writeHashField(hash, "dataset", string(d.ID))
	for _, testCase := range d.Cases {
		writeHashField(hash, "case", string(testCase.ID))
		// Canonicalize so a whitespace-only edit to an input does not read as a
		// different dataset. Invalid JSON is hashed verbatim; Validate rejects it.
		if canonical, err := canonicalJSON(testCase.Input); err == nil {
			writeHashField(hash, "input", string(canonical))
		} else {
			writeHashField(hash, "input", string(testCase.Input))
		}
		writeHashField(hash, "messages", encodeMessages(testCase.Messages))
		writeHashField(hash, "expected", testCase.Expected)
		for _, key := range sortedKeys(testCase.Metadata) {
			writeHashField(hash, "meta:"+key, testCase.Metadata[key])
		}
	}
	return DatasetVersion(hex.EncodeToString(hash.Sum(nil)))
}

// writeHashField writes a length-prefixed field so that concatenation cannot
// produce the same digest from different field boundaries: ("ab", "c") and
// ("a", "bc") hash differently.
func writeHashField(hash interface{ Write([]byte) (int, error) }, name, value string) {
	_, _ = fmt.Fprintf(hash, "%d:%s=%d:%s;", len(name), name, len(value), value)
}

// encodeMessages renders messages as canonical JSON for hashing. Message has a
// fixed field order and ModelToolCalls is already canonically encoded, but a
// structured-output payload is opaque JSON, so it is canonicalized first —
// otherwise reordering its object keys would change the dataset's version and
// block comparison of two runs over equivalent cases.
func encodeMessages(messages []lebro.Message) string {
	if len(messages) == 0 {
		return ""
	}
	normalized := make([]lebro.Message, len(messages))
	for i, message := range messages {
		if structured := message.StructuredOutput; structured != "" {
			if canonical, err := canonicalJSON(structured.Raw()); err == nil {
				message.StructuredOutput = lebro.ModelStructuredOutput(canonical)
			}
		}
		normalized[i] = message
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		// Message marshalling only fails on invalid structured output, which
		// Validate rejects; fall back to a stable non-empty marker.
		return fmt.Sprintf("unencodable:%d", len(messages))
	}
	return string(encoded)
}

func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func cloneMetadata(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	return maps.Clone(m)
}

func cloneRawJSON(value json.RawMessage) json.RawMessage {
	if value == nil {
		return nil
	}
	return append(json.RawMessage(nil), value...)
}

// canonicalJSON normalizes JSON so semantically equivalent values produce
// identical bytes: whitespace is removed and object keys are sorted. Numbers are
// preserved verbatim via json.Number, and HTML-sensitive characters in strings
// are left unescaped so canonical bytes stay faithful to the source.
func canonicalJSON(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("value is empty")
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	// Reject trailing content so a value followed by garbage, or two top-level
	// values, is an error rather than a silent match on the first one.
	if decoder.More() {
		return nil, errors.New("value has more than one top-level JSON value")
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}
