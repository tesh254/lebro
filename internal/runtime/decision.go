package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"
)

// JSONValue is a JSON-compatible value carried by a decision request. State,
// instructions, and criteria use it so callers pass ordinary Go data instead
// of provider wire JSON.
//
// Values are validated before they reach a provider: only JSON types, finite
// numbers, and bounded nesting are accepted. A value is never silently
// coerced, and a caller that needs a different shape must convert it
// explicitly.
type JSONValue any

// DecisionQuestionID identifies one question in a decision request. The
// identifier is caller-owned: it is never interpreted by a provider, and the
// matching answer is returned under the same identifier.
type DecisionQuestionID string

// DecisionOptionID identifies one option of a choice question. The identifier
// is caller-owned and stable, so a provider that renames or reorders options
// cannot change which option an application selected.
type DecisionOptionID string

// DecisionImage is one optional image input for a decision request.
//
// Bytes holds raw, already-authorized image data. A DecisionImage never
// carries a URL, a file path, or a data URL: an adapter that supports images
// encodes the bytes for its own wire format and rejects anything else.
type DecisionImage struct {
	// MediaType is the MIME type of Bytes, for example "image/png".
	MediaType string
	// Bytes is the raw image payload. An adapter copies the bytes it sends
	// and never retains the caller's slice.
	Bytes []byte
}

// DecisionRequest is one provider-neutral decision request. A request carries
// the state to evaluate, the questions to ask about it, and optional image
// inputs.
type DecisionRequest struct {
	// Model selects the provider model. It is optional only when the adapter
	// was configured with a default model.
	Model string
	// State is the text, object, or array the questions are evaluated
	// against. It must be non-empty.
	State JSONValue
	// Images are optional image inputs. An adapter rejects images unless its
	// advertised capabilities allow them.
	Images []DecisionImage
	// Questions are the named questions to ask. At least one is required, and
	// every identifier must be non-empty.
	Questions map[DecisionQuestionID]DecisionQuestion
}

// DecisionQuestion is one question in a decision request. The set of question
// kinds is closed: NoulQuestion, ChoiceQuestion, and ScoreQuestion.
type DecisionQuestion interface{ isDecisionQuestion() }

// NoulQuestion asks a yes/no question. The answer is the probability that the
// condition holds; it is not a boolean and never an action.
type NoulQuestion struct {
	// Instructions describes the question. It must be non-empty.
	Instructions JSONValue
	// Criteria optionally describes what a yes and a no mean. A nil value
	// omits the criteria entirely.
	Criteria *NoulCriteria
}

func (NoulQuestion) isDecisionQuestion() {}

// NoulCriteria describes the two outcomes of a noul question.
type NoulCriteria struct {
	// True describes what a yes (a probability near one) means.
	True JSONValue
	// False describes what a no (a probability near zero) means.
	False JSONValue
}

// ChoiceQuestion asks the provider to select one of a fixed set of options.
type ChoiceQuestion struct {
	// Instructions describes the question. It must be non-empty.
	Instructions JSONValue
	// Criteria maps each option to its description. At least two options are
	// required, and the identifiers are caller-owned and stable.
	Criteria map[DecisionOptionID]JSONValue
}

func (ChoiceQuestion) isDecisionQuestion() {}

// ScoreQuestion asks the provider to rate the state on an ordered scale.
type ScoreQuestion struct {
	// Instructions describes the question. It must be non-empty.
	Instructions JSONValue
	// Criteria holds the ordered level descriptions. Index zero is the lowest
	// level. At least two levels are required.
	Criteria []JSONValue
}

func (ScoreQuestion) isDecisionQuestion() {}

// DecisionConfidenceMetric names what a reported confidence number means.
//
// A confidence value can only be compared with another one when both use the
// same metric. Two providers report a number in the same range for the same
// question and mean different things by it, so a reported confidence always
// carries the metric it was computed with.
type DecisionConfidenceMetric string

const (
	// DecisionConfidenceUnknown marks a reported confidence whose meaning
	// the provider does not document. It is an explicit state rather than an
	// absence: the number is there and it cannot be compared with another.
	DecisionConfidenceUnknown DecisionConfidenceMetric = "unknown"

	// DecisionConfidenceTopProbabilityUniform marks a confidence measured as
	// how far the largest probability sits above an even split of the
	// outcomes. TypeSafe reports it for a choice answer.
	DecisionConfidenceTopProbabilityUniform DecisionConfidenceMetric = "top_probability_uniform"

	// DecisionConfidenceModalDistanceUniform marks a confidence measured as
	// the probability-weighted distance from the most likely level, against
	// the same distance for an even spread across the levels. TypeSafe
	// reports it for a score answer.
	DecisionConfidenceModalDistanceUniform DecisionConfidenceMetric = "modal_distance_uniform"

	// DecisionConfidenceTopTwoMargin marks a confidence measured as the gap
	// between the two highest probabilities. Fastino reports it for a choice
	// answer and for a score answer.
	DecisionConfidenceTopTwoMargin DecisionConfidenceMetric = "top_two_margin"

	// DecisionConfidenceDistanceFromHalf marks a confidence measured as the
	// absolute distance of a probability from one half. Fastino reports it
	// for a noul answer.
	DecisionConfidenceDistanceFromHalf DecisionConfidenceMetric = "distance_from_half"
)

// DecisionAnswer is one answer in a decision response. The set of answer kinds
// is closed: NoulAnswer, ChoiceAnswer, ScoreAnswer, and RefusalAnswer.
type DecisionAnswer interface{ isDecisionAnswer() }

// NoulAnswer is the answer to a noul question.
type NoulAnswer struct {
	// ProbabilityYes is the probability that the condition holds, from zero
	// to one. It is a probability, not an action and not a decision.
	ProbabilityYes float64
	// Confidence is the provider-reported certainty. It is nil when the
	// provider does not report confidence, and it is never derived from
	// ProbabilityYes.
	Confidence *float64
	// ConfidenceMetric names what Confidence means for this answer. It is
	// empty when the answer carries no confidence, and it is
	// DecisionConfidenceUnknown when the provider reports a confidence
	// without documenting what it means.
	ConfidenceMetric DecisionConfidenceMetric
}

func (NoulAnswer) isDecisionAnswer() {}

// PredicateQuestion is a neutral name for a binary judgment question.
//
// It is an alias for [NoulQuestion]: the two names describe the same
// question, and a predicate question is answered with the probability that a
// stated condition holds.
type PredicateQuestion = NoulQuestion

// PredicateAnswer is a neutral name for a binary judgment answer.
//
// It is an alias for [NoulAnswer]: the two names describe the same answer.
type PredicateAnswer = NoulAnswer

// ChoiceAnswer is the answer to a choice question.
type ChoiceAnswer struct {
	// Choice is the selected option.
	Choice DecisionOptionID
	// Probabilities maps every requested option to its probability. It is
	// empty when the provider reports no distribution.
	Probabilities map[DecisionOptionID]float64
	// Confidence is the provider-reported certainty. It is nil when the
	// provider does not report confidence.
	Confidence *float64
	// ConfidenceMetric names what Confidence means for this answer. It is
	// empty when the answer carries no confidence, and it is
	// DecisionConfidenceUnknown when the provider reports a confidence
	// without documenting what it means.
	ConfidenceMetric DecisionConfidenceMetric
}

func (ChoiceAnswer) isDecisionAnswer() {}

// ScoreProvenance records how a score answer was produced.
type ScoreProvenance string

const (
	// ScoreNative marks a value the provider reported directly.
	ScoreNative ScoreProvenance = "native"
	// ScoreNormalized marks a value derived from a documented distribution
	// instead of received from the provider, whether an adapter derived it
	// or a caller computed it from the answer's own distribution.
	ScoreNormalized ScoreProvenance = "normalized"
)

// ScoreAnswer is the answer to a score question.
//
// A selected level and a weighted level are different things and are kept
// apart. SelectedLevel is the level the provider itself picked, and stays nil
// when the provider reports no winner. ExpectedLevel is the
// probability-weighted value, and stays nil when the provider reports none.
type ScoreAnswer struct {
	// SelectedLevel is the winning level index, when the provider reports
	// one.
	SelectedLevel *int
	// ExpectedLevel is the probability-weighted level, when the provider
	// reports one.
	ExpectedLevel *float64
	// Probabilities maps each requested level index to its probability.
	Probabilities map[int]float64
	// Legend maps each requested level index to its description, when the
	// provider reports one.
	Legend map[int]JSONValue
	// Confidence is the provider-reported certainty. It is nil when the
	// provider does not report confidence.
	Confidence *float64
	// ConfidenceMetric names what Confidence means for this answer. It is
	// empty when the answer carries no confidence, and it is
	// DecisionConfidenceUnknown when the provider reports a confidence
	// without documenting what it means.
	ConfidenceMetric DecisionConfidenceMetric
	// Provenance records whether the values were reported by the provider or
	// derived by the adapter.
	Provenance ScoreProvenance
}

func (ScoreAnswer) isDecisionAnswer() {}

// RefusalAnswer is the answer to a question the provider declined to answer.
//
// A refusal is a judgment the provider made about its own ability to answer
// the question. It is distinct from a false answer, from a low probability,
// from a missing answer, and from a failure to reach the provider: none of
// those say that the provider tried and declined.
//
// A refusal is not an authorization and not a decision. An application that
// treats a refusal as a rejection has made that choice, and the contract
// does not make it for them.
type RefusalAnswer struct {
	// Reason is the provider's explanation, when it gave one. It is
	// optional: a refusal need not carry a reason.
	Reason string
}

func (RefusalAnswer) isDecisionAnswer() {}

// ExpectedIndex returns the probability-weighted level index of the answer's
// distribution.
//
// The value is derived from the reported distribution, not reported by the
// provider: a provider's own ExpectedLevel stays exactly as the provider
// reported it. It reports false when the answer carries no distribution.
//
// The returned index is a derived quantity. A caller that stores it
// alongside the answer records it with [ScoreNormalized], the provenance of
// a value derived from a documented distribution rather than received from
// the provider.
func (a ScoreAnswer) ExpectedIndex() (float64, bool) {
	if len(a.Probabilities) == 0 {
		return 0, false
	}
	var total, weighted float64
	for level, probability := range a.Probabilities {
		total += probability
		weighted += float64(level) * probability
	}
	if total == 0 {
		return 0, false
	}
	return weighted / total, true
}

// DecisionTopProbability returns the largest probability in a distribution
// and whether the distribution carries one.
//
// The value is derived from the reported distribution, not reported by the
// provider. A caller that needs the provider's own number uses the answer it
// came with.
func DecisionTopProbability[T comparable](distribution map[T]float64) (float64, bool) {
	top := 0.0
	found := false
	for _, probability := range distribution {
		if !found || probability > top {
			top = probability
			found = true
		}
	}
	return top, found
}

// DecisionTopTwoMargin returns the difference between the two largest
// probabilities in a distribution, and whether the distribution carries two.
//
// The value is derived from the reported distribution, not reported by the
// provider. It is the measure one provider uses for its own confidence, and
// it is not the measure another provider uses.
func DecisionTopTwoMargin[T comparable](distribution map[T]float64) (float64, bool) {
	top := 0.0
	second := 0.0
	count := 0
	for _, probability := range distribution {
		count++
		switch {
		case probability > top:
			second = top
			top = probability
		case probability > second:
			second = probability
		}
	}
	if count < 2 {
		return 0, false
	}
	return top - second, true
}

// DecisionUsage is the token usage a provider reported for a decision
// request. A nil field means the provider did not report that count.
type DecisionUsage struct {
	// InputTokens is the number of input tokens the provider reported.
	InputTokens *int64
	// OutputTokens is the number of output tokens the provider reported.
	OutputTokens *int64
}

// DecisionResponse is one provider-neutral decision response.
type DecisionResponse struct {
	// Provider identifies the adapter that produced the response.
	Provider ProviderID
	// UpstreamProvider identifies the provider that actually served the
	// request, when a gateway reports it. It is empty when the adapter
	// reached the provider directly, and it is never the same fact as
	// Provider: a gateway fronts a provider without being it.
	UpstreamProvider ProviderID
	// RequestedModel is the model the request asked for. It is the
	// adapter's own default model when the request named none.
	RequestedModel string
	// Model is the resolved model identity the provider reported. It can
	// differ from RequestedModel: an alias resolves to a dated model, and
	// a provider may report its own short name.
	Model string
	// Answers holds one answer per requested question, keyed by the
	// caller-owned question identifier.
	Answers map[DecisionQuestionID]DecisionAnswer
	// Usage is the reported token usage, or nil when the provider reported
	// none.
	Usage *DecisionUsage
	// RequestID is the provider request identifier, when the provider
	// reports one.
	RequestID string
	// Duration is the wall-clock time the adapter spent on the request,
	// including retries.
	Duration time.Duration
	// Attempts is the number of provider attempts that were made. It is at
	// least one for a successful response.
	Attempts int
}

// DecisionImageCapabilities describes the image inputs a provider accepts.
type DecisionImageCapabilities struct {
	// Enabled reports whether the provider accepts images at all.
	Enabled bool
	// MediaTypes lists the accepted MIME types, for example "image/png".
	MediaTypes []string
	// MaxImages is the largest number of images one request may carry. Zero
	// means unspecified.
	MaxImages int
	// MaxBytesPerImage is the largest decoded size of one image. Zero means
	// unspecified.
	MaxBytesPerImage int64
	// MaxTotalBytes is the largest decoded size of all images together. Zero
	// means unspecified.
	MaxTotalBytes int64
}

// DecisionAvailability says whether a provider reports a value for a
// question kind, and whether its absence is a malformed response.
type DecisionAvailability string

const (
	// DecisionAvailabilityUnavailable marks a value the provider does not
	// report for the kind. A response never carries one.
	DecisionAvailabilityUnavailable DecisionAvailability = "unavailable"

	// DecisionAvailabilityOptional marks a value the provider may report. A
	// response without it is valid, and the value stays absent.
	DecisionAvailabilityOptional DecisionAvailability = "optional"

	// DecisionAvailabilityRequired marks a value the provider reports. A
	// response without it is a malformed response.
	DecisionAvailabilityRequired DecisionAvailability = "required"
)

// DecisionUncertainty describes the uncertainty a provider reports for one
// question kind.
//
// Uncertainty is a property of the question kind, not of the provider. One
// provider can report a confidence for a choice answer and none for a noul
// answer, and two providers can report the same kind with different
// metrics, so the uncertainty belongs with the kind.
type DecisionUncertainty struct {
	// Distribution says whether the provider reports a probability
	// distribution for this kind.
	Distribution DecisionAvailability
	// Confidence says whether the provider reports a confidence for this
	// kind.
	Confidence DecisionAvailability
	// ConfidenceMetric names what a reported confidence means for this kind.
	// It is empty when the provider reports no confidence for the kind.
	ConfidenceMetric DecisionConfidenceMetric
	// Refusal reports whether the provider may refuse this kind.
	Refusal bool
}

// Validate checks that the uncertainty describes a usable contract.
//
// An unset availability means the provider reports nothing for the kind: the
// restrictive reading, so a configuration that leaves it out never claims a
// value it has not verified.
func (u DecisionUncertainty) Validate(kind string) error {
	switch u.Confidence {
	case "", DecisionAvailabilityUnavailable:
		if u.ConfidenceMetric != "" {
			return fmt.Errorf("lebro: decision %s uncertainty reports no confidence, so it must not name a confidence metric", kind)
		}
	case DecisionAvailabilityOptional, DecisionAvailabilityRequired:
		if u.ConfidenceMetric == "" {
			return fmt.Errorf("lebro: decision %s uncertainty reports a confidence, so it must name a confidence metric", kind)
		}
	default:
		return fmt.Errorf("lebro: decision %s confidence availability %q is not a known availability", kind, u.Confidence)
	}
	switch u.Distribution {
	case "", DecisionAvailabilityUnavailable, DecisionAvailabilityOptional, DecisionAvailabilityRequired:
	default:
		return fmt.Errorf("lebro: decision %s distribution availability %q is not a known availability", kind, u.Distribution)
	}
	return nil
}

// DecisionCapabilities describes what a decision provider supports. Callers
// use it to pick a provider and to validate a request before it is sent.
//
// A capability belongs to one route and one model. Two providers report
// confidence with different meanings, and one provider may report a
// confidence for a choice answer and none for a noul answer, so the
// capabilities describe a kind rather than an adapter.
type DecisionCapabilities struct {
	// Noul reports whether binary questions are supported.
	Noul bool
	// NoulUncertainty describes the uncertainty a noul answer carries.
	NoulUncertainty DecisionUncertainty
	// Choice reports whether choice questions are supported.
	Choice bool
	// ChoiceUncertainty describes the uncertainty a choice answer carries.
	ChoiceUncertainty DecisionUncertainty
	// Score reports whether score questions are supported.
	Score bool
	// ScoreUncertainty describes the uncertainty a score answer carries.
	ScoreUncertainty DecisionUncertainty
	// MultipleQuestions reports whether one request may carry more than one
	// question.
	MultipleQuestions bool
	// StructuredState reports whether the state may be an object or an array
	// rather than text only.
	StructuredState bool
	// StructuredInstructions reports whether instructions and criteria may be
	// structured rather than text only.
	StructuredInstructions bool
	// Images describes the accepted image inputs.
	Images DecisionImageCapabilities
	// MaxQuestions is the largest number of questions one request may carry.
	// Zero means unspecified.
	MaxQuestions int
	// MaxChoiceOptions is the largest number of options one choice question
	// may carry. Zero means unspecified.
	MaxChoiceOptions int
	// MaxScoreLevels is the largest number of levels one score question may
	// carry. Zero means unspecified.
	MaxScoreLevels int
	// Batching reports whether the provider accepts a batch of independent
	// requests in one call.
	Batching bool
	// MaxStateBytes is the largest accepted state size in bytes. Zero means
	// unspecified. An adapter checks it before it sends a request.
	MaxStateBytes int64
	// RetryableStatuses lists the HTTP status codes the provider documents as
	// retryable.
	RetryableStatuses []int
}

// Validate checks that the capabilities describe a usable contract.
//
// A kind that reports a confidence must name the metric that confidence
// means, and a kind that reports no confidence must not name one.
func (c DecisionCapabilities) Validate() error {
	for _, kind := range []struct {
		name        string
		uncertainty DecisionUncertainty
	}{
		{"noul", c.NoulUncertainty},
		{"choice", c.ChoiceUncertainty},
		{"score", c.ScoreUncertainty},
	} {
		if err := kind.uncertainty.Validate(kind.name); err != nil {
			return err
		}
	}
	return nil
}

// DecisionModel is implemented by decision adapters. It is a separate
// contract from Model: a decision provider returns typed judgments, not
// generated text.
//
// Implementations must honor context cancellation, validate provider
// responses, and map failures to DecisionError. They never cross to another
// provider on their own.
type DecisionModel interface {
	// ProviderID identifies the adapter.
	ProviderID() ProviderID
	// Capabilities describes what the adapter supports for its default
	// model.
	Capabilities() DecisionCapabilities
	// CapabilitiesFor describes what the adapter supports for a named
	// model. It reports false for a model the adapter has no verified
	// profile for, and a request that names such a model is rejected
	// rather than answered under another model's contract.
	CapabilitiesFor(model string) (DecisionCapabilities, bool)
	// Decide evaluates the request and returns the typed answers.
	Decide(context.Context, DecisionRequest) (DecisionResponse, error)
}

// DecisionOutcome is a deterministic recommendation derived from a decision
// answer and caller-owned thresholds. It is never an authorization: the
// application owns the policy that turns a recommendation into an action.
type DecisionOutcome string

const (
	// DecisionOutcomeSelect marks an answer that clearly satisfies the
	// caller's thresholds.
	DecisionOutcomeSelect DecisionOutcome = "select"
	// DecisionOutcomeReview marks an answer that is ambiguous, low
	// confidence, or missing a confidence the caller requires.
	DecisionOutcomeReview DecisionOutcome = "review"
	// DecisionOutcomeEscalate marks an answer that clearly fails the
	// caller's thresholds.
	DecisionOutcomeEscalate DecisionOutcome = "escalate"
)

// DecisionGate applies caller-owned thresholds to a decision answer.
//
// The gate is deterministic and has no side effect: it turns a probability
// into a recommendation and never authorizes a tool, a spend, or any other
// action. An application that wants a different effect owns it.
type DecisionGate struct {
	// SelectProbability is the probability of true at or above which a noul
	// answer selects.
	SelectProbability float64
	// EscalateProbability is the probability of true at or below which a noul
	// answer escalates. Answers between the two thresholds are a review.
	EscalateProbability float64
	// MinimumConfidence is the smallest provider-reported confidence that may
	// select. A nil value ignores confidence. An answer without a reported
	// confidence never selects.
	MinimumConfidence *float64
	// ConfidenceMetric names the metric MinimumConfidence is expressed in.
	//
	// A gate with a confidence threshold must name the metric: two
	// providers report a confidence in the same range and mean different
	// things by it, so a number is only comparable with a threshold that
	// was written against the same measure. An answer whose metric does
	// not match never selects.
	ConfidenceMetric DecisionConfidenceMetric
}

// Validate checks that the thresholds are usable.
func (g DecisionGate) Validate() error {
	if math.IsNaN(g.SelectProbability) || math.IsInf(g.SelectProbability, 0) {
		return fmt.Errorf("lebro: decision gate select probability must be a finite number, got %v", g.SelectProbability)
	}
	if g.SelectProbability < 0 || g.SelectProbability > 1 {
		return fmt.Errorf("lebro: decision gate select probability must be between 0 and 1, got %v", g.SelectProbability)
	}
	if math.IsNaN(g.EscalateProbability) || math.IsInf(g.EscalateProbability, 0) {
		return fmt.Errorf("lebro: decision gate escalate probability must be a finite number, got %v", g.EscalateProbability)
	}
	if g.EscalateProbability < 0 || g.EscalateProbability > 1 {
		return fmt.Errorf("lebro: decision gate escalate probability must be between 0 and 1, got %v", g.EscalateProbability)
	}
	if g.EscalateProbability > g.SelectProbability {
		return errors.New("lebro: decision gate escalate probability must not exceed the select probability")
	}
	if g.MinimumConfidence != nil {
		if math.IsNaN(*g.MinimumConfidence) || math.IsInf(*g.MinimumConfidence, 0) {
			return fmt.Errorf("lebro: decision gate minimum confidence must be a finite number, got %v", *g.MinimumConfidence)
		}
		if *g.MinimumConfidence < 0 || *g.MinimumConfidence > 1 {
			return fmt.Errorf("lebro: decision gate minimum confidence must be between 0 and 1, got %v", *g.MinimumConfidence)
		}
	}
	if g.MinimumConfidence != nil && g.ConfidenceMetric == "" {
		// A confidence threshold without the metric it is expressed in
		// would be compared against a number that may mean something else
		// entirely.
		return errors.New("lebro: decision gate confidence threshold requires a confidence metric")
	}
	if g.SelectProbability == 0 && g.EscalateProbability == 0 && g.MinimumConfidence == nil {
		// A gate with no thresholds would select every answer. An
		// unconfigured gate is a misconfiguration, and it fails closed.
		return errors.New("lebro: decision gate has no thresholds set")
	}
	return nil
}

// Noul returns the recommendation for a noul answer.
//
// A gate that does not validate fails closed: it recommends a review instead
// of selecting on thresholds it cannot trust.
func (g DecisionGate) Noul(answer NoulAnswer) DecisionOutcome {
	if err := g.Validate(); err != nil {
		return DecisionOutcomeReview
	}
	// A malformed answer is a review, never a selection.
	if err := validateNoulAnswer(answer); err != nil {
		return DecisionOutcomeReview
	}
	if g.MinimumConfidence != nil {
		if answer.Confidence == nil {
			return DecisionOutcomeReview
		}
		if answer.ConfidenceMetric != g.ConfidenceMetric {
			// The reported number and the threshold do not mean the same
			// thing, so comparing them would answer a question the caller
			// did not ask.
			return DecisionOutcomeReview
		}
		if *answer.Confidence < *g.MinimumConfidence {
			return DecisionOutcomeReview
		}
	}
	switch {
	case answer.ProbabilityYes >= g.SelectProbability:
		return DecisionOutcomeSelect
	case answer.ProbabilityYes <= g.EscalateProbability:
		return DecisionOutcomeEscalate
	default:
		return DecisionOutcomeReview
	}
}

// Answer returns the recommendation for an answer of any kind.
//
// The gate's thresholds describe a binary judgment. A refusal, a choice
// answer, a score answer, and an answer of an unknown kind are all a
// review: none of them satisfies a threshold the gate cannot read, and a
// refusal is a decision the provider made about its own ability to answer
// rather than a judgment about the state.
func (g DecisionGate) Answer(answer DecisionAnswer) DecisionOutcome {
	switch typed := answer.(type) {
	case NoulAnswer:
		return g.Noul(typed)
	case *NoulAnswer:
		if typed == nil {
			return DecisionOutcomeReview
		}
		return g.Noul(*typed)
	default:
		return DecisionOutcomeReview
	}
}

// DecisionErrorKind is the normalized category of a decision failure.
type DecisionErrorKind string

const (
	// DecisionErrorInvalidRequest marks a request the adapter rejected
	// before it reached the provider.
	DecisionErrorInvalidRequest DecisionErrorKind = "invalid_request"
	// DecisionErrorUnsupportedCapability marks a request that asks for
	// something the provider does not support, such as an image input at a
	// provider without image support.
	DecisionErrorUnsupportedCapability DecisionErrorKind = "unsupported_capability"
	// DecisionErrorAuthentication marks a rejected or missing credential.
	DecisionErrorAuthentication DecisionErrorKind = "authentication"
	// DecisionErrorPermissionDenied marks a credential that may not use the
	// requested model, including exhausted credits.
	DecisionErrorPermissionDenied DecisionErrorKind = "permission_denied"
	// DecisionErrorRateLimited marks a request refused for rate or capacity
	// reasons.
	DecisionErrorRateLimited DecisionErrorKind = "rate_limited"
	// DecisionErrorUnavailable marks a provider that is temporarily unable
	// to serve the request.
	DecisionErrorUnavailable DecisionErrorKind = "unavailable"
	// DecisionErrorTimeout marks a request that exceeded its deadline.
	DecisionErrorTimeout DecisionErrorKind = "timeout"
	// DecisionErrorCancelled marks a request the caller cancelled.
	DecisionErrorCancelled DecisionErrorKind = "cancelled"
	// DecisionErrorTransport marks a failure that produced no HTTP response.
	DecisionErrorTransport DecisionErrorKind = "transport"
	// DecisionErrorMalformedResponse marks a response the adapter could not
	// interpret.
	DecisionErrorMalformedResponse DecisionErrorKind = "malformed_response"
	// DecisionErrorUnknown marks a failure with no better classification.
	DecisionErrorUnknown DecisionErrorKind = "unknown"
)

var (
	// ErrDecisionInvalidRequest marks a locally rejected request.
	ErrDecisionInvalidRequest = errors.New("lebro: decision invalid request")
	// ErrDecisionUnsupportedCapability marks an unsupported request.
	ErrDecisionUnsupportedCapability = errors.New("lebro: decision unsupported capability")
	// ErrDecisionAuthentication marks an authentication failure.
	ErrDecisionAuthentication = errors.New("lebro: decision authentication failed")
	// ErrDecisionPermissionDenied marks a permission failure.
	ErrDecisionPermissionDenied = errors.New("lebro: decision permission denied")
	// ErrDecisionRateLimited marks a rate-limited request.
	ErrDecisionRateLimited = errors.New("lebro: decision rate limited")
	// ErrDecisionUnavailable marks an unavailable provider.
	ErrDecisionUnavailable = errors.New("lebro: decision unavailable")
	// ErrDecisionTimeout marks a timed-out request.
	ErrDecisionTimeout = errors.New("lebro: decision request timed out")
	// ErrDecisionCancelled marks a cancelled request.
	ErrDecisionCancelled = errors.New("lebro: decision request cancelled")
	// ErrDecisionTransport marks a transport failure.
	ErrDecisionTransport = errors.New("lebro: decision transport failure")
	// ErrDecisionMalformedResponse marks an uninterpretable response.
	ErrDecisionMalformedResponse = errors.New("lebro: malformed decision response")
	// ErrDecisionUnknown marks an unclassified failure.
	ErrDecisionUnknown = errors.New("lebro: unknown decision failure")
)

// DecisionError preserves decision failure details behind a normalized
// category. It never carries credentials or image bytes: Message holds the
// provider's own description of the failure, and Err retains the original
// cause.
type DecisionError struct {
	// Kind is the normalized failure category.
	Kind DecisionErrorKind
	// Provider is the provider that reported the failure.
	Provider string
	// Code is the provider-specific error code, when it reports one.
	Code string
	// StatusCode is the HTTP status code, when the failure came from a
	// response.
	StatusCode int
	// RetryAfter is the delay the provider asked for, when it reported one.
	RetryAfter time.Duration
	// Message describes the failure.
	Message string
	// Err retains the original cause.
	Err error
}

func (e *DecisionError) Error() string {
	if e == nil {
		return "lebro: decision failure"
	}
	kind := e.Kind
	if kind == "" {
		kind = DecisionErrorUnknown
	}
	// The message is the only free text in the error, and the adapter
	// redacts it before it is set. The cause is kept for errors.Is and
	// errors.As chaining, but its text is never copied into the message, so
	// a cause that carries request content cannot reach a log through
	// Error().
	detail := e.Message
	provider := ""
	if e.Provider != "" {
		provider = " from " + e.Provider
	}
	if detail == "" {
		return fmt.Sprintf("lebro: decision %s%s", kind, provider)
	}
	return fmt.Sprintf("lebro: decision %s%s: %s", kind, provider, detail)
}

// Unwrap exposes the original cause.
func (e *DecisionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Is supports errors.Is checks against the normalized sentinels while Unwrap
// continues to preserve the original cause.
func (e *DecisionError) Is(target error) bool {
	if e == nil {
		return false
	}
	return target == decisionErrorSentinel(e.Kind)
}

// Retryable reports whether retrying later can reasonably succeed.
func (e *DecisionError) Retryable() bool {
	if e == nil {
		return false
	}
	return decisionErrorPolicyFor(e.Kind).retryable
}

func decisionErrorSentinel(kind DecisionErrorKind) error {
	return decisionErrorPolicyFor(kind).sentinel
}

type decisionErrorPolicy struct {
	sentinel  error
	retryable bool
}

func decisionErrorPolicyFor(kind DecisionErrorKind) decisionErrorPolicy {
	switch kind {
	case DecisionErrorInvalidRequest:
		return decisionErrorPolicy{sentinel: ErrDecisionInvalidRequest}
	case DecisionErrorUnsupportedCapability:
		return decisionErrorPolicy{sentinel: ErrDecisionUnsupportedCapability}
	case DecisionErrorAuthentication:
		return decisionErrorPolicy{sentinel: ErrDecisionAuthentication}
	case DecisionErrorPermissionDenied:
		return decisionErrorPolicy{sentinel: ErrDecisionPermissionDenied}
	case DecisionErrorRateLimited:
		return decisionErrorPolicy{sentinel: ErrDecisionRateLimited, retryable: true}
	case DecisionErrorUnavailable:
		return decisionErrorPolicy{sentinel: ErrDecisionUnavailable, retryable: true}
	case DecisionErrorTimeout:
		return decisionErrorPolicy{sentinel: ErrDecisionTimeout, retryable: true}
	case DecisionErrorCancelled:
		return decisionErrorPolicy{sentinel: ErrDecisionCancelled}
	case DecisionErrorTransport:
		return decisionErrorPolicy{sentinel: ErrDecisionTransport, retryable: true}
	case DecisionErrorMalformedResponse:
		return decisionErrorPolicy{sentinel: ErrDecisionMalformedResponse}
	default:
		return decisionErrorPolicy{sentinel: ErrDecisionUnknown}
	}
}

// Validate checks that a decision request is well formed. It is the
// provider-neutral part of request validation: a provider adapter adds its
// own capability and size checks before it sends anything.
func (r DecisionRequest) Validate() error {
	// One node budget covers the whole request, so a large multi-question
	// request cannot exceed the bound one field at a time.
	budget := 0
	if err := validateDecisionState(r.State, &budget); err != nil {
		return err
	}
	if len(r.Questions) == 0 {
		return errors.New("lebro: decision request requires at least one question")
	}
	for id, question := range r.Questions {
		if id == "" {
			return errors.New("lebro: decision question ID must not be empty")
		}
		if question == nil {
			return fmt.Errorf("lebro: decision question %q must not be nil", id)
		}
		if err := validateDecisionQuestion(id, question, &budget); err != nil {
			return err
		}
	}
	for i, image := range r.Images {
		if err := validateDecisionImage(image); err != nil {
			return fmt.Errorf("lebro: decision request image %d: %w", i, err)
		}
	}
	return nil
}

// Validate checks that a decision response is internally consistent.
func (r DecisionResponse) Validate() error {
	for id, answer := range r.Answers {
		if id == "" {
			return errors.New("lebro: decision response answer ID must not be empty")
		}
		if answer == nil {
			return fmt.Errorf("lebro: decision response answer %q must not be nil", id)
		}
		if err := validateDecisionAnswer(answer); err != nil {
			return fmt.Errorf("lebro: decision response answer %q: %w", id, err)
		}
	}
	if r.Attempts < 1 {
		return errors.New("lebro: decision response attempts must be at least one")
	}
	if r.Usage != nil {
		if r.Usage.InputTokens != nil && *r.Usage.InputTokens < 0 {
			return errors.New("lebro: decision usage input tokens must not be negative")
		}
		if r.Usage.OutputTokens != nil && *r.Usage.OutputTokens < 0 {
			return errors.New("lebro: decision usage output tokens must not be negative")
		}
	}
	return nil
}

func validateDecisionQuestion(id DecisionQuestionID, question DecisionQuestion, nodes *int) error {
	switch typed := question.(type) {
	case NoulQuestion:
		return validateNoulQuestion(id, typed, nodes)
	case *NoulQuestion:
		if typed == nil {
			return fmt.Errorf("lebro: decision question %q must not be nil", id)
		}
		return validateNoulQuestion(id, *typed, nodes)
	case ChoiceQuestion:
		return validateChoiceQuestion(id, typed, nodes)
	case *ChoiceQuestion:
		if typed == nil {
			return fmt.Errorf("lebro: decision question %q must not be nil", id)
		}
		return validateChoiceQuestion(id, *typed, nodes)
	case ScoreQuestion:
		return validateScoreQuestion(id, typed, nodes)
	case *ScoreQuestion:
		if typed == nil {
			return fmt.Errorf("lebro: decision question %q must not be nil", id)
		}
		return validateScoreQuestion(id, *typed, nodes)
	default:
		return fmt.Errorf("lebro: decision question %q has an unsupported type %T", id, question)
	}
}

func validateNoulQuestion(id DecisionQuestionID, typed NoulQuestion, nodes *int) error {
	if err := validateDecisionInstructions(id, typed.Instructions, nodes); err != nil {
		return err
	}
	if typed.Criteria != nil {
		if err := validateDecisionText(fmt.Sprintf("decision question %q criteria true", id), typed.Criteria.True, nodes); err != nil {
			return err
		}
		if err := validateDecisionText(fmt.Sprintf("decision question %q criteria false", id), typed.Criteria.False, nodes); err != nil {
			return err
		}
	}
	return nil
}

func validateChoiceQuestion(id DecisionQuestionID, typed ChoiceQuestion, nodes *int) error {
	if err := validateDecisionInstructions(id, typed.Instructions, nodes); err != nil {
		return err
	}
	if len(typed.Criteria) < 2 {
		return fmt.Errorf("lebro: decision question %q requires at least two options", id)
	}
	for option, description := range typed.Criteria {
		if option == "" {
			return fmt.Errorf("lebro: decision question %q has an empty option ID", id)
		}
		if err := validateDecisionText(fmt.Sprintf("decision question %q option %q", id, option), description, nodes); err != nil {
			return err
		}
	}
	return nil
}

func validateScoreQuestion(id DecisionQuestionID, typed ScoreQuestion, nodes *int) error {
	if err := validateDecisionInstructions(id, typed.Instructions, nodes); err != nil {
		return err
	}
	if len(typed.Criteria) < 2 {
		return fmt.Errorf("lebro: decision question %q requires at least two levels", id)
	}
	for level, description := range typed.Criteria {
		if err := validateDecisionText(fmt.Sprintf("decision question %q level %d", id, level), description, nodes); err != nil {
			return err
		}
	}
	return nil
}

func validateDecisionAnswer(answer DecisionAnswer) error {
	switch typed := answer.(type) {
	case NoulAnswer:
		return validateNoulAnswer(typed)
	case *NoulAnswer:
		if typed == nil {
			return errors.New("lebro: decision answer must not be nil")
		}
		return validateNoulAnswer(*typed)
	case ChoiceAnswer:
		return validateChoiceAnswer(typed)
	case *ChoiceAnswer:
		if typed == nil {
			return errors.New("lebro: decision answer must not be nil")
		}
		return validateChoiceAnswer(*typed)
	case ScoreAnswer:
		return validateScoreAnswer(typed)
	case *ScoreAnswer:
		if typed == nil {
			return errors.New("lebro: decision answer must not be nil")
		}
		return validateScoreAnswer(*typed)
	case RefusalAnswer:
		return validateRefusalAnswer(typed)
	case *RefusalAnswer:
		if typed == nil {
			return errors.New("lebro: decision answer must not be nil")
		}
		return validateRefusalAnswer(*typed)
	default:
		return fmt.Errorf("lebro: unsupported decision answer type %T", answer)
	}
}

// validateRefusalAnswer checks a refusal. The reason is optional: a
// provider may decline a question without explaining itself.
func validateRefusalAnswer(typed RefusalAnswer) error {
	return nil
}

func validateNoulAnswer(typed NoulAnswer) error {
	if err := validateProbability("noul probability", typed.ProbabilityYes); err != nil {
		return err
	}
	return validateOptionalConfidence(typed.Confidence, typed.ConfidenceMetric)
}

func validateChoiceAnswer(typed ChoiceAnswer) error {
	if typed.Choice == "" {
		return errors.New("lebro: choice answer requires a selected option")
	}
	if len(typed.Probabilities) > 0 {
		if _, ok := typed.Probabilities[typed.Choice]; !ok {
			return fmt.Errorf("lebro: choice answer selects option %q, which the reported distribution does not carry", typed.Choice)
		}
	}
	for option, probability := range typed.Probabilities {
		if option == "" {
			return errors.New("lebro: choice answer contains an empty option ID")
		}
		if err := validateProbability("choice probability", probability); err != nil {
			return err
		}
	}
	return validateOptionalConfidence(typed.Confidence, typed.ConfidenceMetric)
}

func validateScoreAnswer(typed ScoreAnswer) error {
	if typed.SelectedLevel != nil && *typed.SelectedLevel < 0 {
		return errors.New("lebro: score answer selected level must not be negative")
	}
	if typed.ExpectedLevel != nil {
		if math.IsNaN(*typed.ExpectedLevel) || math.IsInf(*typed.ExpectedLevel, 0) {
			return errors.New("lebro: score answer expected level must be finite")
		}
	}
	for level, probability := range typed.Probabilities {
		if level < 0 {
			return errors.New("lebro: score answer level must not be negative")
		}
		if err := validateProbability("score probability", probability); err != nil {
			return err
		}
	}
	switch typed.Provenance {
	case ScoreNative, ScoreNormalized:
	default:
		return fmt.Errorf("lebro: score answer provenance %q is not a known provenance", typed.Provenance)
	}
	return validateOptionalConfidence(typed.Confidence, typed.ConfidenceMetric)
}

func validateProbability(name string, probability float64) error {
	if math.IsNaN(probability) || math.IsInf(probability, 0) {
		return fmt.Errorf("%s must be a finite number", name)
	}
	if probability < 0 || probability > 1 {
		return fmt.Errorf("%s must be between 0 and 1, got %v", name, probability)
	}
	return nil
}

// validateOptionalConfidence checks a reported confidence and the metric that
// says what it means.
//
// A confidence and its metric travel together: an answer that reports a
// number without saying what the number means is a different answer from one
// that reports no number at all.
func validateOptionalConfidence(confidence *float64, metric DecisionConfidenceMetric) error {
	if confidence == nil {
		if metric != "" {
			return errors.New("lebro: a decision answer without a confidence must not name a confidence metric")
		}
		return nil
	}
	if metric == "" {
		return errors.New("lebro: a decision answer with a confidence must name a confidence metric")
	}
	return validateProbability("confidence", *confidence)
}

func validateDecisionState(state JSONValue, nodes *int) error {
	if state == nil {
		return errors.New("lebro: decision request requires a state")
	}
	switch typed := state.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return errors.New("lebro: decision request state must not be empty")
		}
	case map[string]any:
		if len(typed) == 0 {
			return errors.New("lebro: decision request state must not be empty")
		}
	case []any:
		if len(typed) == 0 {
			return errors.New("lebro: decision request state must not be empty")
		}
	default:
		// Other JSON shapes (numbers, booleans, or a named map or slice type)
		// are accepted only when they are a non-empty collection.
		value := reflect.ValueOf(state)
		switch value.Kind() {
		case reflect.Map:
			if value.Len() == 0 {
				return errors.New("lebro: decision request state must not be empty")
			}
		case reflect.Slice, reflect.Array:
			if value.Len() == 0 {
				return errors.New("lebro: decision request state must not be empty")
			}
		default:
			return errors.New("lebro: decision request state must be text, an object, or an array")
		}
	}
	return validateJSONValue("decision state", state, 0, nodes)
}

func validateDecisionInstructions(id DecisionQuestionID, instructions JSONValue, nodes *int) error {
	if instructions == nil {
		return fmt.Errorf("lebro: decision question %q requires instructions", id)
	}
	if text, ok := instructions.(string); ok {
		if strings.TrimSpace(text) == "" {
			return fmt.Errorf("lebro: decision question %q requires non-empty instructions", id)
		}
		return validateJSONValue(fmt.Sprintf("decision question %q instructions", id), instructions, 0, nodes)
	}
	value := reflect.ValueOf(instructions)
	switch value.Kind() {
	case reflect.Map:
		if value.Len() == 0 {
			return fmt.Errorf("lebro: decision question %q requires non-empty instructions", id)
		}
	case reflect.Slice, reflect.Array:
		if value.Len() == 0 {
			return fmt.Errorf("lebro: decision question %q requires non-empty instructions", id)
		}
	default:
		return fmt.Errorf("lebro: decision question %q instructions must be text, an object, or an array", id)
	}
	return validateJSONValue(fmt.Sprintf("decision question %q instructions", id), instructions, 0, nodes)
}

func validateDecisionText(name string, value JSONValue, nodes *int) error {
	if value == nil {
		return nil
	}
	if text, ok := value.(string); ok && strings.TrimSpace(text) == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	return validateJSONValue(name, value, 0, nodes)
}

func validateDecisionImage(image DecisionImage) error {
	if image.MediaType == "" {
		return errors.New("media type is required")
	}
	if !strings.Contains(image.MediaType, "/") {
		return fmt.Errorf("media type %q must be of the form type/subtype", image.MediaType)
	}
	if len(image.Bytes) == 0 {
		return errors.New("image bytes must not be empty")
	}
	if decisionImageIsReference(image.Bytes) {
		return errors.New("image bytes must be raw image data, not a URL or a data URL")
	}
	if !decisionImageMatchesMediaType(image.MediaType, image.Bytes) {
		return fmt.Errorf("image bytes do not match the declared media type %q", image.MediaType)
	}
	return nil
}

// decisionImageMatchesMediaType reports whether the bytes are the format the
// declared media type names.
//
// Only the formats the contract knows are checked. A media type the contract
// does not know is left to the provider, which stays the authority on its
// own formats.
func decisionImageMatchesMediaType(mediaType string, data []byte) bool {
	switch strings.ToLower(mediaType) {
	case "image/png":
		return bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	case "image/jpeg":
		return bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff})
	case "image/webp":
		return len(data) >= 12 && bytes.HasPrefix(data, []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP"))
	default:
		return true
	}
}

// decisionImageIsReference reports whether image bytes carry a remote
// reference instead of image data.
//
// A decision image is always raw bytes, and an adapter never fetches one. The
// check is deliberately narrow: it only fires on text that starts with a URL
// or a data URL scheme, which no image format produces.
func decisionImageIsReference(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return false
	}
	// Only the leading bytes decide, so a full-size image is not copied
	// into a string on every validation.
	prefix := trimmed
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	text := strings.ToLower(string(prefix))
	return strings.HasPrefix(text, "http://") ||
		strings.HasPrefix(text, "https://") ||
		strings.HasPrefix(text, "data:")
}

// ValidateDecisionImages checks images against the capabilities a provider
// advertises. It is the local gate that keeps an unsupported image request
// from reaching the network: an adapter calls it before it encodes anything.
//
// A media type the provider does not accept fails as an unsupported
// capability, and an image count or size beyond the advertised limits fails as
// an invalid request.
func ValidateDecisionImages(caps DecisionImageCapabilities, images []DecisionImage) error {
	if len(images) == 0 {
		return nil
	}
	if !caps.Enabled {
		return fmt.Errorf("%w: the provider does not accept image inputs", ErrDecisionUnsupportedCapability)
	}
	if caps.MaxImages > 0 && len(images) > caps.MaxImages {
		return fmt.Errorf("%w: the provider accepts at most %d images, got %d", ErrDecisionInvalidRequest, caps.MaxImages, len(images))
	}
	var total int64
	for i, image := range images {
		// A malformed media type is a request error, not a capability one.
		// The generic request validation reports it.
		if strings.Contains(image.MediaType, "/") && !decisionMediaTypeAllowed(caps.MediaTypes, image.MediaType) {
			return fmt.Errorf("%w: image %d media type %q is not accepted", ErrDecisionUnsupportedCapability, i, image.MediaType)
		}
		size := int64(len(image.Bytes))
		if caps.MaxBytesPerImage > 0 && size > caps.MaxBytesPerImage {
			return fmt.Errorf("%w: image %d is %d bytes, the provider accepts at most %d", ErrDecisionInvalidRequest, i, size, caps.MaxBytesPerImage)
		}
		total += size
	}
	if caps.MaxTotalBytes > 0 && total > caps.MaxTotalBytes {
		return fmt.Errorf("%w: the images total %d bytes, the provider accepts at most %d", ErrDecisionInvalidRequest, total, caps.MaxTotalBytes)
	}
	return nil
}

func decisionMediaTypeAllowed(allowed []string, mediaType string) bool {
	for _, candidate := range allowed {
		if strings.EqualFold(candidate, mediaType) {
			return true
		}
	}
	return false
}

const (
	// maxDecisionJSONDepth bounds the nesting of decision state, instructions,
	// and criteria. It matches the deepest nesting a provider documents.
	maxDecisionJSONDepth = 64
	// maxDecisionJSONNodes bounds how many values a single decision request
	// may carry, so a hostile or accidental structure cannot stall a call.
	maxDecisionJSONNodes = 100000
)

// validateJSONValue checks that a value can be represented as JSON. It walks
// the value reflectively instead of marshaling it, so a cyclic structure is
// reported as an error instead of exhausting the stack, and so a non-finite
// number is rejected rather than silently encoded.
func validateJSONValue(name string, value JSONValue, depth int, nodes *int) error {
	if depth > maxDecisionJSONDepth {
		return fmt.Errorf("%s exceeds the maximum nesting depth of %d", name, maxDecisionJSONDepth)
	}
	if value == nil {
		return nil
	}
	return validateJSONReflect(name, reflect.ValueOf(value), depth, nodes)
}

func validateJSONReflect(name string, value reflect.Value, depth int, nodes *int) error {
	*nodes++
	if *nodes > maxDecisionJSONNodes {
		return fmt.Errorf("%s carries more than %d values", name, maxDecisionJSONNodes)
	}
	if depth > maxDecisionJSONDepth {
		return fmt.Errorf("%s exceeds the maximum nesting depth of %d", name, maxDecisionJSONDepth)
	}
	switch value.Kind() {
	case reflect.Invalid, reflect.Pointer, reflect.Interface:
		if value.IsNil() {
			return nil
		}
		return validateJSONReflect(name, value.Elem(), depth, nodes)
	case reflect.Bool:
		return nil
	case reflect.String:
		if !utf8.ValidString(value.String()) {
			return fmt.Errorf("%s contains a string that is not valid UTF-8", name)
		}
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return nil
	case reflect.Float32, reflect.Float64:
		number := value.Float()
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return fmt.Errorf("%s contains a number that is not finite", name)
		}
		return nil
	case reflect.Slice, reflect.Array:
		if value.Type() == reflect.TypeOf(json.RawMessage{}) {
			return validateJSONRawMessage(name, value.Bytes(), depth, nodes)
		}
		if value.Type().Elem().Kind() == reflect.Uint8 {
			// encoding/json encodes a byte slice as a base64 string, which
			// would silently change the value on the wire.
			return fmt.Errorf("%s contains a byte slice, which would be encoded as a base64 string", name)
		}
		for i := 0; i < value.Len(); i++ {
			if err := validateJSONReflect(name, value.Index(i), depth+1, nodes); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("%s must use string keys", name)
		}
		iterator := value.MapRange()
		for iterator.Next() {
			if err := validateJSONReflect(name, iterator.Value(), depth+1, nodes); err != nil {
				return err
			}
		}
		return nil
	case reflect.Struct:
		return validateJSONStruct(name, value, depth, nodes)
	default:
		return fmt.Errorf("%s contains a value of type %s, which is not JSON-compatible", name, value.Type())
	}
}

func validateJSONStruct(name string, value reflect.Value, depth int, nodes *int) error {
	structType := value.Type()
	for i := 0; i < structType.NumField(); i++ {
		field := structType.Field(i)
		if !field.IsExported() {
			continue
		}
		if err := validateJSONReflect(name, value.Field(i), depth+1, nodes); err != nil {
			return err
		}
	}
	return nil
}

// validateJSONRawMessage checks raw JSON through the same bounded traversal
// as any other value, so a raw message cannot carry a payload past the
// request's depth and node limits.
func validateJSONRawMessage(name string, raw []byte, depth int, nodes *int) error {
	if len(raw) == 0 {
		return fmt.Errorf("%s contains an empty raw JSON value", name)
	}
	if !json.Valid(raw) {
		return fmt.Errorf("%s contains raw JSON that is not valid", name)
	}
	if !utf8.Valid(raw) {
		// Decoding would replace the bad bytes before the string check
		// could see them.
		return fmt.Errorf("%s contains raw JSON with invalid UTF-8", name)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return fmt.Errorf("%s contains raw JSON that cannot be read: %w", name, err)
	}
	return validateJSONValue(name, decoded, depth, nodes)
}
