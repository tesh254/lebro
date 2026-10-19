package decisions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/tesh254/lebro"
)

const (
	// systemOnePath is the System One evaluation path every provider in this
	// package exposes.
	systemOnePath = "/v1/systemone"
)

// systemOneProfile captures the documented differences between the providers
// this package adapts. Everything the providers share lives in the wire and
// client code; everything they document differently lives here.
type systemOneProfile struct {
	// provider is the stable provider identifier reported in a response.
	provider string
	// model is the provider model the profile was verified for.
	model string
	// capabilities describes what the provider supports.
	capabilities lebro.DecisionCapabilities
	// distributionTolerance is the largest deviation a reported probability
	// distribution may have from a total of one.
	distributionTolerance float64
	// scoreWeightedField is the wire field that holds the probability-
	// weighted level of a score answer.
	scoreWeightedField string
	// scoreWinnerField is the wire field that holds the winning level index
	// of a score answer. It is empty when the provider reports no winner.
	scoreWinnerField string
	// buildQuestions renders the question map. It is provider-specific
	// because one provider accepts text instructions only.
	buildQuestions func(map[lebro.DecisionQuestionID]lebro.DecisionQuestion) (map[string]any, error)
	// extraFields adds the provider-specific top-level request fields.
	extraFields func(lebro.DecisionRequest) map[string]any
	// imageFields renders the provider-specific image fields for a request.
	// It is nil when the provider takes no images.
	imageFields func(lebro.DecisionRequest) map[string]any
	// maxBodyBytes is the largest request body the provider accepts. Zero
	// means the provider documents no byte limit.
	maxBodyBytes int64
	// requestIDHeaders are the response headers that carry the provider
	// request identifier, when the provider documents one. The first header
	// that carries a value wins.
	requestIDHeaders []string
	// requestSecrets returns the provider-specific request content that must
	// never reach an error message, such as an encoded image payload.
	requestSecrets func(lebro.DecisionRequest) []string
}

// decide runs one decision request against a provider.
func decide(ctx context.Context, client *decisionClient, profile systemOneProfile, request lebro.DecisionRequest) (lebro.DecisionResponse, error) {
	// The image capability gate runs first: a provider that does not accept
	// images at all reports that, rather than a complaint about the bytes.
	if err := lebro.ValidateDecisionImages(profile.capabilities.Images, request.Images); err != nil {
		kind := lebro.DecisionErrorInvalidRequest
		if errors.Is(err, lebro.ErrDecisionUnsupportedCapability) {
			kind = lebro.DecisionErrorUnsupportedCapability
		}
		return lebro.DecisionResponse{}, client.failure(kind, err.Error(), err)
	}
	if err := request.Validate(); err != nil {
		return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorInvalidRequest, err.Error(), err)
	}
	if err := checkDecisionKinds(profile.capabilities, profile.provider, request); err != nil {
		return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorUnsupportedCapability, err.Error(), err)
	}
	if err := checkDecisionLimits(profile.capabilities, profile.provider, request); err != nil {
		return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorInvalidRequest, err.Error(), err)
	}

	model := request.Model
	if model == "" {
		model = client.defaultModel()
	}
	if model == "" {
		return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorInvalidRequest, "lebro: model is required", nil)
	}

	if !profile.capabilities.StructuredState {
		if _, ok := request.State.(string); !ok {
			return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorInvalidRequest,
				fmt.Sprintf("lebro: %s accepts text state only", profile.provider), nil)
		}
	}
	questions, err := profile.buildQuestions(request.Questions)
	if err != nil {
		return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorInvalidRequest, err.Error(), err)
	}

	body := map[string]any{
		"model":     model,
		"state":     request.State,
		"questions": questions,
	}
	if profile.extraFields != nil {
		for key, value := range profile.extraFields(request) {
			body[key] = value
		}
	}
	if profile.imageFields != nil {
		for key, value := range profile.imageFields(request) {
			body[key] = value
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorInvalidRequest, fmt.Sprintf("lebro: encode request: %v", err), err)
	}
	if profile.capabilities.MaxStateBytes > 0 {
		state, err := json.Marshal(request.State)
		if err != nil {
			return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorInvalidRequest, fmt.Sprintf("lebro: encode state: %v", err), err)
		}
		if int64(len(state)) > profile.capabilities.MaxStateBytes {
			return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorInvalidRequest,
				fmt.Sprintf("lebro: the state is %d bytes, %s accepts at most %d", len(state), profile.provider, profile.capabilities.MaxStateBytes), nil)
		}
	}
	if profile.maxBodyBytes > 0 && int64(len(encoded)) > profile.maxBodyBytes {
		return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorInvalidRequest,
			fmt.Sprintf("lebro: the request body is %d bytes, %s accepts at most %d", len(encoded), profile.provider, profile.maxBodyBytes), nil)
	}

	result, err := client.post(ctx, encoded, profile.requestIDHeaders, retryableStatuses(profile.capabilities.RetryableStatuses))
	if err != nil {
		return lebro.DecisionResponse{}, err
	}
	if result.status >= http.StatusBadRequest {
		// The redactor is built only here: a successful request must not
		// pay for a second copy of the request content.
		redactor := client.newRedactor(request, profile.requestSecrets)
		return lebro.DecisionResponse{}, client.statusFailure(result.status, result.body, result.retryAfter, redactor)
	}

	decoded, err := decodeSystemOneResponse(result.body)
	if err != nil {
		return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorMalformedResponse, err.Error(), err)
	}
	if decoded.Model == "" {
		return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorMalformedResponse, "lebro: the decision response carries no model identity", nil)
	}
	answers, err := mapSystemOneAnswers(request, decoded.Answers, profile)
	if err != nil {
		return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorMalformedResponse, err.Error(), err)
	}

	response := lebro.DecisionResponse{
		Provider:         lebro.ProviderID(profile.provider),
		UpstreamProvider: lebro.ProviderID(decoded.Provider),
		RequestedModel:   model,
		Model:            decoded.Model,
		Answers:          answers,
		RequestID:        result.requestID,
		Duration:         result.duration,
		Attempts:         result.attempts,
	}
	if response.RequestID == "" {
		// A gateway reports the generation it made in the body when it does
		// not repeat it in a header.
		response.RequestID = decoded.ID
	}
	if decoded.Usage != nil {
		usage := &lebro.DecisionUsage{}
		if decoded.Usage.InputTokens != nil {
			if *decoded.Usage.InputTokens < 0 {
				return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorMalformedResponse, "lebro: the provider reported a negative input token count", nil)
			}
			tokens := *decoded.Usage.InputTokens
			usage.InputTokens = &tokens
		}
		if decoded.Usage.OutputTokens != nil {
			if *decoded.Usage.OutputTokens < 0 {
				return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorMalformedResponse, "lebro: the provider reported a negative output token count", nil)
			}
			tokens := *decoded.Usage.OutputTokens
			usage.OutputTokens = &tokens
		}
		response.Usage = usage
	}
	return response, nil
}

// checkDecisionKinds rejects a question kind the provider does not support.
func checkDecisionKinds(caps lebro.DecisionCapabilities, provider string, request lebro.DecisionRequest) error {
	for id, question := range request.Questions {
		switch question.(type) {
		case lebro.NoulQuestion, *lebro.NoulQuestion:
			if !caps.Noul {
				return fmt.Errorf("lebro: decision question %q is a noul question, which %s does not support", id, provider)
			}
		case lebro.ChoiceQuestion, *lebro.ChoiceQuestion:
			if !caps.Choice {
				return fmt.Errorf("lebro: decision question %q is a choice question, which %s does not support", id, provider)
			}
		case lebro.ScoreQuestion, *lebro.ScoreQuestion:
			if !caps.Score {
				return fmt.Errorf("lebro: decision question %q is a score question, which %s does not support", id, provider)
			}
		}
	}
	return nil
}

// checkDecisionLimits rejects a request that exceeds a documented limit.
func checkDecisionLimits(caps lebro.DecisionCapabilities, provider string, request lebro.DecisionRequest) error {
	if caps.MaxQuestions > 0 && len(request.Questions) > caps.MaxQuestions {
		return fmt.Errorf("lebro: %s accepts at most %d questions per request, got %d", provider, caps.MaxQuestions, len(request.Questions))
	}
	if !caps.MultipleQuestions && len(request.Questions) > 1 {
		return fmt.Errorf("lebro: %s accepts one question per request, got %d", provider, len(request.Questions))
	}
	for id, question := range request.Questions {
		var options, levels int
		switch typed := question.(type) {
		case lebro.ChoiceQuestion:
			options = len(typed.Criteria)
		case *lebro.ChoiceQuestion:
			if typed != nil {
				options = len(typed.Criteria)
			}
		case lebro.ScoreQuestion:
			levels = len(typed.Criteria)
		case *lebro.ScoreQuestion:
			if typed != nil {
				levels = len(typed.Criteria)
			}
		}
		if caps.MaxChoiceOptions > 0 && options > caps.MaxChoiceOptions {
			return fmt.Errorf("lebro: decision question %q has %d options, %s accepts at most %d", id, options, provider, caps.MaxChoiceOptions)
		}
		if caps.MaxScoreLevels > 0 && levels > caps.MaxScoreLevels {
			return fmt.Errorf("lebro: decision question %q has %d levels, %s accepts at most %d", id, levels, provider, caps.MaxScoreLevels)
		}
	}
	return nil
}

// systemOneResponse is the shared System One response envelope.
type systemOneResponse struct {
	Model string `json:"model"`
	// ID is the provider's identifier for the exchange. A gateway reports
	// the generation it made here.
	ID string `json:"id"`
	// Provider names the provider that actually served the request. A
	// gateway reports it; a direct connection leaves it empty.
	Provider string                     `json:"provider"`
	Answers  map[string]systemOneAnswer `json:"answers"`
	Usage    *systemOneUsage            `json:"usage"`
}

// systemOneUsage is the shared System One usage envelope.
type systemOneUsage struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
}

// systemOneAnswer is the shared System One answer shape. A provider fills
// the fields its question type defines and leaves the rest unset.
type systemOneAnswer struct {
	Type          string                     `json:"type"`
	Noul          *float64                   `json:"noul"`
	Choice        *string                    `json:"choice"`
	Probabilities map[string]float64         `json:"probabilities"`
	Confidence    *float64                   `json:"confidence"`
	Legend        map[string]json.RawMessage `json:"legend"`
	// Fields holds every field of the answer. A provider names its score
	// fields, so the answer cannot decode them into fixed fields.
	Fields map[string]json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps every field of the answer alongside the decoded ones.
func (a *systemOneAnswer) UnmarshalJSON(data []byte) error {
	type answerFields systemOneAnswer
	var decoded answerFields
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*a = systemOneAnswer(decoded)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	a.Fields = fields
	return nil
}

// number returns a numeric field of the answer.
func (a systemOneAnswer) number(name string) (json.Number, bool) {
	raw, ok := a.Fields[name]
	if !ok {
		return "", false
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return "", false
	}
	return number, true
}

// decodeSystemOneResponse decodes the shared response envelope.
func decodeSystemOneResponse(body []byte) (*systemOneResponse, error) {
	var decoded systemOneResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("lebro: decode the decision response: %w", err)
	}
	if decoded.Answers == nil {
		return nil, errors.New("lebro: the decision response carries no answers")
	}
	return &decoded, nil
}

// mapSystemOneAnswers maps the provider answers onto the requested question
// identifiers.
func mapSystemOneAnswers(request lebro.DecisionRequest, wire map[string]systemOneAnswer, profile systemOneProfile) (map[lebro.DecisionQuestionID]lebro.DecisionAnswer, error) {
	answers := make(map[lebro.DecisionQuestionID]lebro.DecisionAnswer, len(request.Questions))
	for id, question := range request.Questions {
		raw, ok := wire[string(id)]
		if !ok {
			return nil, fmt.Errorf("lebro: the provider returned no answer for question %q", id)
		}
		answer, err := mapSystemOneAnswer(id, question, raw, profile)
		if err != nil {
			return nil, err
		}
		answers[id] = answer
	}
	for key := range wire {
		if _, ok := request.Questions[lebro.DecisionQuestionID(key)]; !ok {
			// An answer for a question that was never asked is a response
			// the adapter cannot place, not an answer to ignore.
			return nil, fmt.Errorf("lebro: the provider returned an answer for the unknown question %q", key)
		}
	}
	return answers, nil
}

// questionUncertainty returns the uncertainty the profile advertises for a
// question kind.
func questionUncertainty(capabilities lebro.DecisionCapabilities, question lebro.DecisionQuestion) lebro.DecisionUncertainty {
	switch question.(type) {
	case lebro.NoulQuestion, *lebro.NoulQuestion:
		return capabilities.NoulUncertainty
	case lebro.ChoiceQuestion, *lebro.ChoiceQuestion:
		return capabilities.ChoiceUncertainty
	case lebro.ScoreQuestion, *lebro.ScoreQuestion:
		return capabilities.ScoreUncertainty
	default:
		return lebro.DecisionUncertainty{}
	}
}

// applyConfidence stamps the metric on a reported confidence.
//
// A provider that reports a confidence the profile does not describe gets an
// unknown metric: the number is there and what it means is not known.
func applyConfidence(id lebro.DecisionQuestionID, uncertainty lebro.DecisionUncertainty, reported *float64) (*float64, lebro.DecisionConfidenceMetric, error) {
	if uncertainty.Confidence == lebro.DecisionAvailabilityUnavailable {
		if reported != nil {
			// The profile says the provider reports no confidence for this
			// kind, so a confidence in the answer is a value the adapter
			// cannot vouch for.
			return nil, "", fmt.Errorf("lebro: decision question %q returned a confidence the provider does not report", id)
		}
		return nil, "", nil
	}
	if reported == nil {
		if uncertainty.Confidence == lebro.DecisionAvailabilityRequired {
			return nil, "", fmt.Errorf("lebro: decision question %q returned no confidence", id)
		}
		return nil, "", nil
	}
	if err := checkProbability(*reported); err != nil {
		return nil, "", fmt.Errorf("lebro: decision question %q: %w", id, err)
	}
	confidence := *reported
	return &confidence, uncertainty.ConfidenceMetric, nil
}

// distributionEpsilon absorbs the rounding of a floating point sum.
const distributionEpsilon = 1e-9

// distributionTolerance returns the largest deviation a reported
// distribution of count outcomes may have from a total of one.
//
// A provider that rounds each probability to two decimals moves the total by
// up to half a hundredth per outcome, so the bound follows the number of
// outcomes rather than being one number for every question.
func distributionTolerance(configured float64, count int) float64 {
	tolerance := configured
	if rounding := 0.005 * float64(count); rounding > tolerance {
		tolerance = rounding
	}
	return tolerance + distributionEpsilon
}

// checkDistribution checks a reported probability distribution.
//
// A reported distribution must add up to one within the provider's
// tolerance. A distribution the provider did not report is absent: it is
// malformed only when the profile says the provider always reports one, and
// a distribution the profile says the provider never reports is malformed
// too.
func checkDistribution(id lebro.DecisionQuestionID, uncertainty lebro.DecisionUncertainty, reported bool, total, tolerance float64) error {
	if uncertainty.Distribution == lebro.DecisionAvailabilityUnavailable {
		if reported {
			return fmt.Errorf("lebro: decision question %q returned a probability distribution the provider does not report", id)
		}
		return nil
	}
	if !reported {
		if uncertainty.Distribution == lebro.DecisionAvailabilityRequired {
			return fmt.Errorf("lebro: decision question %q returned no probability distribution", id)
		}
		return nil
	}
	if difference := math.Abs(total - 1); difference > tolerance {
		return fmt.Errorf("lebro: decision question %q returned a probability distribution that adds up to %v instead of one", id, total)
	}
	return nil
}

// systemOneDistribution is a probability distribution the provider reported.
type systemOneDistribution struct {
	// present reports whether the provider sent a distribution at all.
	present bool
	// total is the sum of the reported probabilities.
	total float64
}

// mapSystemOneAnswer maps one provider answer onto the neutral contract.
func mapSystemOneAnswer(id lebro.DecisionQuestionID, question lebro.DecisionQuestion, raw systemOneAnswer, profile systemOneProfile) (lebro.DecisionAnswer, error) {
	uncertainty := questionUncertainty(profile.capabilities, question)
	switch typed := question.(type) {
	case lebro.NoulQuestion:
		return mapSystemOneNoul(id, raw, uncertainty)
	case *lebro.NoulQuestion:
		if typed == nil {
			return nil, fmt.Errorf("lebro: decision question %q must not be nil", id)
		}
		return mapSystemOneNoul(id, raw, uncertainty)
	case lebro.ChoiceQuestion:
		return mapSystemOneChoice(id, typed, raw, uncertainty, profile)
	case *lebro.ChoiceQuestion:
		if typed == nil {
			return nil, fmt.Errorf("lebro: decision question %q must not be nil", id)
		}
		return mapSystemOneChoice(id, *typed, raw, uncertainty, profile)
	case lebro.ScoreQuestion:
		return mapSystemOneScore(id, typed, raw, uncertainty, profile)
	case *lebro.ScoreQuestion:
		if typed == nil {
			return nil, fmt.Errorf("lebro: decision question %q must not be nil", id)
		}
		return mapSystemOneScore(id, *typed, raw, uncertainty, profile)
	default:
		return nil, fmt.Errorf("lebro: decision question %q has an unsupported type %T", id, question)
	}
}

// mapSystemOneNoul maps a noul answer. The provider confidence is reported
// only when the provider sends one.
func mapSystemOneNoul(id lebro.DecisionQuestionID, raw systemOneAnswer, uncertainty lebro.DecisionUncertainty) (lebro.DecisionAnswer, error) {
	if raw.Type != "noul" {
		return nil, fmt.Errorf("lebro: decision question %q expected a noul answer, got %q", id, raw.Type)
	}
	if raw.Noul == nil {
		return nil, fmt.Errorf("lebro: decision question %q returned no noul probability", id)
	}
	if err := checkProbability(*raw.Noul); err != nil {
		return nil, fmt.Errorf("lebro: decision question %q: %w", id, err)
	}
	confidence, metric, err := applyConfidence(id, uncertainty, raw.Confidence)
	if err != nil {
		return nil, err
	}
	return lebro.NoulAnswer{ProbabilityYes: *raw.Noul, Confidence: confidence, ConfidenceMetric: metric}, nil
}

// mapSystemOneChoice maps a choice answer.
func mapSystemOneChoice(id lebro.DecisionQuestionID, question lebro.ChoiceQuestion, raw systemOneAnswer, uncertainty lebro.DecisionUncertainty, profile systemOneProfile) (lebro.DecisionAnswer, error) {
	if raw.Type != "choice" {
		return nil, fmt.Errorf("lebro: decision question %q expected a choice answer, got %q", id, raw.Type)
	}
	if raw.Choice == nil || *raw.Choice == "" {
		return nil, fmt.Errorf("lebro: decision question %q returned no selected option", id)
	}
	selected := lebro.DecisionOptionID(*raw.Choice)
	if _, ok := question.Criteria[selected]; !ok {
		return nil, fmt.Errorf("lebro: decision question %q selected option %q, which was not requested", id, selected)
	}
	probabilities, distribution, err := mapSystemOneProbabilities(id, question.Criteria, raw.Probabilities)
	if err != nil {
		return nil, err
	}
	tolerance := distributionTolerance(profile.distributionTolerance, len(question.Criteria))
	if err := checkDistribution(id, uncertainty, distribution.present, distribution.total, tolerance); err != nil {
		return nil, err
	}
	confidence, metric, err := applyConfidence(id, uncertainty, raw.Confidence)
	if err != nil {
		return nil, err
	}
	return lebro.ChoiceAnswer{Choice: selected, Probabilities: probabilities, Confidence: confidence, ConfidenceMetric: metric}, nil
}

// mapSystemOneProbabilities maps the provider distribution onto the requested
// options. Every requested option must carry a probability: a partial
// distribution is a malformed response, not a distribution with implicit
// zeros.
func mapSystemOneProbabilities(id lebro.DecisionQuestionID, requested map[lebro.DecisionOptionID]lebro.JSONValue, reported map[string]float64) (map[lebro.DecisionOptionID]float64, systemOneDistribution, error) {
	if len(requested) == 0 {
		return nil, systemOneDistribution{}, fmt.Errorf("lebro: decision question %q requested no options", id)
	}
	if reported == nil {
		// The provider reported no distribution. The answer carries none
		// rather than one the provider did not send.
		return nil, systemOneDistribution{}, nil
	}
	for option := range reported {
		if _, ok := requested[lebro.DecisionOptionID(option)]; !ok {
			return nil, systemOneDistribution{}, fmt.Errorf("lebro: decision question %q returned a probability for option %q, which was not requested", id, option)
		}
	}
	probabilities := make(map[lebro.DecisionOptionID]float64, len(requested))
	distribution := systemOneDistribution{present: true}
	for option := range requested {
		probability, ok := reported[string(option)]
		if !ok {
			return nil, systemOneDistribution{}, fmt.Errorf("lebro: decision question %q returned no probability for option %q", id, option)
		}
		if err := checkProbability(probability); err != nil {
			return nil, systemOneDistribution{}, fmt.Errorf("lebro: decision question %q: %w", id, err)
		}
		probabilities[option] = probability
		distribution.total += probability
	}
	return probabilities, distribution, nil
}

// mapSystemOneScore maps a score answer and keeps a selected level separate
// from a probability-weighted value.
func mapSystemOneScore(id lebro.DecisionQuestionID, question lebro.ScoreQuestion, raw systemOneAnswer, uncertainty lebro.DecisionUncertainty, profile systemOneProfile) (lebro.DecisionAnswer, error) {
	if raw.Type != "score" {
		return nil, fmt.Errorf("lebro: decision question %q expected a score answer, got %q", id, raw.Type)
	}
	weightedField := profile.scoreWeightedField
	winnerField := profile.scoreWinnerField
	probabilities, distribution, err := mapSystemOneLevelProbabilities(id, len(question.Criteria), raw.Probabilities)
	if err != nil {
		return nil, err
	}
	tolerance := distributionTolerance(profile.distributionTolerance, len(question.Criteria))
	if err := checkDistribution(id, uncertainty, distribution.present, distribution.total, tolerance); err != nil {
		return nil, err
	}
	legend, err := mapSystemOneLegend(id, len(question.Criteria), raw.Legend)
	if err != nil {
		return nil, err
	}
	answer := lebro.ScoreAnswer{
		Probabilities: probabilities,
		Legend:        legend,
		Provenance:    lebro.ScoreNative,
	}
	// The provider names its score fields, so the adapter reads the fields
	// the configuration names and requires the ones the provider must send.
	if winnerField != "" {
		winner, ok := raw.number(winnerField)
		if !ok {
			return nil, fmt.Errorf("lebro: decision question %q returned no %q winning level", id, winnerField)
		}
		level, err := strconv.Atoi(winner.String())
		if err != nil {
			return nil, fmt.Errorf("lebro: decision question %q returned a %q level that is not an integer", id, winnerField)
		}
		if level < 0 || level >= len(question.Criteria) {
			return nil, fmt.Errorf("lebro: decision question %q returned score level %d, outside the requested range", id, level)
		}
		answer.SelectedLevel = &level
	}
	weighted, ok := raw.number(weightedField)
	if !ok {
		return nil, fmt.Errorf("lebro: decision question %q returned no %q score field", id, weightedField)
	}
	expected, err := weighted.Float64()
	if err != nil {
		return nil, fmt.Errorf("lebro: decision question %q returned a %q score that is not a number", id, weightedField)
	}
	if expected < 0 || expected > float64(len(question.Criteria)-1) {
		return nil, fmt.Errorf("lebro: decision question %q returned a weighted score outside the requested range", id)
	}
	answer.ExpectedLevel = &expected
	confidence, metric, err := applyConfidence(id, uncertainty, raw.Confidence)
	if err != nil {
		return nil, err
	}
	answer.Confidence = confidence
	answer.ConfidenceMetric = metric
	return answer, nil
}

// mapSystemOneLevelProbabilities maps the provider level distribution onto
// the requested level indexes. A level the caller did not ask for is a
// malformed response, not a level to ignore.
func mapSystemOneLevelProbabilities(id lebro.DecisionQuestionID, levels int, reported map[string]float64) (map[int]float64, systemOneDistribution, error) {
	if levels <= 0 {
		return nil, systemOneDistribution{}, fmt.Errorf("lebro: decision question %q requested no levels", id)
	}
	if reported == nil {
		return nil, systemOneDistribution{}, nil
	}
	for key := range reported {
		level, err := strconv.Atoi(key)
		if err != nil {
			return nil, systemOneDistribution{}, fmt.Errorf("lebro: decision question %q returned a probability key that is not a level index", id)
		}
		if level < 0 || level >= levels {
			return nil, systemOneDistribution{}, fmt.Errorf("lebro: decision question %q returned probability for level %d, outside the requested range", id, level)
		}
	}
	probabilities := make(map[int]float64, levels)
	distribution := systemOneDistribution{present: true}
	for level := 0; level < levels; level++ {
		probability, ok := reported[strconv.Itoa(level)]
		if !ok {
			return nil, systemOneDistribution{}, fmt.Errorf("lebro: decision question %q returned no probability for level %d", id, level)
		}
		if err := checkProbability(probability); err != nil {
			return nil, systemOneDistribution{}, fmt.Errorf("lebro: decision question %q: %w", id, err)
		}
		probabilities[level] = probability
		distribution.total += probability
	}
	return probabilities, distribution, nil
}

// mapSystemOneLegend maps the provider legend onto the requested level
// indexes.
func mapSystemOneLegend(id lebro.DecisionQuestionID, levels int, raw map[string]json.RawMessage) (map[int]lebro.JSONValue, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	legend := make(map[int]lebro.JSONValue, len(raw))
	for key, value := range raw {
		level, err := strconv.Atoi(key)
		if err != nil {
			return nil, fmt.Errorf("lebro: decision question %q returned a legend key that is not a level index", id)
		}
		if level < 0 || level >= levels {
			return nil, fmt.Errorf("lebro: decision question %q returned legend level %d, outside the requested range", id, level)
		}
		var decoded lebro.JSONValue
		if err := json.Unmarshal(value, &decoded); err != nil {
			return nil, fmt.Errorf("lebro: decision question %q returned a legend entry that is not valid JSON", id)
		}
		legend[level] = decoded
	}
	return legend, nil
}

// checkProbability rejects a probability outside the unit interval.
func checkProbability(probability float64) error {
	if probability < 0 || probability > 1 {
		return fmt.Errorf("the provider returned the probability %v, which is outside the range 0 to 1", probability)
	}
	return nil
}

// buildSystemOneQuestions renders the shared question map. Instructions and
// criteria keep their JSON shape.
func buildSystemOneQuestions(questions map[lebro.DecisionQuestionID]lebro.DecisionQuestion) (map[string]any, error) {
	rendered := make(map[string]any, len(questions))
	for id, question := range questions {
		typedNoul, isNoul := question.(lebro.NoulQuestion)
		if pointer, ok := question.(*lebro.NoulQuestion); ok {
			if pointer == nil {
				return nil, fmt.Errorf("lebro: decision question %q must not be nil", id)
			}
			typedNoul, isNoul = *pointer, true
		}
		typedChoice, isChoice := question.(lebro.ChoiceQuestion)
		if pointer, ok := question.(*lebro.ChoiceQuestion); ok {
			if pointer == nil {
				return nil, fmt.Errorf("lebro: decision question %q must not be nil", id)
			}
			typedChoice, isChoice = *pointer, true
		}
		typedScore, isScore := question.(lebro.ScoreQuestion)
		if pointer, ok := question.(*lebro.ScoreQuestion); ok {
			if pointer == nil {
				return nil, fmt.Errorf("lebro: decision question %q must not be nil", id)
			}
			typedScore, isScore = *pointer, true
		}
		switch {
		case isNoul:
			typed := typedNoul
			wire := map[string]any{
				"type":         "noul",
				"instructions": typed.Instructions,
			}
			if typed.Criteria != nil {
				criteria := map[string]any{}
				if typed.Criteria.True != nil {
					criteria["true"] = typed.Criteria.True
				}
				if typed.Criteria.False != nil {
					criteria["false"] = typed.Criteria.False
				}
				if len(criteria) > 0 {
					wire["criteria"] = criteria
				}
			}
			rendered[string(id)] = wire
		case isChoice:
			typed := typedChoice
			criteria := make(map[string]any, len(typed.Criteria))
			for option, description := range typed.Criteria {
				criteria[string(option)] = description
			}
			rendered[string(id)] = map[string]any{
				"type":         "choice",
				"instructions": typed.Instructions,
				"criteria":     criteria,
			}
		case isScore:
			typed := typedScore
			criteria := make([]any, 0, len(typed.Criteria))
			for _, description := range typed.Criteria {
				criteria = append(criteria, description)
			}
			rendered[string(id)] = map[string]any{
				"type":         "score",
				"instructions": typed.Instructions,
				"criteria":     criteria,
			}
		default:
			return nil, fmt.Errorf("lebro: decision question %q has an unsupported type %T", id, question)
		}
	}
	return rendered, nil
}

// buildTextOnlyQuestions renders the question map for a provider that accepts
// text instructions and text criteria only. A structured value is rejected
// before the request is sent rather than flattened into something the
// provider never described.
func buildTextOnlyQuestions(questions map[lebro.DecisionQuestionID]lebro.DecisionQuestion) (map[string]any, error) {
	rendered := make(map[string]any, len(questions))
	for id, question := range questions {
		typedNoul, isNoul := question.(lebro.NoulQuestion)
		if pointer, ok := question.(*lebro.NoulQuestion); ok {
			if pointer == nil {
				return nil, fmt.Errorf("lebro: decision question %q must not be nil", id)
			}
			typedNoul, isNoul = *pointer, true
		}
		typedChoice, isChoice := question.(lebro.ChoiceQuestion)
		if pointer, ok := question.(*lebro.ChoiceQuestion); ok {
			if pointer == nil {
				return nil, fmt.Errorf("lebro: decision question %q must not be nil", id)
			}
			typedChoice, isChoice = *pointer, true
		}
		typedScore, isScore := question.(lebro.ScoreQuestion)
		if pointer, ok := question.(*lebro.ScoreQuestion); ok {
			if pointer == nil {
				return nil, fmt.Errorf("lebro: decision question %q must not be nil", id)
			}
			typedScore, isScore = *pointer, true
		}
		switch {
		case isNoul:
			typed := typedNoul
			instructions, err := textInstruction(id, typed.Instructions)
			if err != nil {
				return nil, err
			}
			wire := map[string]any{
				"type":         "noul",
				"instructions": instructions,
			}
			if typed.Criteria != nil {
				criteria := map[string]any{}
				if typed.Criteria.True != nil {
					text, err := textCriterion(id, "true", typed.Criteria.True)
					if err != nil {
						return nil, err
					}
					criteria["true"] = text
				}
				if typed.Criteria.False != nil {
					text, err := textCriterion(id, "false", typed.Criteria.False)
					if err != nil {
						return nil, err
					}
					criteria["false"] = text
				}
				if len(criteria) > 0 {
					wire["criteria"] = criteria
				}
			}
			rendered[string(id)] = wire
		case isChoice:
			typed := typedChoice
			instructions, err := textInstruction(id, typed.Instructions)
			if err != nil {
				return nil, err
			}
			criteria := make(map[string]any, len(typed.Criteria))
			for option, description := range typed.Criteria {
				text, err := textCriterion(id, string(option), description)
				if err != nil {
					return nil, err
				}
				criteria[string(option)] = text
			}
			rendered[string(id)] = map[string]any{
				"type":         "choice",
				"instructions": instructions,
				"criteria":     criteria,
			}
		case isScore:
			typed := typedScore
			instructions, err := textInstruction(id, typed.Instructions)
			if err != nil {
				return nil, err
			}
			criteria := make([]any, 0, len(typed.Criteria))
			for level, description := range typed.Criteria {
				text, err := textCriterion(id, strconv.Itoa(level), description)
				if err != nil {
					return nil, err
				}
				criteria = append(criteria, text)
			}
			rendered[string(id)] = map[string]any{
				"type":         "score",
				"instructions": instructions,
				"criteria":     criteria,
			}
		default:
			return nil, fmt.Errorf("lebro: decision question %q has an unsupported type %T", id, question)
		}
	}
	return rendered, nil
}

// textInstruction requires a non-empty text instruction.
func textInstruction(id lebro.DecisionQuestionID, instructions lebro.JSONValue) (string, error) {
	text, ok := instructions.(string)
	if !ok {
		return "", fmt.Errorf("lebro: decision question %q requires text instructions, and the provider accepts text only", id)
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("lebro: decision question %q requires non-empty instructions", id)
	}
	return text, nil
}

// textCriterion requires a text criterion.
func textCriterion(id lebro.DecisionQuestionID, name string, criterion lebro.JSONValue) (string, error) {
	text, ok := criterion.(string)
	if !ok {
		return "", fmt.Errorf("lebro: decision question %q requires text criteria, and the provider accepts text only", id)
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("lebro: decision question %q requires non-empty criteria", id)
	}
	return text, nil
}

// encodeImageBase64 encodes image bytes for a provider wire format. The
// encoding happens here, at the wire boundary, and the result is never
// retained.
func encodeImageBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}
