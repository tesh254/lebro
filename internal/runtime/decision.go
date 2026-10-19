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

// DecisionAnswer is one answer in a decision response. The set of answer kinds
// is closed: NoulAnswer, ChoiceAnswer, and ScoreAnswer.
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
}

func (NoulAnswer) isDecisionAnswer() {}

// ChoiceAnswer is the answer to a choice question.
type ChoiceAnswer struct {
	// Choice is the selected option.
	Choice DecisionOptionID
	// Probabilities maps every requested option to its probability.
	Probabilities map[DecisionOptionID]float64
	// Confidence is the provider-reported certainty. It is nil when the
	// provider does not report confidence.
	Confidence *float64
}

func (ChoiceAnswer) isDecisionAnswer() {}

// ScoreProvenance records how a score answer was produced.
type ScoreProvenance string

const (
	// ScoreNative marks a value the provider reported directly.
	ScoreNative ScoreProvenance = "native"
	// ScoreNormalized marks a value an adapter derived from a documented
	// distribution instead of receiving it from the provider.
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
	// Provenance records whether the values were reported by the provider or
	// derived by the adapter.
	Provenance ScoreProvenance
}

func (ScoreAnswer) isDecisionAnswer() {}

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
	// Model is the provider model that produced the response.
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

// DecisionCapabilities describes what a decision provider supports. Callers
// use it to pick a provider and to validate a request before it is sent.
type DecisionCapabilities struct {
	// Noul reports whether binary questions are supported.
	Noul bool
	// Choice reports whether choice questions are supported.
	Choice bool
	// Score reports whether score questions are supported.
	Score bool
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
	// Distributions reports whether the provider returns a probability
	// distribution alongside an answer.
	Distributions bool
	// Confidence reports whether the provider reports confidence.
	Confidence bool
	// Batching reports whether the provider accepts a batch of independent
	// requests in one call.
	Batching bool
	// MaxStateBytes is the largest accepted state size in bytes. Zero means
	// unspecified.
	MaxStateBytes int64
	// RetryableStatuses lists the HTTP status codes the provider documents as
	// retryable.
	RetryableStatuses []int
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
	// Capabilities describes what the adapter supports.
	Capabilities() DecisionCapabilities
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
	if g.MinimumConfidence != nil {
		if answer.Confidence == nil || *answer.Confidence < *g.MinimumConfidence {
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
	detail := e.Message
	if detail == "" && e.Err != nil {
		detail = e.Err.Error()
	}
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
	if err := validateDecisionState(r.State); err != nil {
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
		if err := validateDecisionQuestion(id, question); err != nil {
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
	if r.Attempts < 0 {
		return errors.New("lebro: decision response attempts must not be negative")
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

func validateDecisionQuestion(id DecisionQuestionID, question DecisionQuestion) error {
	switch typed := question.(type) {
	case NoulQuestion:
		return validateNoulQuestion(id, typed)
	case *NoulQuestion:
		if typed == nil {
			return fmt.Errorf("lebro: decision question %q must not be nil", id)
		}
		return validateNoulQuestion(id, *typed)
	case ChoiceQuestion:
		return validateChoiceQuestion(id, typed)
	case *ChoiceQuestion:
		if typed == nil {
			return fmt.Errorf("lebro: decision question %q must not be nil", id)
		}
		return validateChoiceQuestion(id, *typed)
	case ScoreQuestion:
		return validateScoreQuestion(id, typed)
	case *ScoreQuestion:
		if typed == nil {
			return fmt.Errorf("lebro: decision question %q must not be nil", id)
		}
		return validateScoreQuestion(id, *typed)
	default:
		return fmt.Errorf("lebro: decision question %q has an unsupported type %T", id, question)
	}
}

func validateNoulQuestion(id DecisionQuestionID, typed NoulQuestion) error {
	if err := validateDecisionInstructions(id, typed.Instructions); err != nil {
		return err
	}
	if typed.Criteria != nil {
		if err := validateDecisionText(fmt.Sprintf("decision question %q criteria true", id), typed.Criteria.True); err != nil {
			return err
		}
		if err := validateDecisionText(fmt.Sprintf("decision question %q criteria false", id), typed.Criteria.False); err != nil {
			return err
		}
	}
	return nil
}

func validateChoiceQuestion(id DecisionQuestionID, typed ChoiceQuestion) error {
	if err := validateDecisionInstructions(id, typed.Instructions); err != nil {
		return err
	}
	if len(typed.Criteria) < 2 {
		return fmt.Errorf("lebro: decision question %q requires at least two options", id)
	}
	for option, description := range typed.Criteria {
		if option == "" {
			return fmt.Errorf("lebro: decision question %q has an empty option ID", id)
		}
		if err := validateDecisionText(fmt.Sprintf("decision question %q option %q", id, option), description); err != nil {
			return err
		}
	}
	return nil
}

func validateScoreQuestion(id DecisionQuestionID, typed ScoreQuestion) error {
	if err := validateDecisionInstructions(id, typed.Instructions); err != nil {
		return err
	}
	if len(typed.Criteria) < 2 {
		return fmt.Errorf("lebro: decision question %q requires at least two levels", id)
	}
	for level, description := range typed.Criteria {
		if err := validateDecisionText(fmt.Sprintf("decision question %q level %d", id, level), description); err != nil {
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
	default:
		return fmt.Errorf("lebro: unsupported decision answer type %T", answer)
	}
}

func validateNoulAnswer(typed NoulAnswer) error {
	if err := validateProbability("noul probability", typed.ProbabilityYes); err != nil {
		return err
	}
	return validateOptionalConfidence(typed.Confidence)
}

func validateChoiceAnswer(typed ChoiceAnswer) error {
	if typed.Choice == "" {
		return errors.New("lebro: choice answer requires a selected option")
	}
	for option, probability := range typed.Probabilities {
		if option == "" {
			return errors.New("lebro: choice answer contains an empty option ID")
		}
		if err := validateProbability("choice probability", probability); err != nil {
			return err
		}
	}
	return validateOptionalConfidence(typed.Confidence)
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
	return validateOptionalConfidence(typed.Confidence)
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

func validateOptionalConfidence(confidence *float64) error {
	if confidence == nil {
		return nil
	}
	return validateProbability("confidence", *confidence)
}

func validateDecisionState(state JSONValue) error {
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
	return validateJSONValue("decision state", state, 0)
}

func validateDecisionInstructions(id DecisionQuestionID, instructions JSONValue) error {
	if instructions == nil {
		return fmt.Errorf("lebro: decision question %q requires instructions", id)
	}
	if text, ok := instructions.(string); ok {
		if strings.TrimSpace(text) == "" {
			return fmt.Errorf("lebro: decision question %q requires non-empty instructions", id)
		}
		return validateJSONValue(fmt.Sprintf("decision question %q instructions", id), instructions, 0)
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
	return validateJSONValue(fmt.Sprintf("decision question %q instructions", id), instructions, 0)
}

func validateDecisionText(name string, value JSONValue) error {
	if value == nil {
		return nil
	}
	if text, ok := value.(string); ok && strings.TrimSpace(text) == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	return validateJSONValue(name, value, 0)
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
	text := strings.ToLower(string(trimmed))
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
func validateJSONValue(name string, value JSONValue, depth int) error {
	if depth > maxDecisionJSONDepth {
		return fmt.Errorf("%s exceeds the maximum nesting depth of %d", name, maxDecisionJSONDepth)
	}
	if value == nil {
		return nil
	}
	nodes := 0
	return validateJSONReflect(name, reflect.ValueOf(value), depth, &nodes)
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
	case reflect.Bool, reflect.String:
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
			return validateJSONRawMessage(name, value.Bytes())
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

func validateJSONRawMessage(name string, raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	if !json.Valid(raw) {
		return fmt.Errorf("%s contains raw JSON that is not valid", name)
	}
	return nil
}
