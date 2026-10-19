package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func TestDecisionRequestValidate(t *testing.T) {
	valid := DecisionRequest{
		State: "A customer asked for a refund.",
		Questions: map[DecisionQuestionID]DecisionQuestion{
			"refund": NoulQuestion{
				Instructions: "Does this request qualify for a refund?",
				Criteria:     &NoulCriteria{True: "Qualifies", False: "Does not qualify"},
			},
		},
	}
	cases := []struct {
		name    string
		request DecisionRequest
		want    string
	}{
		{
			name:    "valid",
			request: valid,
		},
		{
			name: "missing state",
			request: DecisionRequest{
				Questions: valid.Questions,
			},
			want: "requires a state",
		},
		{
			name: "empty text state",
			request: DecisionRequest{
				State:     "   ",
				Questions: valid.Questions,
			},
			want: "state must not be empty",
		},
		{
			name: "empty object state",
			request: DecisionRequest{
				State:     map[string]any{},
				Questions: valid.Questions,
			},
			want: "state must not be empty",
		},
		{
			name: "empty array state",
			request: DecisionRequest{
				State:     []any{},
				Questions: valid.Questions,
			},
			want: "state must not be empty",
		},
		{
			name: "numeric state",
			request: DecisionRequest{
				State:     42,
				Questions: valid.Questions,
			},
			want: "state must be text, an object, or an array",
		},
		{
			name: "no questions",
			request: DecisionRequest{
				State: "state",
			},
			want: "requires at least one question",
		},
		{
			name: "empty question ID",
			request: DecisionRequest{
				State: "state",
				Questions: map[DecisionQuestionID]DecisionQuestion{
					"": NoulQuestion{Instructions: "Is it?"},
				},
			},
			want: "question ID must not be empty",
		},
		{
			name: "nil question",
			request: DecisionRequest{
				State: "state",
				Questions: map[DecisionQuestionID]DecisionQuestion{
					"q": nil,
				},
			},
			want: "must not be nil",
		},
		{
			name: "missing instructions",
			request: DecisionRequest{
				State: "state",
				Questions: map[DecisionQuestionID]DecisionQuestion{
					"q": NoulQuestion{},
				},
			},
			want: "requires instructions",
		},
		{
			name: "empty instructions",
			request: DecisionRequest{
				State: "state",
				Questions: map[DecisionQuestionID]DecisionQuestion{
					"q": NoulQuestion{Instructions: "  "},
				},
			},
			want: "requires non-empty instructions",
		},
		{
			name: "numeric instructions",
			request: DecisionRequest{
				State: "state",
				Questions: map[DecisionQuestionID]DecisionQuestion{
					"q": NoulQuestion{Instructions: 7},
				},
			},
			want: "instructions must be text, an object, or an array",
		},
		{
			name: "choice needs two options",
			request: DecisionRequest{
				State: "state",
				Questions: map[DecisionQuestionID]DecisionQuestion{
					"q": ChoiceQuestion{
						Instructions: "Which one?",
						Criteria:     map[DecisionOptionID]JSONValue{"only": "the only option"},
					},
				},
			},
			want: "requires at least two options",
		},
		{
			name: "choice needs a named option",
			request: DecisionRequest{
				State: "state",
				Questions: map[DecisionQuestionID]DecisionQuestion{
					"q": ChoiceQuestion{
						Instructions: "Which one?",
						Criteria:     map[DecisionOptionID]JSONValue{"": "unnamed", "other": "named"},
					},
				},
			},
			want: "has an empty option ID",
		},
		{
			name: "score needs two levels",
			request: DecisionRequest{
				State: "state",
				Questions: map[DecisionQuestionID]DecisionQuestion{
					"q": ScoreQuestion{
						Instructions: "How much?",
						Criteria:     []JSONValue{"only one level"},
					},
				},
			},
			want: "requires at least two levels",
		},
		{
			name: "noul criteria must carry content",
			request: DecisionRequest{
				State: "state",
				Questions: map[DecisionQuestionID]DecisionQuestion{
					"q": NoulQuestion{
						Instructions: "Is it?",
						Criteria:     &NoulCriteria{True: "  ", False: "no means no"},
					},
				},
			},
			want: "criteria true must not be empty",
		},
		{
			name: "noul criteria false must carry content",
			request: DecisionRequest{
				State: "state",
				Questions: map[DecisionQuestionID]DecisionQuestion{
					"q": NoulQuestion{
						Instructions: "Is it?",
						Criteria:     &NoulCriteria{True: "yes means yes", False: ""},
					},
				},
			},
			want: "criteria false must not be empty",
		},
		{
			name: "noul without criteria",
			request: DecisionRequest{
				State: "state",
				Questions: map[DecisionQuestionID]DecisionQuestion{
					"q": NoulQuestion{Instructions: "Is it?"},
				},
			},
		},
		{
			name: "noul with an empty criteria pair",
			request: DecisionRequest{
				State: "state",
				Questions: map[DecisionQuestionID]DecisionQuestion{
					"q": NoulQuestion{Instructions: "Is it?", Criteria: &NoulCriteria{}},
				},
			},
		},
		{
			name: "image needs a media type",
			request: DecisionRequest{
				State:     "state",
				Images:    []DecisionImage{{Bytes: []byte("data")}},
				Questions: valid.Questions,
			},
			want: "media type is required",
		},
		{
			name: "image needs a media type shape",
			request: DecisionRequest{
				State:     "state",
				Images:    []DecisionImage{{MediaType: "png", Bytes: []byte("data")}},
				Questions: valid.Questions,
			},
			want: "must be of the form type/subtype",
		},
		{
			name: "image needs bytes",
			request: DecisionRequest{
				State:     "state",
				Images:    []DecisionImage{{MediaType: "image/png"}},
				Questions: valid.Questions,
			},
			want: "image bytes must not be empty",
		},
		{
			name: "image must not be a URL",
			request: DecisionRequest{
				State:     "state",
				Images:    []DecisionImage{{MediaType: "image/png", Bytes: []byte("https://example.com/photo.png")}},
				Questions: valid.Questions,
			},
			want: "not a URL or a data URL",
		},
		{
			name: "image must not be a data URL",
			request: DecisionRequest{
				State:     "state",
				Images:    []DecisionImage{{MediaType: "image/png", Bytes: []byte("data:image/png;base64,iVBORw0KGgo=")}},
				Questions: valid.Questions,
			},
			want: "not a URL or a data URL",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.request.Validate()
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() error = nil, want %q", testCase.want)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Validate() error = %v, want it to mention %q", err, testCase.want)
			}
		})
	}
}

func TestDecisionRequestValidateRejectsNonJSONValues(t *testing.T) {
	cases := []struct {
		name  string
		state JSONValue
		want  string
	}{
		{
			name:  "channel",
			state: map[string]any{"c": make(chan int)},
			want:  "not JSON-compatible",
		},
		{
			name:  "function",
			state: map[string]any{"f": func() {}},
			want:  "not JSON-compatible",
		},
		{
			name:  "not a number",
			state: map[string]any{"n": math.NaN()},
			want:  "not finite",
		},
		{
			name:  "infinite number",
			state: map[string]any{"n": math.Inf(1)},
			want:  "not finite",
		},
		{
			name:  "non-string map key",
			state: map[any]any{1: "one"},
			want:  "must use string keys",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := DecisionRequest{
				State: testCase.state,
				Questions: map[DecisionQuestionID]DecisionQuestion{
					"q": NoulQuestion{Instructions: "Is it?"},
				},
			}
			err := request.Validate()
			if err == nil {
				t.Fatalf("Validate() error = nil, want %q", testCase.want)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Validate() error = %v, want it to mention %q", err, testCase.want)
			}
		})
	}
}

func TestDecisionRequestValidateRejectsCyclicState(t *testing.T) {
	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	request := DecisionRequest{
		State: cyclic,
		Questions: map[DecisionQuestionID]DecisionQuestion{
			"q": NoulQuestion{Instructions: "Is it?"},
		},
	}
	err := request.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want a cyclic state to be rejected")
	}
	if !strings.Contains(err.Error(), "nesting depth") {
		t.Fatalf("Validate() error = %v, want a nesting depth failure", err)
	}
}

func TestDecisionRequestValidateAcceptsDeepButBoundedState(t *testing.T) {
	deep := JSONValue("leaf")
	for i := 0; i < 20; i++ {
		deep = map[string]any{"nested": deep}
	}
	request := DecisionRequest{
		State: deep,
		Questions: map[DecisionQuestionID]DecisionQuestion{
			"q": NoulQuestion{Instructions: "Is it?"},
		},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

func TestDecisionRequestValidateAcceptsStructuredInstructions(t *testing.T) {
	request := DecisionRequest{
		State: "state",
		Questions: map[DecisionQuestionID]DecisionQuestion{
			"q": NoulQuestion{
				Instructions: map[string]any{
					"question":  "Is the resume for the same person as `candidate`?",
					"candidate": map[string]any{"name": "John Smith"},
				},
			},
		},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

func TestValidateDecisionImages(t *testing.T) {
	caps := DecisionImageCapabilities{
		Enabled:          true,
		MediaTypes:       []string{"image/png", "image/jpeg"},
		MaxImages:        2,
		MaxBytesPerImage: 16,
		MaxTotalBytes:    24,
	}
	image := DecisionImage{MediaType: "image/png", Bytes: []byte("0123456789")}
	cases := []struct {
		name   string
		caps   DecisionImageCapabilities
		images []DecisionImage
		want   error
	}{
		{
			name: "no images",
			caps: caps,
		},
		{
			name:   "allowed",
			caps:   caps,
			images: []DecisionImage{image},
		},
		{
			name:   "images disabled",
			caps:   DecisionImageCapabilities{MediaTypes: []string{"image/png"}},
			images: []DecisionImage{image},
			want:   ErrDecisionUnsupportedCapability,
		},
		{
			name:   "media type not allowed",
			caps:   caps,
			images: []DecisionImage{{MediaType: "image/gif", Bytes: []byte("data")}},
			want:   ErrDecisionUnsupportedCapability,
		},
		{
			name:   "too many images",
			caps:   caps,
			images: []DecisionImage{image, image, image},
			want:   ErrDecisionInvalidRequest,
		},
		{
			name:   "image too large",
			caps:   caps,
			images: []DecisionImage{{MediaType: "image/png", Bytes: []byte("01234567890123456789")}},
			want:   ErrDecisionInvalidRequest,
		},
		{
			name:   "total too large",
			caps:   caps,
			images: []DecisionImage{image, {MediaType: "image/jpeg", Bytes: []byte("0123456789012345")}},
			want:   ErrDecisionInvalidRequest,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := ValidateDecisionImages(testCase.caps, testCase.images)
			if testCase.want == nil {
				if err != nil {
					t.Fatalf("ValidateDecisionImages() error = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, testCase.want) {
				t.Fatalf("ValidateDecisionImages() error = %v, want %v", err, testCase.want)
			}
		})
	}
}

func TestDecisionResponseValidate(t *testing.T) {
	cases := []struct {
		name     string
		response DecisionResponse
		want     string
	}{
		{
			name: "valid",
			response: DecisionResponse{
				Answers: map[DecisionQuestionID]DecisionAnswer{
					"q": NoulAnswer{ProbabilityYes: 0.5},
				},
				Attempts: 1,
			},
		},
		{
			name: "probability out of range",
			response: DecisionResponse{
				Answers: map[DecisionQuestionID]DecisionAnswer{
					"q": NoulAnswer{ProbabilityYes: 1.5},
				},
				Attempts: 1,
			},
			want: "must be between 0 and 1",
		},
		{
			name: "not a number",
			response: DecisionResponse{
				Answers: map[DecisionQuestionID]DecisionAnswer{
					"q": NoulAnswer{ProbabilityYes: math.NaN()},
				},
				Attempts: 1,
			},
			want: "must be a finite number",
		},
		{
			name: "empty choice",
			response: DecisionResponse{
				Answers: map[DecisionQuestionID]DecisionAnswer{
					"q": ChoiceAnswer{Probabilities: map[DecisionOptionID]float64{"a": 1}},
				},
				Attempts: 1,
			},
			want: "requires a selected option",
		},
		{
			name: "choice probability out of range",
			response: DecisionResponse{
				Answers: map[DecisionQuestionID]DecisionAnswer{
					"q": ChoiceAnswer{Choice: "a", Probabilities: map[DecisionOptionID]float64{"a": 1.2}},
				},
			},
			want: "must be between 0 and 1",
		},
		{
			name: "choice option without an ID",
			response: DecisionResponse{
				Answers: map[DecisionQuestionID]DecisionAnswer{
					"q": ChoiceAnswer{Choice: "a", Probabilities: map[DecisionOptionID]float64{"a": 0.5, "": 0.5}},
				},
				Attempts: 1,
			},
			want: "empty option ID",
		},
		{
			name: "choice selects an option the distribution does not carry",
			response: DecisionResponse{
				Answers: map[DecisionQuestionID]DecisionAnswer{
					"q": ChoiceAnswer{Choice: "b", Probabilities: map[DecisionOptionID]float64{"a": 1}},
				},
				Attempts: 1,
			},
			want: "which the reported distribution does not carry",
		},
		{
			name: "valid choice",
			response: DecisionResponse{
				Answers: map[DecisionQuestionID]DecisionAnswer{
					"q": ChoiceAnswer{Choice: "a", Probabilities: map[DecisionOptionID]float64{"a": 0.7, "b": 0.3}},
				},
				Attempts: 1,
			},
		},
		{
			name: "valid score",
			response: DecisionResponse{
				Answers: map[DecisionQuestionID]DecisionAnswer{
					"q": ScoreAnswer{
						ExpectedLevel: float64Pointer(1.4),
						Probabilities: map[int]float64{0: 0.2, 1: 0.2, 2: 0.6},
						Provenance:    ScoreNative,
					},
				},
				Attempts: 1,
			},
		},
		{
			name: "score probability out of range",
			response: DecisionResponse{
				Answers: map[DecisionQuestionID]DecisionAnswer{
					"q": ScoreAnswer{Probabilities: map[int]float64{0: 1.5}, Provenance: ScoreNative},
				},
				Attempts: 1,
			},
			want: "must be between 0 and 1",
		},
		{
			name: "negative score level",
			response: DecisionResponse{
				Answers: map[DecisionQuestionID]DecisionAnswer{
					"q": ScoreAnswer{Probabilities: map[int]float64{-1: 1}, Provenance: ScoreNative},
				},
				Attempts: 1,
			},
			want: "level must not be negative",
		},
		{
			name: "unknown score provenance",
			response: DecisionResponse{
				Answers: map[DecisionQuestionID]DecisionAnswer{
					"q": ScoreAnswer{Provenance: ScoreProvenance("derived")},
				},
				Attempts: 1,
			},
			want: "not a known provenance",
		},
		{
			name: "missing score provenance",
			response: DecisionResponse{
				Answers: map[DecisionQuestionID]DecisionAnswer{
					"q": ScoreAnswer{},
				},
				Attempts: 1,
			},
			want: "not a known provenance",
		},
		{
			name: "score confidence out of range",
			response: DecisionResponse{
				Answers: map[DecisionQuestionID]DecisionAnswer{
					"q": ScoreAnswer{Confidence: float64Pointer(1.2), ConfidenceMetric: DecisionConfidenceUnknown, Provenance: ScoreNative},
				},
				Attempts: 1,
			},
			want: "must be between 0 and 1",
		},
		{
			name: "negative attempts",
			response: DecisionResponse{
				Attempts: -1,
			},
			want: "attempts must be at least one",
		},
		{
			name: "negative usage",
			response: DecisionResponse{
				Usage:    &DecisionUsage{InputTokens: int64Pointer(-1)},
				Attempts: 1,
			},
			want: "input tokens must not be negative",
		},
		{
			name: "unsupported answer",
			response: DecisionResponse{
				Answers:  map[DecisionQuestionID]DecisionAnswer{"q": unknownDecisionAnswer{}},
				Attempts: 1,
			},
			want: "unsupported decision answer type",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.response.Validate()
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() error = nil, want %q", testCase.want)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Validate() error = %v, want it to mention %q", err, testCase.want)
			}
		})
	}
}

// unknownDecisionAnswer is an answer type outside the closed set.
type unknownDecisionAnswer struct{}

func (unknownDecisionAnswer) isDecisionAnswer() {}

func TestDecisionErrorTaxonomy(t *testing.T) {
	cases := []struct {
		kind      DecisionErrorKind
		sentinel  error
		retryable bool
	}{
		{kind: DecisionErrorInvalidRequest, sentinel: ErrDecisionInvalidRequest},
		{kind: DecisionErrorUnsupportedCapability, sentinel: ErrDecisionUnsupportedCapability},
		{kind: DecisionErrorAuthentication, sentinel: ErrDecisionAuthentication},
		{kind: DecisionErrorPermissionDenied, sentinel: ErrDecisionPermissionDenied},
		{kind: DecisionErrorRateLimited, sentinel: ErrDecisionRateLimited, retryable: true},
		{kind: DecisionErrorUnavailable, sentinel: ErrDecisionUnavailable, retryable: true},
		{kind: DecisionErrorTimeout, sentinel: ErrDecisionTimeout, retryable: true},
		{kind: DecisionErrorTransport, sentinel: ErrDecisionTransport, retryable: true},
		{kind: DecisionErrorMalformedResponse, sentinel: ErrDecisionMalformedResponse},
		{kind: DecisionErrorCancelled, sentinel: ErrDecisionCancelled},
		{kind: DecisionErrorUnknown, sentinel: ErrDecisionUnknown},
	}
	for _, testCase := range cases {
		t.Run(string(testCase.kind), func(t *testing.T) {
			failure := &DecisionError{Kind: testCase.kind, Provider: "provider", Message: "detail"}
			if !errors.Is(failure, testCase.sentinel) {
				t.Fatalf("errors.Is() = false, want the %q sentinel", testCase.sentinel)
			}
			if failure.Retryable() != testCase.retryable {
				t.Fatalf("Retryable() = %v, want %v", failure.Retryable(), testCase.retryable)
			}
			if !strings.Contains(failure.Error(), "detail") {
				t.Fatalf("Error() = %q, want the detail", failure.Error())
			}
			if !strings.Contains(failure.Error(), "provider") {
				t.Fatalf("Error() = %q, want the provider", failure.Error())
			}
		})
	}
}

func TestDecisionErrorPreservesCause(t *testing.T) {
	cause := context.Canceled
	failure := &DecisionError{Kind: DecisionErrorCancelled, Err: cause}
	if !errors.Is(failure, context.Canceled) {
		t.Fatal("errors.Is() = false, want the context cause to be preserved")
	}
	if failure.Unwrap() != cause {
		t.Fatalf("Unwrap() = %v, want %v", failure.Unwrap(), cause)
	}
}

func TestDecisionGate(t *testing.T) {
	minimum := 0.6
	gate := DecisionGate{
		SelectProbability:   0.8,
		EscalateProbability: 0.2,
		MinimumConfidence:   &minimum,
		ConfidenceMetric:    DecisionConfidenceTopTwoMargin,
	}
	cases := []struct {
		name   string
		gate   DecisionGate
		answer NoulAnswer
		want   DecisionOutcome
	}{
		{
			name:   "confident yes",
			gate:   gate,
			answer: NoulAnswer{ProbabilityYes: 0.9, Confidence: float64Pointer(0.7), ConfidenceMetric: DecisionConfidenceTopTwoMargin},
			want:   DecisionOutcomeSelect,
		},
		{
			name:   "ambiguous",
			gate:   gate,
			answer: NoulAnswer{ProbabilityYes: 0.5, Confidence: float64Pointer(0.7), ConfidenceMetric: DecisionConfidenceTopTwoMargin},
			want:   DecisionOutcomeReview,
		},
		{
			name:   "confident no",
			gate:   gate,
			answer: NoulAnswer{ProbabilityYes: 0.1, Confidence: float64Pointer(0.7), ConfidenceMetric: DecisionConfidenceTopTwoMargin},
			want:   DecisionOutcomeEscalate,
		},
		{
			name:   "low confidence",
			gate:   gate,
			answer: NoulAnswer{ProbabilityYes: 0.9, Confidence: float64Pointer(0.2), ConfidenceMetric: DecisionConfidenceTopTwoMargin},
			want:   DecisionOutcomeReview,
		},
		{
			name:   "missing confidence",
			gate:   gate,
			answer: NoulAnswer{ProbabilityYes: 0.9},
			want:   DecisionOutcomeReview,
		},
		{
			name:   "no confidence requirement",
			gate:   DecisionGate{SelectProbability: 0.8, EscalateProbability: 0.2},
			answer: NoulAnswer{ProbabilityYes: 0.9},
			want:   DecisionOutcomeSelect,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.gate.Noul(testCase.answer); got != testCase.want {
				t.Fatalf("Noul() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestDecisionGateValidate(t *testing.T) {
	cases := []struct {
		name string
		gate DecisionGate
	}{
		{name: "select above one", gate: DecisionGate{SelectProbability: 1.2}},
		{name: "select below zero", gate: DecisionGate{SelectProbability: -0.1}},
		{name: "escalate above one", gate: DecisionGate{EscalateProbability: 1.2}},
		{name: "escalate below zero", gate: DecisionGate{EscalateProbability: -0.1}},
		{name: "inverted thresholds", gate: DecisionGate{SelectProbability: 0.2, EscalateProbability: 0.8}},
		{name: "confidence above one", gate: DecisionGate{MinimumConfidence: float64Pointer(1.5)}},
		{name: "confidence without a metric", gate: DecisionGate{MinimumConfidence: float64Pointer(0.5)}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if err := testCase.gate.Validate(); err == nil {
				t.Fatal("Validate() error = nil, want a failure")
			}
		})
	}
}

func float64Pointer(value float64) *float64 { return &value }

func int64Pointer(value int64) *int64 { return &value }

// TestDecisionGateFailsClosed pins that a gate which does not validate
// recommends a review instead of selecting on thresholds it cannot trust.
func TestDecisionGateFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		gate DecisionGate
	}{
		{name: "zero value", gate: DecisionGate{}},
		{name: "not a number", gate: DecisionGate{SelectProbability: math.NaN(), EscalateProbability: 0.1}},
		{name: "inverted", gate: DecisionGate{SelectProbability: 0.1, EscalateProbability: 0.9}},
		{name: "infinite confidence", gate: DecisionGate{SelectProbability: 0.9, EscalateProbability: 0.1, MinimumConfidence: float64Pointer(math.Inf(1))}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			answer := NoulAnswer{ProbabilityYes: 0.99, Confidence: float64Pointer(0.99), ConfidenceMetric: DecisionConfidenceTopTwoMargin}
			if got := testCase.gate.Noul(answer); got != DecisionOutcomeReview {
				t.Fatalf("Noul() = %q, want %q", got, DecisionOutcomeReview)
			}
		})
	}
}

// TestDecisionRequestValidateRejectsByteSlices pins that a byte slice is
// not a JSON value: encoding/json would turn it into a base64 string.
func TestDecisionRequestValidateRejectsByteSlices(t *testing.T) {
	request := DecisionRequest{
		State: []byte("state"),
		Questions: map[DecisionQuestionID]DecisionQuestion{
			"q": NoulQuestion{Instructions: "Is it?"},
		},
	}
	err := request.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want a byte slice to be rejected")
	}
	if !strings.Contains(err.Error(), "base64 string") {
		t.Fatalf("Validate() error = %v, want it to mention the base64 encoding", err)
	}
}

// TestDecisionRequestValidateAcceptsRawMessage pins that raw JSON is still
// accepted, because it is already a JSON value.
func TestDecisionRequestValidateAcceptsRawMessage(t *testing.T) {
	request := DecisionRequest{
		State: json.RawMessage(`{"order":"A-104"}`),
		Questions: map[DecisionQuestionID]DecisionQuestion{
			"q": NoulQuestion{Instructions: "Is it?"},
		},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

// TestDecisionRequestValidateChecksImageFormat pins that the bytes must
// match the media type they declare.
func TestDecisionRequestValidateChecksImageFormat(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe0}
	webp := []byte("RIFF\x00\x00\x00\x00WEBP")
	cases := []struct {
		name      string
		mediaType string
		bytes     []byte
		want      string
	}{
		{name: "png", mediaType: "image/png", bytes: png},
		{name: "jpeg", mediaType: "image/jpeg", bytes: jpeg},
		{name: "webp", mediaType: "image/webp", bytes: webp},
		{name: "png declared as jpeg", mediaType: "image/jpeg", bytes: png, want: "do not match the declared media type"},
		{name: "jpeg declared as png", mediaType: "image/png", bytes: jpeg, want: "do not match the declared media type"},
		{name: "not an image", mediaType: "image/png", bytes: []byte("just some text"), want: "do not match the declared media type"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := DecisionRequest{
				State:  "state",
				Images: []DecisionImage{{MediaType: testCase.mediaType, Bytes: testCase.bytes}},
				Questions: map[DecisionQuestionID]DecisionQuestion{
					"q": NoulQuestion{Instructions: "Is it?"},
				},
			}
			err := request.Validate()
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() error = nil, want %q", testCase.want)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Validate() error = %v, want it to mention %q", err, testCase.want)
			}
		})
	}
}

// TestDecisionRequestValidateAcceptsPointerQuestions pins that the pointer
// forms of the closed question set validate like the value forms.
func TestDecisionRequestValidateAcceptsPointerQuestions(t *testing.T) {
	noul := NoulQuestion{Instructions: "Is it?"}
	choice := ChoiceQuestion{Instructions: "Which one?", Criteria: map[DecisionOptionID]JSONValue{"a": "one", "b": "two"}}
	score := ScoreQuestion{Instructions: "How much?", Criteria: []JSONValue{"low", "high"}}
	request := DecisionRequest{
		State: "state",
		Questions: map[DecisionQuestionID]DecisionQuestion{
			"noul":   &noul,
			"choice": &choice,
			"score":  &score,
		},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

// TestDecisionResponseValidateAcceptsPointerAnswers pins the same for the
// answer set.
func TestDecisionResponseValidateAcceptsPointerAnswers(t *testing.T) {
	noul := NoulAnswer{ProbabilityYes: 0.5}
	response := DecisionResponse{
		Answers:  map[DecisionQuestionID]DecisionAnswer{"q": &noul},
		Attempts: 1,
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

// TestDecisionRefusalAnswer pins that a refusal is a valid answer and that
// it is distinct from every other answer kind. A provider declining to
// answer is not a low probability and not a failure to reach the provider.
func TestDecisionRefusalAnswer(t *testing.T) {
	response := DecisionResponse{
		Answers:  map[DecisionQuestionID]DecisionAnswer{"q": RefusalAnswer{Reason: "the state is too ambiguous"}},
		Attempts: 1,
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
	withoutReason := DecisionResponse{
		Answers:  map[DecisionQuestionID]DecisionAnswer{"q": RefusalAnswer{}},
		Attempts: 1,
	}
	if err := withoutReason.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

// TestDecisionGateFailsClosedOnARefusal pins that a gate never turns a
// refusal into a selection. The gate's thresholds describe a binary
// judgment, and a refusal is not one.
func TestDecisionGateFailsClosedOnARefusal(t *testing.T) {
	gate := DecisionGate{SelectProbability: 0.8, EscalateProbability: 0.2}
	if got := gate.Answer(RefusalAnswer{Reason: "the state is too ambiguous"}); got != DecisionOutcomeReview {
		t.Fatalf("Answer() = %q, want %q", got, DecisionOutcomeReview)
	}
	if got := gate.Answer(ChoiceAnswer{Choice: "a"}); got != DecisionOutcomeReview {
		t.Fatalf("Answer() = %q, want %q", got, DecisionOutcomeReview)
	}
	if got := gate.Answer(ScoreAnswer{}); got != DecisionOutcomeReview {
		t.Fatalf("Answer() = %q, want %q", got, DecisionOutcomeReview)
	}
	if got := gate.Answer(NoulAnswer{ProbabilityYes: 0.95}); got != DecisionOutcomeSelect {
		t.Fatalf("Answer() = %q, want %q", got, DecisionOutcomeSelect)
	}
}

// TestDecisionGateFailsClosedOnAnotherMetric pins that a confidence is only
// compared with a threshold written against the same measure.
func TestDecisionGateFailsClosedOnAnotherMetric(t *testing.T) {
	minimum := 0.5
	gate := DecisionGate{
		SelectProbability:   0.8,
		EscalateProbability: 0.2,
		MinimumConfidence:   &minimum,
		ConfidenceMetric:    DecisionConfidenceTopTwoMargin,
	}
	other := NoulAnswer{ProbabilityYes: 0.95, Confidence: float64Pointer(0.9), ConfidenceMetric: DecisionConfidenceTopProbabilityUniform}
	if got := gate.Noul(other); got != DecisionOutcomeReview {
		t.Fatalf("Noul() = %q, want %q", got, DecisionOutcomeReview)
	}
	same := NoulAnswer{ProbabilityYes: 0.95, Confidence: float64Pointer(0.9), ConfidenceMetric: DecisionConfidenceTopTwoMargin}
	if got := gate.Noul(same); got != DecisionOutcomeSelect {
		t.Fatalf("Noul() = %q, want %q", got, DecisionOutcomeSelect)
	}
}

// TestDecisionAnswerConfidenceNeedsAMetric pins that a confidence and the
// metric that says what it means travel together.
func TestDecisionAnswerConfidenceNeedsAMetric(t *testing.T) {
	withConfidence := ScoreAnswer{Confidence: float64Pointer(0.9), Provenance: ScoreNative}
	if err := validateDecisionAnswer(withConfidence); err == nil {
		t.Fatal("validateDecisionAnswer() error = nil, want a failure")
	}
	withMetric := ScoreAnswer{ConfidenceMetric: DecisionConfidenceUnknown, Provenance: ScoreNative}
	if err := validateDecisionAnswer(withMetric); err == nil {
		t.Fatal("validateDecisionAnswer() error = nil, want a failure")
	}
	complete := ScoreAnswer{Confidence: float64Pointer(0.9), ConfidenceMetric: DecisionConfidenceUnknown, Provenance: ScoreNative}
	if err := validateDecisionAnswer(complete); err != nil {
		t.Fatalf("validateDecisionAnswer() error = %v, want nil", err)
	}
}

// TestDecisionDerivedHelpers pins that the derived helpers report what they
// compute and never stand in for a provider's own number.
func TestDecisionDerivedHelpers(t *testing.T) {
	choice := ChoiceAnswer{
		Choice:        "a",
		Probabilities: map[DecisionOptionID]float64{"a": 0.75, "b": 0.25, "c": 0},
	}
	top, ok := DecisionTopProbability(choice.Probabilities)
	if !ok || top != 0.75 {
		t.Fatalf("DecisionTopProbability() = %v, %v, want 0.75, true", top, ok)
	}
	margin, ok := DecisionTopTwoMargin(choice.Probabilities)
	if !ok || margin != 0.5 {
		t.Fatalf("DecisionTopTwoMargin() = %v, %v, want 0.5, true", margin, ok)
	}
	if _, ok := DecisionTopTwoMargin(map[DecisionOptionID]float64{"a": 0.5}); ok {
		t.Fatal("DecisionTopTwoMargin() = true, want false for a single outcome")
	}
	if _, ok := DecisionTopProbability(map[DecisionOptionID]float64{}); ok {
		t.Fatal("DecisionTopProbability() = true, want false for an empty distribution")
	}

	score := ScoreAnswer{
		Probabilities: map[int]float64{0: 0.2, 1: 0.3, 2: 0.5},
		ExpectedLevel: float64Pointer(99),
		Provenance:    ScoreNative,
	}
	index, ok := score.ExpectedIndex()
	if !ok || index != 1.3 {
		t.Fatalf("ExpectedIndex() = %v, %v, want 1.3, true", index, ok)
	}
	if score.ExpectedLevel == nil || *score.ExpectedLevel != 99 {
		t.Fatal("ExpectedIndex() overwrote the provider's own score")
	}
	if _, ok := (ScoreAnswer{}).ExpectedIndex(); ok {
		t.Fatal("ExpectedIndex() = true, want false without a distribution")
	}
}

// TestDecisionCapabilitiesValidate pins that a kind that reports a
// confidence names the metric that confidence means.
func TestDecisionCapabilitiesValidate(t *testing.T) {
	cases := []struct {
		name         string
		capabilities DecisionCapabilities
		wantError    bool
	}{
		{
			name: "a confidence without a metric",
			capabilities: DecisionCapabilities{
				ChoiceUncertainty: DecisionUncertainty{Confidence: DecisionAvailabilityRequired},
			},
			wantError: true,
		},
		{
			name: "a metric without a confidence",
			capabilities: DecisionCapabilities{
				ChoiceUncertainty: DecisionUncertainty{ConfidenceMetric: DecisionConfidenceUnknown},
			},
			wantError: true,
		},
		{
			name: "an unknown availability",
			capabilities: DecisionCapabilities{
				ScoreUncertainty: DecisionUncertainty{Confidence: DecisionAvailabilityRequired, ConfidenceMetric: DecisionConfidenceUnknown, Distribution: DecisionAvailability("maybe")},
			},
			wantError: true,
		},
		{
			name: "a complete contract",
			capabilities: DecisionCapabilities{
				NoulUncertainty:   DecisionUncertainty{Confidence: DecisionAvailabilityUnavailable},
				ChoiceUncertainty: DecisionUncertainty{Confidence: DecisionAvailabilityRequired, ConfidenceMetric: DecisionConfidenceUnknown},
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.capabilities.Validate()
			if testCase.wantError && err == nil {
				t.Fatal("Validate() error = nil, want a failure")
			}
			if !testCase.wantError && err != nil {
				t.Fatalf("Validate() error = %v, want nil", err)
			}
		})
	}
}

// TestPredicateAliases pins that the neutral name and the provider name
// describe the same question and the same answer.
func TestPredicateAliases(t *testing.T) {
	var question DecisionQuestion = PredicateQuestion{Instructions: "Is it?"}
	noul, ok := question.(NoulQuestion)
	if !ok {
		t.Fatalf("the question = %#v, want a noul question", question)
	}
	if noul.Instructions != "Is it?" {
		t.Fatalf("the instructions = %#v, want %q", noul.Instructions, "Is it?")
	}
	var answer DecisionAnswer = PredicateAnswer{ProbabilityYes: 0.8, ConfidenceMetric: DecisionConfidenceUnknown}
	predicate, ok := answer.(NoulAnswer)
	if !ok {
		t.Fatalf("the answer = %#v, want a noul answer", answer)
	}
	if predicate.ProbabilityYes != 0.8 {
		t.Fatalf("the probability = %v, want 0.8", predicate.ProbabilityYes)
	}
}

// TestDecisionGateNeedsASelectThreshold pins that a gate cannot select
// everything. A select probability of zero selects every answer, so a gate
// that only sets a confidence threshold would select anything the provider
// was confident about.
func TestDecisionGateNeedsASelectThreshold(t *testing.T) {
	minimum := 0.5
	gate := DecisionGate{MinimumConfidence: &minimum, ConfidenceMetric: DecisionConfidenceTopTwoMargin}
	if err := gate.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want a failure")
	}
	answer := NoulAnswer{ProbabilityYes: 0.99, Confidence: float64Pointer(0.99), ConfidenceMetric: DecisionConfidenceTopTwoMargin}
	if got := gate.Noul(answer); got != DecisionOutcomeReview {
		t.Fatalf("Noul() = %q, want %q", got, DecisionOutcomeReview)
	}
}

// TestDecisionGateRejectsAnUndocumentedMetric pins that a threshold cannot
// be written against a measure nobody documented.
func TestDecisionGateRejectsAnUndocumentedMetric(t *testing.T) {
	minimum := 0.5
	gate := DecisionGate{
		SelectProbability: 0.8,
		MinimumConfidence: &minimum,
		ConfidenceMetric:  DecisionConfidenceUnknown,
	}
	if err := gate.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want a failure")
	}
	answer := NoulAnswer{ProbabilityYes: 0.99, Confidence: float64Pointer(0.99), ConfidenceMetric: DecisionConfidenceUnknown}
	if got := gate.Noul(answer); got != DecisionOutcomeReview {
		t.Fatalf("Noul() = %q, want %q", got, DecisionOutcomeReview)
	}
}

// TestDecisionDerivedHelpersIgnoreNonFiniteProbabilities pins that the
// derived helpers do not depend on the order a map is walked in.
func TestDecisionDerivedHelpersIgnoreNonFiniteProbabilities(t *testing.T) {
	distribution := map[DecisionOptionID]float64{"a": 0.6, "b": math.NaN(), "c": 0.4}
	top, ok := DecisionTopProbability(distribution)
	if !ok || top != 0.6 {
		t.Fatalf("DecisionTopProbability() = %v, %v, want 0.6, true", top, ok)
	}
	margin, ok := DecisionTopTwoMargin(distribution)
	if !ok || math.Abs(margin-0.2) > 1e-9 {
		t.Fatalf("DecisionTopTwoMargin() = %v, %v, want 0.2, true", margin, ok)
	}
	if _, ok := DecisionTopTwoMargin(map[DecisionOptionID]float64{"a": math.NaN(), "b": math.NaN()}); ok {
		t.Fatal("DecisionTopTwoMargin() = true, want false without a finite probability")
	}
}
