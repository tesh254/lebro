package evals_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/evals"
)

func TestDatasetValidateAcceptsBothInputShapes(t *testing.T) {
	dataset := evals.Dataset{
		ID: "qa",
		Cases: []evals.Case{
			{ID: "json", Input: json.RawMessage(`{"q":"one"}`), Expected: "1"},
			{ID: "messages", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "two"}}, Expected: "2"},
		},
	}
	if err := dataset.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestDatasetValidateRejectsInvalidShapes(t *testing.T) {
	tests := []struct {
		name    string
		dataset evals.Dataset
		want    error
	}{
		{
			name:    "no ID",
			dataset: evals.Dataset{Cases: []evals.Case{{ID: "a", Expected: "x", Input: json.RawMessage(`1`)}}},
			want:    evals.ErrInvalidDataset,
		},
		{
			name:    "no cases",
			dataset: evals.Dataset{ID: "qa"},
			want:    evals.ErrInvalidDataset,
		},
		{
			name: "duplicate case ID",
			dataset: evals.Dataset{ID: "qa", Cases: []evals.Case{
				{ID: "a", Input: json.RawMessage(`1`)},
				{ID: "a", Input: json.RawMessage(`2`)},
			}},
			want: evals.ErrInvalidDataset,
		},
		{
			name:    "case with no ID",
			dataset: evals.Dataset{ID: "qa", Cases: []evals.Case{{Input: json.RawMessage(`1`)}}},
			want:    evals.ErrInvalidCase,
		},
		{
			name:    "case with neither input nor messages",
			dataset: evals.Dataset{ID: "qa", Cases: []evals.Case{{ID: "a", Expected: "x"}}},
			want:    evals.ErrInvalidCase,
		},
		{
			name:    "case with invalid JSON input",
			dataset: evals.Dataset{ID: "qa", Cases: []evals.Case{{ID: "a", Input: json.RawMessage(`{`)}}},
			want:    evals.ErrInvalidCase,
		},
		{
			name: "case with invalid message",
			dataset: evals.Dataset{ID: "qa", Cases: []evals.Case{
				{ID: "a", Messages: []lebro.Message{{Role: "wizard", Content: "hi"}}},
			}},
			want: evals.ErrInvalidCase,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.dataset.Validate()
			if !errors.Is(err, test.want) {
				t.Fatalf("Validate() = %v, want %v", err, test.want)
			}
		})
	}
}

// TestDatasetVersionIsStableAcrossFormatting is the guarantee that makes
// comparison trustworthy: reformatting a case's JSON input must not read as a
// different dataset, or every reformat would look like a regression.
func TestDatasetVersionIsStableAcrossFormatting(t *testing.T) {
	compact := evals.Dataset{ID: "qa", Cases: []evals.Case{
		{ID: "a", Input: json.RawMessage(`{"b":2,"a":1}`), Expected: "x"},
	}}
	spaced := evals.Dataset{ID: "qa", Cases: []evals.Case{
		{ID: "a", Input: json.RawMessage("{\n  \"a\": 1,\n  \"b\": 2\n}"), Expected: "x"},
	}}
	if compact.Version() != spaced.Version() {
		t.Fatalf("version changed with formatting: %q vs %q", compact.Version(), spaced.Version())
	}
}

// TestDatasetVersionChangesWithContent covers each axis that must move the
// version. A version insensitive to any of these would let two runs claim to
// measure the same thing while measuring different things.
func TestDatasetVersionChangesWithContent(t *testing.T) {
	base := evals.Dataset{ID: "qa", Cases: []evals.Case{
		{ID: "a", Input: json.RawMessage(`{"q":1}`), Expected: "one", Metadata: map[string]string{"tag": "smoke"}},
		{ID: "b", Input: json.RawMessage(`{"q":2}`), Expected: "two"},
	}}
	baseVersion := base.Version()

	tests := []struct {
		name   string
		mutate func(evals.Dataset) evals.Dataset
	}{
		{
			name: "changed input value",
			mutate: func(d evals.Dataset) evals.Dataset {
				d.Cases[0].Input = json.RawMessage(`{"q":99}`)
				return d
			},
		},
		{
			name: "changed expected value",
			mutate: func(d evals.Dataset) evals.Dataset {
				d.Cases[0].Expected = "uno"
				return d
			},
		},
		{
			name: "changed case ID",
			mutate: func(d evals.Dataset) evals.Dataset {
				d.Cases[0].ID = "z"
				return d
			},
		},
		{
			name: "changed case metadata",
			mutate: func(d evals.Dataset) evals.Dataset {
				d.Cases[0].Metadata = map[string]string{"tag": "regression"}
				return d
			},
		},
		{
			name: "reordered cases",
			mutate: func(d evals.Dataset) evals.Dataset {
				d.Cases[0], d.Cases[1] = d.Cases[1], d.Cases[0]
				return d
			},
		},
		{
			name: "added case",
			mutate: func(d evals.Dataset) evals.Dataset {
				d.Cases = append(d.Cases, evals.Case{ID: "c", Input: json.RawMessage(`{"q":3}`)})
				return d
			},
		},
		{
			name: "removed case",
			mutate: func(d evals.Dataset) evals.Dataset {
				d.Cases = d.Cases[:1]
				return d
			},
		},
		{
			name: "changed dataset ID",
			mutate: func(d evals.Dataset) evals.Dataset {
				d.ID = "other"
				return d
			},
		},
		{
			name: "added messages",
			mutate: func(d evals.Dataset) evals.Dataset {
				d.Cases[0].Messages = []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}
				return d
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := test.mutate(base.Clone())
			if got := mutated.Version(); got == baseVersion {
				t.Fatalf("version %q unchanged after %s", got, test.name)
			}
		})
	}
}

// TestDatasetVersionIgnoresDescriptiveFields pins the deliberate exclusion:
// annotating a dataset must not invalidate comparisons against earlier runs of
// the same cases.
func TestDatasetVersionIgnoresDescriptiveFields(t *testing.T) {
	base := evals.Dataset{ID: "qa", Cases: []evals.Case{{ID: "a", Input: json.RawMessage(`1`)}}}
	annotated := base.Clone()
	annotated.Name = "Question answering"
	annotated.Metadata = map[string]string{"owner": "platform"}

	if base.Version() != annotated.Version() {
		t.Fatalf("version changed with descriptive fields: %q vs %q", base.Version(), annotated.Version())
	}
}

// TestDatasetVersionFieldBoundaries covers the length-prefix in the hash. Without
// it, writeHashField's "name=value;" records for case ID "ab" and input `"c"`
// concatenate to the same bytes as case ID "a" and input `b"c"` — both render as
// `case=ab;input="c";` if the lengths aren't encoded — so two datasets whose
// field values shift across that boundary must still produce different digests.
func TestDatasetVersionFieldBoundaries(t *testing.T) {
	left := evals.Dataset{ID: "qa", Cases: []evals.Case{
		{ID: "ab", Input: json.RawMessage(`"c"`)},
	}}
	right := evals.Dataset{ID: "qa", Cases: []evals.Case{
		{ID: "a", Input: json.RawMessage(`"bc"`)},
	}}
	if left.Version() == right.Version() {
		t.Fatal("datasets with shifted field boundaries share a version")
	}

	// A field value containing the field separator itself must not let the
	// decoder misread where one field ends and the next begins.
	withSeparator := evals.Dataset{ID: "qa", Cases: []evals.Case{
		{ID: "a", Expected: "x;input=0:;"},
	}}
	withoutSeparator := evals.Dataset{ID: "qa", Cases: []evals.Case{
		{ID: "a", Expected: "x"},
	}}
	if withSeparator.Version() == withoutSeparator.Version() {
		t.Fatal("a field value containing the separator collided with a shorter value")
	}
}

// TestDatasetVersionCanonicalizesStructuredOutput pins that a structured-output
// payload on a message is hashed by its canonical bytes: reordering its object
// keys must not change the dataset's version, or every equivalent re-encoding of
// a stored message would look like a different dataset.
func TestDatasetVersionCanonicalizesStructuredOutput(t *testing.T) {
	build := func(raw string) evals.Dataset {
		return evals.Dataset{ID: "qa", Cases: []evals.Case{{
			ID: "a",
			Messages: []lebro.Message{{
				Role:             lebro.RoleAssistant,
				Content:          "answer",
				StructuredOutput: lebro.NewModelStructuredOutput(json.RawMessage(raw)),
			}},
		}}}
	}
	ordered := build(`{"a":1,"b":2}`)
	reordered := build(`{"b":2,"a":1}`)
	if ordered.Version() != reordered.Version() {
		t.Fatalf("version changed with structured-output key order: %q vs %q", ordered.Version(), reordered.Version())
	}

	changed := build(`{"a":1,"b":99}`)
	if ordered.Version() == changed.Version() {
		t.Fatal("version unchanged after a structured-output value changed")
	}
}

func TestDatasetCloneIsolatesMutation(t *testing.T) {
	original := evals.Dataset{
		ID:       "qa",
		Metadata: map[string]string{"owner": "platform"},
		Cases: []evals.Case{{
			ID:       "a",
			Input:    json.RawMessage(`{"q":1}`),
			Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
			Metadata: map[string]string{"tag": "smoke"},
		}},
	}
	clone := original.Clone()
	before := original.Version()

	clone.Cases[0].Input[2] = 'X'
	clone.Cases[0].Metadata["tag"] = "changed"
	clone.Cases[0].Messages[0].Content = "changed"
	clone.Metadata["owner"] = "changed"

	if got := original.Version(); got != before {
		t.Fatal("mutating a clone changed the original dataset")
	}
	if original.Cases[0].Metadata["tag"] != "smoke" {
		t.Fatalf("case metadata leaked: %q", original.Cases[0].Metadata["tag"])
	}
	if original.Cases[0].Messages[0].Content != "hi" {
		t.Fatalf("case messages leaked: %q", original.Cases[0].Messages[0].Content)
	}
	if original.Metadata["owner"] != "platform" {
		t.Fatalf("dataset metadata leaked: %q", original.Metadata["owner"])
	}
}
