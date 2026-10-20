package decisions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tesh254/lebro"
)

const (
	// openAIProvider identifies the adapter in a response.
	openAIProvider = "openai"
	// openAIBaseURL is the public OpenAI API root.
	openAIBaseURL = "https://api.openai.com"
	// openAIPath is the OpenAI Decisions evaluation path.
	openAIPath = "/v1/decisions"
	// openAIModel is the model the Decisions API serves today.
	openAIModel = "gpt-6-luna"
	// openAIMaxImages is the largest number of images one request may carry.
	openAIMaxImages = 128
	// openAIMaxQuestions is the largest number of questions one request may
	// carry.
	openAIMaxQuestions = 200
	// openAIMaxChoiceOptions is the largest number of options one choice
	// question may carry.
	openAIMaxChoiceOptions = 255
	// openAIStateLabel introduces a structured state to the model. It is the
	// rendering policy: a state that is not text is written as JSON under
	// this label, so the model is told what it is reading.
	openAIStateLabel = "The state to evaluate, as JSON:"
	// openAICriteriaLabel introduces the rendered criteria of a binary
	// judgment to the model.
	//
	// The OpenAI Decisions schema has no field for the true and false
	// criteria of a binary judgment, so the adapter writes them into the
	// instructions under this label rather than dropping the caller's
	// meaning.
	openAICriteriaLabel = "The two outcomes, as JSON: "
)

// openAIRetryableStatuses are the HTTP status codes OpenAI documents as
// retryable: a rate limit and the server-side failures.
var openAIRetryableStatuses = []int{
	http.StatusTooManyRequests,
	http.StatusInternalServerError,
	http.StatusBadGateway,
	http.StatusServiceUnavailable,
	http.StatusGatewayTimeout,
}

// OpenAIConfig configures the OpenAI Decisions adapter. APIKey is required;
// every other field has a documented default.
type OpenAIConfig struct {
	// APIKey is the OpenAI credential. It is sent as a bearer token and is
	// never logged, returned, or included in an error.
	APIKey string
	// BaseURL is the API root. It defaults to the public OpenAI endpoint.
	BaseURL string
	// Model is the model used when a request does not name one. It defaults
	// to "gpt-6-luna", the only model the Decisions API serves today.
	Model string
	// HTTPClient issues requests. A client timeout bounds each attempt.
	HTTPClient *http.Client
	// Timeout bounds one attempt. It defaults to 60 seconds.
	Timeout time.Duration
	// MaxRetries is the number of extra attempts a transient failure gets. A
	// nil value uses the default of 2, and zero disables retries.
	MaxRetries *int
	// RetryBackoff is the first delay between attempts. It defaults to 500
	// milliseconds and doubles with every further attempt.
	RetryBackoff time.Duration
}

// OpenAIModel is a decision adapter for the OpenAI Decisions API.
//
// The OpenAI Decisions protocol is not the System One protocol these
// adapters share: its questions and answers are arrays rather than maps, its
// input is a message list rather than a shared state field, and it can
// refuse a question on its own. The adapter keeps its own request and
// response codec and shares the transport — the credential, the timeout, the
// bounded retries, the redirect refusal, and the failure taxonomy.
//
// The adapter is safe for concurrent use.
type OpenAIModel struct {
	client *decisionClient
	// capabilities describes what the OpenAI Decisions model supports.
	capabilities lebro.DecisionCapabilities
}

var _ lebro.DecisionModel = (*OpenAIModel)(nil)

// NewOpenAI builds an OpenAI Decisions adapter.
//
// The Decisions API is in public beta and currently serves one model.
func NewOpenAI(config OpenAIConfig) (*OpenAIModel, error) {
	baseURL := config.BaseURL
	if baseURL == "" {
		baseURL = openAIBaseURL
	}
	model := config.Model
	if model == "" {
		model = openAIModel
	}
	client, err := newDecisionClient(
		openAIProvider,
		baseURL,
		openAIPath,
		"Authorization",
		"Bearer ",
		config.APIKey,
		model,
		httpClientConfig{
			client:       config.HTTPClient,
			timeout:      config.Timeout,
			maxRetries:   config.MaxRetries,
			retryBackoff: config.RetryBackoff,
		},
	)
	if err != nil {
		return nil, err
	}
	return &OpenAIModel{
		client: client,
		capabilities: lebro.DecisionCapabilities{
			Noul:                   true,
			Choice:                 true,
			Score:                  true,
			MultipleQuestions:      true,
			StructuredState:        true,
			StructuredInstructions: false,
			Batching:               false,
			Images: lebro.DecisionImageCapabilities{
				Enabled:    true,
				MediaTypes: []string{"image/png", "image/jpeg", "image/webp"},
				MaxImages:  openAIMaxImages,
			},
			MaxQuestions:      openAIMaxQuestions,
			MaxChoiceOptions:  openAIMaxChoiceOptions,
			MaxScoreLevels:    0,
			MaxStateBytes:     0,
			RetryableStatuses: openAIRetryableStatuses,
			// A predicate answer carries a probability and nothing else, so
			// it has no distribution and no confidence.
			NoulUncertainty: lebro.DecisionUncertainty{
				Distribution: lebro.DecisionAvailabilityUnavailable,
				Confidence:   lebro.DecisionAvailabilityUnavailable,
				Refusal:      true,
			},
			// A choice or a score answer carries a distribution and a
			// confidence on every answer. OpenAI documents that it exposes
			// the confidence but not what the number means, so the metric
			// says exactly that.
			ChoiceUncertainty: lebro.DecisionUncertainty{
				Distribution:     lebro.DecisionAvailabilityRequired,
				Confidence:       lebro.DecisionAvailabilityRequired,
				ConfidenceMetric: lebro.DecisionConfidenceUnknown,
				Refusal:          true,
			},
			ScoreUncertainty: lebro.DecisionUncertainty{
				Distribution:     lebro.DecisionAvailabilityRequired,
				Confidence:       lebro.DecisionAvailabilityRequired,
				ConfidenceMetric: lebro.DecisionConfidenceUnknown,
				Refusal:          true,
			},
		},
	}, nil
}

// ProviderID identifies the adapter.
func (m *OpenAIModel) ProviderID() lebro.ProviderID {
	return lebro.ProviderID(openAIProvider)
}

// Capabilities describes what the configured model supports.
func (m *OpenAIModel) Capabilities() lebro.DecisionCapabilities {
	capabilities := m.capabilities
	capabilities.Images.MediaTypes = append([]string(nil), capabilities.Images.MediaTypes...)
	capabilities.RetryableStatuses = append([]int(nil), capabilities.RetryableStatuses...)
	return capabilities
}

// CapabilitiesFor describes what the configured model supports. An empty
// model name means the adapter's own model.
//
// The Decisions API serves one model, so a request that names another one
// has no profile and is rejected.
func (m *OpenAIModel) CapabilitiesFor(model string) (lebro.DecisionCapabilities, bool) {
	if model != "" && model != m.client.model {
		return lebro.DecisionCapabilities{}, false
	}
	return m.Capabilities(), true
}

// Decide evaluates the request with the OpenAI Decisions API and returns the
// typed answers.
func (m *OpenAIModel) Decide(ctx context.Context, request lebro.DecisionRequest) (lebro.DecisionResponse, error) {
	if err := lebro.ValidateDecisionImages(m.capabilities.Images, request.Images); err != nil {
		kind := lebro.DecisionErrorInvalidRequest
		if errors.Is(err, lebro.ErrDecisionUnsupportedCapability) {
			kind = lebro.DecisionErrorUnsupportedCapability
		}
		return lebro.DecisionResponse{}, m.client.failure(kind, err.Error(), err)
	}
	if err := request.Validate(); err != nil {
		return lebro.DecisionResponse{}, m.client.failure(lebro.DecisionErrorInvalidRequest, err.Error(), err)
	}
	if err := checkDecisionKinds(m.capabilities, openAIProvider, request); err != nil {
		return lebro.DecisionResponse{}, m.client.failure(lebro.DecisionErrorUnsupportedCapability, err.Error(), err)
	}
	if err := checkDecisionLimits(m.capabilities, openAIProvider, request); err != nil {
		return lebro.DecisionResponse{}, m.client.failure(lebro.DecisionErrorInvalidRequest, err.Error(), err)
	}

	model := request.Model
	if model == "" {
		model = m.client.defaultModel()
	}
	if model == "" {
		return lebro.DecisionResponse{}, m.client.failure(lebro.DecisionErrorInvalidRequest, "lebro: model is required", nil)
	}
	if _, ok := m.CapabilitiesFor(model); !ok {
		return lebro.DecisionResponse{}, m.client.failure(lebro.DecisionErrorUnsupportedCapability,
			fmt.Sprintf("lebro: the provider has no verified profile for model %q", model), nil)
	}

	questions, err := buildOpenAIQuestions(request.Questions)
	if err != nil {
		return lebro.DecisionResponse{}, m.client.failure(lebro.DecisionErrorInvalidRequest, err.Error(), err)
	}
	input, err := buildOpenAIInput(request)
	if err != nil {
		return lebro.DecisionResponse{}, m.client.failure(lebro.DecisionErrorInvalidRequest, err.Error(), err)
	}
	encoded, err := json.Marshal(openAIRequest{Model: model, Input: input, Questions: questions})
	if err != nil {
		return lebro.DecisionResponse{}, m.client.failure(lebro.DecisionErrorInvalidRequest, fmt.Sprintf("lebro: encode request: %v", err), err)
	}

	result, err := m.client.post(ctx, encoded, openAIRequestIDHeaders, retryableStatuses(m.capabilities.RetryableStatuses))
	if err != nil {
		return lebro.DecisionResponse{}, err
	}
	if result.status >= http.StatusBadRequest {
		// The redactor is built only here: a successful request must not
		// pay for a second copy of the request content.
		redactor := m.client.newRedactor(request, openAIRequestSecrets)
		return lebro.DecisionResponse{}, m.client.statusFailure(result.status, result.body, result.retryAfter, redactor)
	}

	decoded, err := decodeOpenAIResponse(result.body)
	if err != nil {
		return lebro.DecisionResponse{}, m.client.failure(lebro.DecisionErrorMalformedResponse, err.Error(), err)
	}
	if decoded.Model == "" {
		return lebro.DecisionResponse{}, m.client.failure(lebro.DecisionErrorMalformedResponse, "lebro: the decision response carries no model identity", nil)
	}
	answers, err := mapOpenAIAnswers(request, decoded.Answers)
	if err != nil {
		return lebro.DecisionResponse{}, m.client.failure(lebro.DecisionErrorMalformedResponse, err.Error(), err)
	}

	response := lebro.DecisionResponse{
		Provider:       lebro.ProviderID(openAIProvider),
		RequestedModel: model,
		Model:          decoded.Model,
		Answers:        answers,
		RequestID:      result.requestID,
		Duration:       result.duration,
		Attempts:       result.attempts,
	}
	if decoded.Usage != nil {
		usage := &lebro.DecisionUsage{}
		if decoded.Usage.InputTokens != nil {
			if *decoded.Usage.InputTokens < 0 {
				return lebro.DecisionResponse{}, m.client.failure(lebro.DecisionErrorMalformedResponse, "lebro: the provider reported a negative input token count", nil)
			}
			tokens := *decoded.Usage.InputTokens
			usage.InputTokens = &tokens
		}
		if decoded.Usage.OutputTokens != nil {
			if *decoded.Usage.OutputTokens < 0 {
				return lebro.DecisionResponse{}, m.client.failure(lebro.DecisionErrorMalformedResponse, "lebro: the provider reported a negative output token count", nil)
			}
			tokens := *decoded.Usage.OutputTokens
			usage.OutputTokens = &tokens
		}
		if decoded.Usage.TotalTokens != nil {
			if *decoded.Usage.TotalTokens < 0 {
				return lebro.DecisionResponse{}, m.client.failure(lebro.DecisionErrorMalformedResponse, "lebro: the provider reported a negative total token count", nil)
			}
			tokens := *decoded.Usage.TotalTokens
			usage.TotalTokens = &tokens
		}
		response.Usage = usage
	}
	return response, nil
}

// openAIRequestIDHeaders are the response headers that carry the provider
// request identifier.
var openAIRequestIDHeaders = []string{"x-request-id"}

// openAIRequest is the OpenAI Decisions request envelope.
type openAIRequest struct {
	// Model is the model that evaluates the request.
	Model string `json:"model"`
	// Input is the shared evidence: a text string, or the user messages
	// that carry the text and the images.
	Input any `json:"input"`
	// Questions are the named questions to ask.
	Questions []openAIQuestion `json:"questions"`
}

// openAIQuestion is one OpenAI Decisions question.
type openAIQuestion struct {
	// Type is the question kind: "predicate", "choice", or "score".
	Type string `json:"type"`
	// Name is the caller-owned question identifier. The provider echoes it
	// on the answer.
	Name string `json:"name"`
	// Instructions describes the question.
	Instructions string `json:"instructions"`
	// Choices are the options of a choice question.
	Choices []openAIChoice `json:"choices,omitempty"`
	// Levels are the ordered levels of a score question.
	Levels []openAILevel `json:"levels,omitempty"`
}

// openAIChoice is one option of a choice question.
type openAIChoice struct {
	// Value is the option identifier. The caller owns it, and the provider
	// echoes it on the answer.
	Value string `json:"value"`
	// Description explains the option.
	Description string `json:"description,omitempty"`
}

// openAILevel is one ordered level of a score question.
type openAILevel struct {
	// Label identifies the level. It is the level index, so the provider
	// echoes back exactly the identity the caller owns.
	Label string `json:"label"`
	// Description explains the level.
	Description string `json:"description,omitempty"`
}

// openAIResponse is the OpenAI Decisions response envelope.
type openAIResponse struct {
	// Answers are the answers, one per question.
	Answers []openAIAnswer `json:"answers"`
	// Model is the model that answered.
	Model string `json:"model"`
	// Usage is the reported token usage.
	Usage *openAIUsage `json:"usage"`
}

// openAIUsage is the OpenAI Decisions usage envelope.
type openAIUsage struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
	TotalTokens  *int64 `json:"total_tokens"`
}

// openAIAnswer is the OpenAI Decisions answer shape. The provider fills the
// fields its answer kind defines and leaves the rest unset.
type openAIAnswer struct {
	// Type is the answer kind: "predicate", "choice", "score", or
	// "refusal".
	Type string `json:"type"`
	// Name is the question the answer belongs to.
	Name string `json:"name"`
	// Probability is the estimate that a predicate holds.
	Probability *float64 `json:"probability"`
	// Choice is the selected option. It is a string or a boolean; the
	// adapter sends strings and expects one back.
	Choice *json.RawMessage `json:"choice"`
	// Confidence is the provider's certainty.
	Confidence *float64 `json:"confidence"`
	// Probabilities is the distribution over the options or the levels.
	Probabilities []openAIProbability `json:"probabilities"`
	// Score is the probability-weighted level index of a score answer.
	Score *float64 `json:"score"`
	// Reason is the provider's explanation of a refusal, when it gives one.
	// The Decisions API documents none today.
	Reason string `json:"reason"`
}

// openAIProbability is one entry of a reported distribution. A choice entry
// carries the option as its value; a score entry carries the level index and
// the level's label.
type openAIProbability struct {
	Value json.RawMessage `json:"value"`
	// Probability is a pointer because an entry without one is malformed
	// and an entry with a zero is a real probability.
	Probability *float64 `json:"probability"`
	Label       string   `json:"label"`
}

// decodeOpenAIResponse decodes the OpenAI Decisions response envelope.
func decodeOpenAIResponse(body []byte) (*openAIResponse, error) {
	var decoded openAIResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("lebro: decode the decision response: %w", err)
	}
	if decoded.Answers == nil {
		return nil, errors.New("lebro: the decision response carries no answers")
	}
	return &decoded, nil
}

// buildOpenAIQuestions renders the questions in a stable order.
//
// The questions go out as an array, so the adapter sorts the identifiers
// before it sends them: the wire order is the adapter's business, and the
// answer carries the name back.
func buildOpenAIQuestions(questions map[lebro.DecisionQuestionID]lebro.DecisionQuestion) ([]openAIQuestion, error) {
	identifiers := make([]string, 0, len(questions))
	for id := range questions {
		identifiers = append(identifiers, string(id))
	}
	sort.Strings(identifiers)
	rendered := make([]openAIQuestion, 0, len(identifiers))
	for _, identifier := range identifiers {
		id := lebro.DecisionQuestionID(identifier)
		question := questions[id]
		switch typed := question.(type) {
		case lebro.NoulQuestion:
			instructions, err := openAIInstructions(id, typed.Instructions, typed.Criteria)
			if err != nil {
				return nil, err
			}
			rendered = append(rendered, openAIQuestion{Type: "predicate", Name: identifier, Instructions: instructions})
		case *lebro.NoulQuestion:
			if typed == nil {
				return nil, fmt.Errorf("lebro: decision question %q must not be nil", id)
			}
			instructions, err := openAIInstructions(id, typed.Instructions, typed.Criteria)
			if err != nil {
				return nil, err
			}
			rendered = append(rendered, openAIQuestion{Type: "predicate", Name: identifier, Instructions: instructions})
		case lebro.ChoiceQuestion:
			instructions, err := openAIText(id, "instructions", typed.Instructions)
			if err != nil {
				return nil, err
			}
			choices, err := openAIChoices(id, typed.Criteria)
			if err != nil {
				return nil, err
			}
			rendered = append(rendered, openAIQuestion{Type: "choice", Name: identifier, Instructions: instructions, Choices: choices})
		case *lebro.ChoiceQuestion:
			if typed == nil {
				return nil, fmt.Errorf("lebro: decision question %q must not be nil", id)
			}
			instructions, err := openAIText(id, "instructions", typed.Instructions)
			if err != nil {
				return nil, err
			}
			choices, err := openAIChoices(id, typed.Criteria)
			if err != nil {
				return nil, err
			}
			rendered = append(rendered, openAIQuestion{Type: "choice", Name: identifier, Instructions: instructions, Choices: choices})
		case lebro.ScoreQuestion:
			instructions, err := openAIText(id, "instructions", typed.Instructions)
			if err != nil {
				return nil, err
			}
			levels, err := openAILevels(id, typed.Criteria)
			if err != nil {
				return nil, err
			}
			rendered = append(rendered, openAIQuestion{Type: "score", Name: identifier, Instructions: instructions, Levels: levels})
		case *lebro.ScoreQuestion:
			if typed == nil {
				return nil, fmt.Errorf("lebro: decision question %q must not be nil", id)
			}
			instructions, err := openAIText(id, "instructions", typed.Instructions)
			if err != nil {
				return nil, err
			}
			levels, err := openAILevels(id, typed.Criteria)
			if err != nil {
				return nil, err
			}
			rendered = append(rendered, openAIQuestion{Type: "score", Name: identifier, Instructions: instructions, Levels: levels})
		default:
			return nil, fmt.Errorf("lebro: decision question %q has an unsupported type %T", id, question)
		}
	}
	return rendered, nil
}

// openAIInstructions renders the instructions of a binary judgment.
//
// The OpenAI Decisions schema has one instruction and no place for the true
// and false criteria, so the criteria are written into the instructions
// under a fixed label. The criteria are the caller's meaning, and dropping
// them would change the question.
func openAIInstructions(id lebro.DecisionQuestionID, instructions lebro.JSONValue, criteria *lebro.NoulCriteria) (string, error) {
	text, err := openAIText(id, "instructions", instructions)
	if err != nil {
		return "", err
	}
	if criteria == nil {
		return text, nil
	}
	yes, err := openAIText(id, "criteria true", criteria.True)
	if err != nil {
		return "", err
	}
	no, err := openAIText(id, "criteria false", criteria.False)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(map[string]string{"yes": yes, "no": no})
	if err != nil {
		return "", fmt.Errorf("lebro: encode the criteria: %w", err)
	}
	return text + "\n\n" + openAICriteriaLabel + string(encoded), nil
}

// openAIChoices renders the options of a choice question in a stable order.
func openAIChoices(id lebro.DecisionQuestionID, criteria map[lebro.DecisionOptionID]lebro.JSONValue) ([]openAIChoice, error) {
	options := make([]string, 0, len(criteria))
	for option := range criteria {
		options = append(options, string(option))
	}
	sort.Strings(options)
	choices := make([]openAIChoice, 0, len(options))
	for _, option := range options {
		description, err := openAIText(id, "option "+option, criteria[lebro.DecisionOptionID(option)])
		if err != nil {
			return nil, err
		}
		choices = append(choices, openAIChoice{Value: option, Description: description})
	}
	return choices, nil
}

// openAILevels renders the ordered levels of a score question.
//
// The label is the level index, so the provider echoes back exactly the
// identity the caller owns and the answer maps back onto the caller's
// levels without guessing.
func openAILevels(id lebro.DecisionQuestionID, criteria []lebro.JSONValue) ([]openAILevel, error) {
	levels := make([]openAILevel, 0, len(criteria))
	for index, description := range criteria {
		text, err := openAIText(id, "level "+strconv.Itoa(index), description)
		if err != nil {
			return nil, err
		}
		levels = append(levels, openAILevel{Label: strconv.Itoa(index), Description: text})
	}
	return levels, nil
}

// openAIText requires a text value.
//
// The OpenAI Decisions schema takes text instructions and text criteria, so
// a structured value is rejected before the request is sent rather than
// flattened into something the provider never described.
func openAIText(id lebro.DecisionQuestionID, name string, value lebro.JSONValue) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("lebro: decision question %q requires text %s, and the provider accepts text only", id, name)
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("lebro: decision question %q requires non-empty %s", id, name)
	}
	return text, nil
}

// buildOpenAIInput renders the shared evidence.
//
// The state is text when the caller supplies text. A structured state is
// written as JSON under a fixed label, so the model is told what it reads.
// The images become inline data URLs in the same message, because the
// Decisions endpoint takes no remote image reference.
func buildOpenAIInput(request lebro.DecisionRequest) (any, error) {
	text, err := openAIStateText(request.State)
	if err != nil {
		return nil, err
	}
	if len(request.Images) == 0 {
		return text, nil
	}
	parts := make([]any, 0, len(request.Images)+1)
	parts = append(parts, map[string]any{"type": "input_text", "text": text})
	for _, image := range request.Images {
		parts = append(parts, map[string]any{
			"type":      "input_image",
			"image_url": "data:" + strings.ToLower(image.MediaType) + ";base64," + encodeImageBase64(image.Bytes),
		})
	}
	return []any{map[string]any{"role": "user", "content": parts}}, nil
}

// openAIStateText renders the state as the text the provider reads.
func openAIStateText(state lebro.JSONValue) (string, error) {
	if text, ok := state.(string); ok {
		return text, nil
	}
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return "", fmt.Errorf("lebro: encode the state: %w", err)
	}
	return openAIStateLabel + "\n" + string(encoded), nil
}

// openAIRequestSecrets returns the request content that must never reach an
// error message.
//
// The rendered state and the rendered instructions are listed because they
// are what the provider read: a provider that echoes the request back
// echoes the rendering, not the caller's original value. Each one is listed
// twice, because a provider that embeds the request in an error embeds it
// in a JSON string, and the escaped form is not the form that was sent.
func openAIRequestSecrets(request lebro.DecisionRequest) []string {
	secrets := make([]string, 0, 2*len(request.Images)+2*len(request.Questions)+2)
	if text, err := openAIStateText(request.State); err == nil && text != "" {
		secrets = append(secrets, text, escapedSecret(text))
	}
	for _, question := range request.Questions {
		var instructions lebro.JSONValue
		var criteria *lebro.NoulCriteria
		switch typed := question.(type) {
		case lebro.NoulQuestion:
			instructions, criteria = typed.Instructions, typed.Criteria
		case *lebro.NoulQuestion:
			if typed == nil {
				continue
			}
			instructions, criteria = typed.Instructions, typed.Criteria
		default:
			continue
		}
		rendered, err := openAIInstructions(lebro.DecisionQuestionID(""), instructions, criteria)
		if err != nil || rendered == "" {
			continue
		}
		secrets = append(secrets, rendered, escapedSecret(rendered))
	}
	for _, image := range request.Images {
		secrets = append(secrets, encodeImageBase64(image.Bytes))
	}
	return secrets
}

// escapedSecret returns the JSON-escaped form of a rendered value, without
// the surrounding quotes.
//
// A provider that echoes the request back embeds the rendered value in a
// JSON string, so what it echoes is the escaped form and not the form that
// was sent.
func escapedSecret(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded[1 : len(encoded)-1])
}

// mapOpenAIAnswers maps the provider answers onto the requested questions.
//
// The answers carry the question's name, so the mapping is by name and not
// by position: a duplicate, an unknown, or a missing name is a response the
// adapter cannot place.
func mapOpenAIAnswers(request lebro.DecisionRequest, wire []openAIAnswer) (map[lebro.DecisionQuestionID]lebro.DecisionAnswer, error) {
	answers := make(map[lebro.DecisionQuestionID]lebro.DecisionAnswer, len(request.Questions))
	for _, answer := range wire {
		if answer.Name == "" {
			return nil, errors.New("lebro: the provider returned an answer with no name")
		}
		id := lebro.DecisionQuestionID(answer.Name)
		if _, duplicate := answers[id]; duplicate {
			return nil, fmt.Errorf("lebro: the provider returned two answers for question %q", answer.Name)
		}
		question, ok := request.Questions[id]
		if !ok {
			return nil, fmt.Errorf("lebro: the provider returned an answer for the unknown question %q", answer.Name)
		}
		mapped, err := mapOpenAIAnswer(id, question, answer)
		if err != nil {
			return nil, err
		}
		answers[id] = mapped
	}
	for id := range request.Questions {
		if _, ok := answers[id]; !ok {
			return nil, fmt.Errorf("lebro: the provider returned no answer for question %q", id)
		}
	}
	return answers, nil
}

// mapOpenAIAnswer maps one provider answer onto the neutral contract.
func mapOpenAIAnswer(id lebro.DecisionQuestionID, question lebro.DecisionQuestion, raw openAIAnswer) (lebro.DecisionAnswer, error) {
	if raw.Type == "refusal" {
		// A refusal is the provider declining the question. It is not a
		// false answer and it does not fail the sibling answers.
		return lebro.RefusalAnswer{Reason: raw.Reason}, nil
	}
	switch typed := question.(type) {
	case lebro.NoulQuestion:
		return mapOpenAIPredicate(id, raw)
	case *lebro.NoulQuestion:
		if typed == nil {
			return nil, fmt.Errorf("lebro: decision question %q must not be nil", id)
		}
		return mapOpenAIPredicate(id, raw)
	case lebro.ChoiceQuestion:
		return mapOpenAIChoice(id, typed, raw)
	case *lebro.ChoiceQuestion:
		if typed == nil {
			return nil, fmt.Errorf("lebro: decision question %q must not be nil", id)
		}
		return mapOpenAIChoice(id, *typed, raw)
	case lebro.ScoreQuestion:
		return mapOpenAIScore(id, typed, raw)
	case *lebro.ScoreQuestion:
		if typed == nil {
			return nil, fmt.Errorf("lebro: decision question %q must not be nil", id)
		}
		return mapOpenAIScore(id, *typed, raw)
	default:
		return nil, fmt.Errorf("lebro: decision question %q has an unsupported type %T", id, question)
	}
}

// mapOpenAIPredicate maps a predicate answer.
//
// A predicate carries a probability and nothing else: the probability
// already is the uncertainty, so there is no confidence to report.
func mapOpenAIPredicate(id lebro.DecisionQuestionID, raw openAIAnswer) (lebro.DecisionAnswer, error) {
	if raw.Type != "predicate" {
		return nil, fmt.Errorf("lebro: decision question %q expected a predicate answer, got %q", id, raw.Type)
	}
	if raw.Probability == nil {
		return nil, fmt.Errorf("lebro: decision question %q returned no probability", id)
	}
	if err := checkProbability(*raw.Probability); err != nil {
		return nil, fmt.Errorf("lebro: decision question %q: %w", id, err)
	}
	if raw.Confidence != nil {
		// A predicate answer carries a probability and nothing else: the
		// probability already is the uncertainty.
		return nil, fmt.Errorf("lebro: decision question %q returned a confidence the provider does not report", id)
	}
	return lebro.NoulAnswer{ProbabilityYes: *raw.Probability}, nil
}

// mapOpenAIChoice maps a choice answer.
func mapOpenAIChoice(id lebro.DecisionQuestionID, question lebro.ChoiceQuestion, raw openAIAnswer) (lebro.DecisionAnswer, error) {
	if raw.Type != "choice" {
		return nil, fmt.Errorf("lebro: decision question %q expected a choice answer, got %q", id, raw.Type)
	}
	if raw.Choice == nil {
		return nil, fmt.Errorf("lebro: decision question %q returned no selected option", id)
	}
	var selected string
	if err := json.Unmarshal(*raw.Choice, &selected); err != nil {
		return nil, fmt.Errorf("lebro: decision question %q returned a selected option that is not a string", id)
	}
	if selected == "" {
		return nil, fmt.Errorf("lebro: decision question %q returned an empty selected option", id)
	}
	option := lebro.DecisionOptionID(selected)
	if _, ok := question.Criteria[option]; !ok {
		return nil, fmt.Errorf("lebro: decision question %q selected option %q, which was not requested", id, option)
	}
	probabilities, err := mapOpenAIChoiceProbabilities(id, question.Criteria, raw.Probabilities)
	if err != nil {
		return nil, err
	}
	confidence, err := openAIConfidence(id, raw.Confidence)
	if err != nil {
		return nil, err
	}
	return lebro.ChoiceAnswer{
		Choice:           option,
		Probabilities:    probabilities,
		Confidence:       confidence,
		ConfidenceMetric: lebro.DecisionConfidenceUnknown,
	}, nil
}

// mapOpenAIChoiceProbabilities maps the reported distribution onto the
// requested options.
//
// Every requested option must carry a probability: a partial distribution is
// a malformed response, not a distribution with implicit zeros.
func mapOpenAIChoiceProbabilities(id lebro.DecisionQuestionID, requested map[lebro.DecisionOptionID]lebro.JSONValue, reported []openAIProbability) (map[lebro.DecisionOptionID]float64, error) {
	if len(requested) == 0 {
		return nil, fmt.Errorf("lebro: decision question %q requested no options", id)
	}
	probabilities := make(map[lebro.DecisionOptionID]float64, len(requested))
	total := 0.0
	for _, entry := range reported {
		var option string
		if err := json.Unmarshal(entry.Value, &option); err != nil {
			return nil, fmt.Errorf("lebro: decision question %q returned a distribution entry that is not an option", id)
		}
		if _, ok := requested[lebro.DecisionOptionID(option)]; !ok {
			return nil, fmt.Errorf("lebro: decision question %q returned a probability for option %q, which was not requested", id, option)
		}
		if entry.Probability == nil {
			return nil, fmt.Errorf("lebro: decision question %q returned a distribution entry with no probability", id)
		}
		if err := checkProbability(*entry.Probability); err != nil {
			return nil, fmt.Errorf("lebro: decision question %q: %w", id, err)
		}
		if _, duplicate := probabilities[lebro.DecisionOptionID(option)]; duplicate {
			return nil, fmt.Errorf("lebro: decision question %q returned two probabilities for option %q", id, option)
		}
		probabilities[lebro.DecisionOptionID(option)] = *entry.Probability
		total += *entry.Probability
	}
	for option := range requested {
		if _, ok := probabilities[option]; !ok {
			return nil, fmt.Errorf("lebro: decision question %q returned no probability for option %q", id, option)
		}
	}
	if err := checkOpenAIDistribution(id, total, len(requested)); err != nil {
		return nil, err
	}
	return probabilities, nil
}

// mapOpenAIScore maps a score answer.
//
// The provider reports the probability-weighted level and no winning level,
// so the selected level stays unset: an argmax would be a value the provider
// did not report.
func mapOpenAIScore(id lebro.DecisionQuestionID, question lebro.ScoreQuestion, raw openAIAnswer) (lebro.DecisionAnswer, error) {
	if raw.Type != "score" {
		return nil, fmt.Errorf("lebro: decision question %q expected a score answer, got %q", id, raw.Type)
	}
	if raw.Score == nil {
		return nil, fmt.Errorf("lebro: decision question %q returned no score", id)
	}
	weighted := *raw.Score
	if weighted < 0 || weighted > float64(len(question.Criteria)-1) {
		return nil, fmt.Errorf("lebro: decision question %q returned a weighted score outside the requested range", id)
	}
	probabilities, err := mapOpenAIScoreProbabilities(id, len(question.Criteria), raw.Probabilities)
	if err != nil {
		return nil, err
	}
	confidence, err := openAIConfidence(id, raw.Confidence)
	if err != nil {
		return nil, err
	}
	return lebro.ScoreAnswer{
		ExpectedLevel:    &weighted,
		Probabilities:    probabilities,
		Confidence:       confidence,
		ConfidenceMetric: lebro.DecisionConfidenceUnknown,
		Provenance:       lebro.ScoreNative,
	}, nil
}

// mapOpenAIScoreProbabilities maps the reported distribution onto the
// requested levels.
func mapOpenAIScoreProbabilities(id lebro.DecisionQuestionID, levels int, reported []openAIProbability) (map[int]float64, error) {
	if levels <= 0 {
		return nil, fmt.Errorf("lebro: decision question %q requested no levels", id)
	}
	probabilities := make(map[int]float64, levels)
	total := 0.0
	for _, entry := range reported {
		var level int
		// A null value unmarshals into a zero without an error, so it is
		// rejected explicitly.
		if err := json.Unmarshal(entry.Value, &level); err != nil || string(entry.Value) == "null" {
			return nil, fmt.Errorf("lebro: decision question %q returned a distribution entry that is not a level index", id)
		}
		if level < 0 || level >= levels {
			return nil, fmt.Errorf("lebro: decision question %q returned probability for level %d, outside the requested range", id, level)
		}
		if entry.Probability == nil {
			return nil, fmt.Errorf("lebro: decision question %q returned a distribution entry with no probability", id)
		}
		if err := checkProbability(*entry.Probability); err != nil {
			return nil, fmt.Errorf("lebro: decision question %q: %w", id, err)
		}
		if _, duplicate := probabilities[level]; duplicate {
			return nil, fmt.Errorf("lebro: decision question %q returned two probabilities for level %d", id, level)
		}
		probabilities[level] = *entry.Probability
		total += *entry.Probability
	}
	for level := 0; level < levels; level++ {
		if _, ok := probabilities[level]; !ok {
			return nil, fmt.Errorf("lebro: decision question %q returned no probability for level %d", id, level)
		}
	}
	if err := checkOpenAIDistribution(id, total, levels); err != nil {
		return nil, err
	}
	return probabilities, nil
}

// openAIConfidence reads the reported confidence.
//
// The Decisions API exposes a confidence on a choice and a score answer and
// does not document what the number means, so the metric stays unknown.
func openAIConfidence(id lebro.DecisionQuestionID, reported *float64) (*float64, error) {
	if reported == nil {
		return nil, fmt.Errorf("lebro: decision question %q returned no confidence", id)
	}
	if err := checkProbability(*reported); err != nil {
		return nil, fmt.Errorf("lebro: decision question %q: %w", id, err)
	}
	confidence := *reported
	return &confidence, nil
}

// checkOpenAIDistribution checks that a reported distribution adds up to
// one.
func checkOpenAIDistribution(id lebro.DecisionQuestionID, total float64, outcomes int) error {
	tolerance := distributionTolerance(0, outcomes)
	if difference := total - 1; difference > tolerance || difference < -tolerance {
		return fmt.Errorf("lebro: decision question %q returned a probability distribution that adds up to %v instead of one", id, total)
	}
	return nil
}
