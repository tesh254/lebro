package decisions

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tesh254/lebro"
)

const (
	// defaultSystemOneProviderID identifies an adapter that was configured
	// without a provider name.
	defaultSystemOneProviderID = "systemone"
	// defaultScoreWeightedField is the field a provider uses for the
	// probability-weighted level when the configuration does not name one.
	defaultScoreWeightedField = "score"
)

// SystemOneScoreFields names the wire fields that hold a score answer.
//
// A provider reports the probability-weighted level in one field, and some
// providers also report the winning level in another. The names belong to
// the provider, so they are configuration:
//
//	TypeSafe and SubQ: {Weighted: "score"}
//	Fastino:          {Weighted: "expected_level", Winner: "score"}
type SystemOneScoreFields struct {
	// Weighted names the field that holds the probability-weighted level,
	// for example "score" or "expected_level". It defaults to "score".
	Weighted string
	// Winner names the field that holds the winning level index, for example
	// "score". It is empty when the provider reports no winning level.
	Winner string
}

// SystemOneImageStyle names the wire shape a provider uses for image inputs.
//
// The shape is the provider's, so it is configuration. A shape this package
// does not implement leaves image inputs disabled, and a request that
// carries an image fails locally instead of reaching the provider.
type SystemOneImageStyle string

const (
	// SystemOneImagesNone means the provider takes no image inputs.
	SystemOneImagesNone SystemOneImageStyle = "none"
	// SystemOneImagesInline sends the images in a top-level "images" array
	// of {mediaType, base64} objects, alongside the state.
	SystemOneImagesInline SystemOneImageStyle = "inline"
)

// SystemOneModelProfile binds a provider model to the contract it was
// verified for.
//
// A provider can serve several models, and the models do not necessarily
// agree on their limits, on the fields their score answers use, or on the
// uncertainty they report. A profile says which contract a model was
// verified against, so an adapter never answers one model under another
// model's rules.
type SystemOneModelProfile struct {
	// Model is the provider model identifier. It is required.
	Model string
	// Score names the wire fields that hold a score answer for this model.
	Score SystemOneScoreFields
	// Capabilities describes what this model supports.
	Capabilities lebro.DecisionCapabilities
}

// SystemOneConfig describes a System One provider's wire contract.
//
// Every difference between the providers this package adapts is data here:
// the endpoint, the credential placement, the model, the score field names,
// the documented limits, and the retryable statuses. A provider this package
// ships no named adapter for is reachable by configuration alone.
//
// The safe defaults are the restrictive ones: instructions and criteria are
// text only, image inputs are disabled, and the provider is unnamed.
type SystemOneConfig struct {
	// APIKey is the provider credential. It is required.
	APIKey string
	// BaseURL is the API root, for example "https://api.fastino.ai".
	BaseURL string
	// Path is the evaluation path. It defaults to "/v1/systemone".
	Path string
	// AuthHeader carries the credential, for example "Authorization" or
	// "X-API-Key". It defaults to "Authorization".
	AuthHeader string
	// AuthPrefix precedes the credential in the header, for example
	// "Bearer ". It is empty when the header carries the credential alone.
	AuthPrefix string
	// ProviderID identifies the adapter in a response. It defaults to
	// "systemone".
	ProviderID lebro.ProviderID
	// Model is the model used when a request does not name one.
	Model string
	// Score names the wire fields that hold a score answer.
	Score SystemOneScoreFields
	// Capabilities describes what the provider supports. Its Images field
	// carries the image limits, StructuredInstructions selects whether
	// instructions and criteria may be structured, MaxStateBytes is checked
	// before a request is sent, and RetryableStatuses are the HTTP statuses
	// the provider documents as retryable. The uncertainty of each question
	// kind is described here too, because a provider can report a
	// confidence for one kind and none for another.
	Capabilities lebro.DecisionCapabilities
	// Profiles lists the models the adapter may serve besides the one this
	// configuration names. A request that names a model with no profile is
	// rejected: the adapter has no verified contract for that model, and
	// answering it under the default model's rules would advertise
	// capabilities it does not have.
	//
	// A profile that names the same model as this configuration replaces
	// that model's capabilities.
	Profiles []SystemOneModelProfile
	// DistributionTolerance is the largest deviation a reported probability
	// distribution may have from a total of one. It covers a provider that
	// rounds its probabilities. Zero uses the default.
	DistributionTolerance float64
	// ImageStyle is the wire shape the provider uses for image inputs. A
	// shape this package does not implement leaves image inputs disabled.
	ImageStyle SystemOneImageStyle
	// MaxBodyBytes is the largest request body the provider accepts. Zero
	// means the provider documents no byte limit.
	MaxBodyBytes int64
	// RequestIDHeaders are the response headers that carry the provider
	// request identifier. The first header that carries a value wins.
	RequestIDHeaders []string
	// DisableStorage sends the provider's storage opt-out, the "store" field
	// set to false, for the providers that document one.
	DisableStorage bool
	// HTTPClient issues requests. A client timeout bounds each attempt.
	HTTPClient *http.Client
	// Timeout bounds one attempt. It defaults to 60 seconds.
	Timeout time.Duration
	// MaxRetries is the number of extra attempts a transient failure gets. A
	// nil value uses the default of 2, and zero disables retries.
	MaxRetries *int
	// RetryBackoff is the first delay between attempts. It defaults to 500
	// milliseconds.
	RetryBackoff time.Duration
}

// SystemOneModel is a decision adapter for a System One provider.
//
// It is the generic form of the named adapters: the provider's endpoint,
// credential placement, model, score fields, capabilities, and retry policy
// are all configuration. The adapter is safe for concurrent use.
type SystemOneModel struct {
	client *decisionClient
	// profile is the contract of the adapter's default model.
	profile systemOneProfile
	// profiles holds every model the adapter may serve, keyed by model
	// identifier.
	profiles map[string]systemOneProfile
}

var _ lebro.DecisionModel = (*SystemOneModel)(nil)

// NewSystemOne builds a decision adapter for a System One provider from its
// wire contract.
//
// The named constructors in this package fill in the documented contract for
// the provider they adapt. Use this one for a provider that is not named
// here, or for a gateway that fronts one.
func NewSystemOne(config SystemOneConfig) (*SystemOneModel, error) {
	profile, err := config.profile()
	if err != nil {
		return nil, err
	}
	profiles, err := config.profiles(profile)
	if err != nil {
		return nil, err
	}
	baseURL := config.BaseURL
	if baseURL == "" {
		return nil, errors.New("lebro: a System One provider needs a base URL")
	}
	// The base URL is never repeated in an error message: it can carry a
	// credential in its userinfo or its query.
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("lebro: the base URL is not a valid URL: %w", errRedirectSafe(err))
	}
	if !parsed.IsAbs() {
		return nil, errors.New("lebro: the base URL must be absolute")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("lebro: the base URL must use http or https")
	}
	if parsed.Host == "" {
		return nil, errors.New("lebro: the base URL must name a host")
	}
	path := config.Path
	if path == "" {
		path = systemOnePath
	}
	if !strings.HasPrefix(path, "/") {
		return nil, errors.New("lebro: a System One path must start with /")
	}
	authHeader := config.AuthHeader
	if authHeader == "" {
		authHeader = "Authorization"
	}
	client, err := newDecisionClient(
		string(profile.provider),
		baseURL,
		path,
		authHeader,
		config.AuthPrefix,
		config.APIKey,
		config.Model,
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
	return &SystemOneModel{client: client, profile: profile, profiles: profiles}, nil
}

// ProviderID identifies the adapter.
func (m *SystemOneModel) ProviderID() lebro.ProviderID {
	return lebro.ProviderID(m.profile.provider)
}

// Capabilities describes what the configured provider supports for its
// default model.
//
// The returned value carries copies of the lists, so a caller cannot change
// the capabilities every later call reports.
func (m *SystemOneModel) Capabilities() lebro.DecisionCapabilities {
	capabilities, _ := m.CapabilitiesFor("")
	return capabilities
}

// CapabilitiesFor describes what the configured provider supports for a
// named model. An empty model name means the adapter's default model.
//
// It reports false for a model the adapter has no verified profile for.
func (m *SystemOneModel) CapabilitiesFor(model string) (lebro.DecisionCapabilities, bool) {
	profile, ok := m.profileFor(model)
	if !ok {
		return lebro.DecisionCapabilities{}, false
	}
	capabilities := profile.capabilities
	capabilities.Images.MediaTypes = append([]string(nil), capabilities.Images.MediaTypes...)
	capabilities.RetryableStatuses = append([]int(nil), capabilities.RetryableStatuses...)
	return capabilities, true
}

// profileFor resolves the contract of a model.
//
// An empty model name means the adapter's default model. A named model needs
// a verified profile: the adapter never answers one model under another
// model's capabilities.
func (m *SystemOneModel) profileFor(model string) (systemOneProfile, bool) {
	if model == "" {
		// An empty model name means the adapter's own model, and a profile
		// may replace that model's contract. The same model must not get
		// different capabilities depending on whether the caller spells
		// its name.
		if profile, ok := m.profiles[m.profile.model]; ok {
			return profile, true
		}
		return m.profile, true
	}
	if profile, ok := m.profiles[model]; ok {
		return profile, true
	}
	return systemOneProfile{}, false
}

// Decide evaluates the request with the configured provider and returns the
// typed answers.
func (m *SystemOneModel) Decide(ctx context.Context, request lebro.DecisionRequest) (lebro.DecisionResponse, error) {
	profile, ok := m.profileFor(request.Model)
	if !ok {
		return lebro.DecisionResponse{}, m.client.failure(lebro.DecisionErrorUnsupportedCapability,
			fmt.Sprintf("lebro: the provider has no verified profile for model %q", request.Model), nil)
	}
	return decide(ctx, m.client, profile, request)
}

// profile turns the configuration into the internal provider contract.
func (c SystemOneConfig) profile() (systemOneProfile, error) {
	provider := string(c.ProviderID)
	if provider == "" {
		provider = defaultSystemOneProviderID
	}
	capabilities := c.Capabilities
	if err := capabilities.Validate(); err != nil {
		return systemOneProfile{}, err
	}
	if err := checkSystemOneCapabilities(capabilities); err != nil {
		return systemOneProfile{}, err
	}
	tolerance, err := c.tolerance()
	if err != nil {
		return systemOneProfile{}, err
	}
	profile := systemOneProfile{
		provider:              provider,
		model:                 c.Model,
		capabilities:          c.capabilities(),
		scoreWeightedField:    c.Score.Weighted,
		scoreWinnerField:      c.Score.Winner,
		distributionTolerance: tolerance,
		buildQuestions:        questionBuilder(capabilities),
		extraFields: func(lebro.DecisionRequest) map[string]any {
			if !c.DisableStorage {
				return nil
			}
			return map[string]any{"store": false}
		},
		maxBodyBytes:     c.MaxBodyBytes,
		requestIDHeaders: append([]string(nil), c.RequestIDHeaders...),
	}
	if profile.scoreWeightedField == "" {
		profile.scoreWeightedField = defaultScoreWeightedField
	}
	if profile.scoreWinnerField != "" && profile.scoreWinnerField == profile.scoreWeightedField {
		return systemOneProfile{}, errors.New("lebro: the score weighted and winner fields must differ")
	}
	if profile.capabilities.Images.Enabled {
		profile.imageFields = inlineImageFields
		profile.requestSecrets = inlineImageSecrets
	}
	return profile, nil
}

// profiles builds the contracts of every model the adapter may serve.
//
// The default model comes from the configuration, and a profile that names
// it replaces its capabilities. A profile for any other model adds that
// model to the adapter.
func (c SystemOneConfig) profiles(defaultProfile systemOneProfile) (map[string]systemOneProfile, error) {
	profiles := map[string]systemOneProfile{}
	if defaultProfile.model != "" {
		profiles[defaultProfile.model] = defaultProfile
	}
	seen := make(map[string]bool, len(c.Profiles))
	for _, entry := range c.Profiles {
		if entry.Model == "" {
			return nil, errors.New("lebro: a System One model profile requires a model")
		}
		if seen[entry.Model] {
			// Two profiles for one model would silently overwrite each
			// other, and the first one's contract would be lost.
			return nil, fmt.Errorf("lebro: the System One model %q has more than one profile", entry.Model)
		}
		seen[entry.Model] = true
		capabilities := entry.Capabilities
		if err := capabilities.Validate(); err != nil {
			return nil, err
		}
		if err := checkSystemOneCapabilities(capabilities); err != nil {
			return nil, err
		}
		capabilities.RetryableStatuses = append([]int(nil), entry.Capabilities.RetryableStatuses...)
		capabilities.Images.MediaTypes = append([]string(nil), entry.Capabilities.Images.MediaTypes...)
		profile := defaultProfile
		profile.model = entry.Model
		profile.capabilities = capabilities
		if c.ImageStyle != SystemOneImagesInline {
			// The wire shape is the route's, so a model cannot enable an
			// input the route cannot encode.
			profile.capabilities.Images = lebro.DecisionImageCapabilities{}
		}
		profile.scoreWeightedField = entry.Score.Weighted
		profile.scoreWinnerField = entry.Score.Winner
		if profile.scoreWeightedField == "" {
			profile.scoreWeightedField = defaultScoreWeightedField
		}
		if profile.scoreWinnerField != "" && profile.scoreWinnerField == profile.scoreWeightedField {
			return nil, errors.New("lebro: the score weighted and winner fields must differ")
		}
		if profile.capabilities.Images.Enabled {
			profile.imageFields = inlineImageFields
			profile.requestSecrets = inlineImageSecrets
		} else {
			profile.imageFields = nil
			profile.requestSecrets = nil
		}
		profile.buildQuestions = questionBuilder(profile.capabilities)
		profiles[entry.Model] = profile
	}
	return profiles, nil
}

// capabilities returns the configured capabilities with the copies and the
// image style applied.
func (c SystemOneConfig) capabilities() lebro.DecisionCapabilities {
	capabilities := c.Capabilities
	capabilities.RetryableStatuses = append([]int(nil), c.Capabilities.RetryableStatuses...)
	capabilities.Images.MediaTypes = append([]string(nil), c.Capabilities.Images.MediaTypes...)
	if c.ImageStyle != SystemOneImagesInline {
		// A shape this package does not implement leaves image inputs off,
		// so a request that carries one fails locally.
		capabilities.Images = lebro.DecisionImageCapabilities{}
	}
	return capabilities
}

// questionBuilder returns the renderer for the question map.
//
// The renderer follows the capabilities it is given, so a model whose profile
// accepts structured instructions is not answered with the text-only shape.
func questionBuilder(capabilities lebro.DecisionCapabilities) func(map[lebro.DecisionQuestionID]lebro.DecisionQuestion) (map[string]any, error) {
	if !capabilities.StructuredInstructions {
		return buildTextOnlyQuestions
	}
	return buildSystemOneQuestions
}

// tolerance returns the largest deviation a reported probability
// distribution may have from a total of one.
//
// Zero means the bound follows the number of outcomes, which is what a
// provider that rounds its probabilities needs.
func (c SystemOneConfig) tolerance() (float64, error) {
	if math.IsNaN(c.DistributionTolerance) || math.IsInf(c.DistributionTolerance, 0) {
		return 0, errors.New("lebro: the distribution tolerance must be a finite number")
	}
	if c.DistributionTolerance < 0 {
		return 0, errors.New("lebro: the distribution tolerance must not be negative")
	}
	return c.DistributionTolerance, nil
}

// checkSystemOneCapabilities rejects a capability the System One protocol
// cannot honor.
//
// The System One answer shape has no refusal, so a profile that advertises
// one would promise an answer the adapter cannot produce.
func checkSystemOneCapabilities(capabilities lebro.DecisionCapabilities) error {
	if capabilities.NoulUncertainty.Refusal ||
		capabilities.ChoiceUncertainty.Refusal ||
		capabilities.ScoreUncertainty.Refusal {
		return errors.New("lebro: the System One protocol has no refusal answer, so a profile must not advertise one")
	}
	return nil
}

// inlineImageFields renders the images in a top-level "images" array of
// {mediaType, base64} objects.
func inlineImageFields(request lebro.DecisionRequest) map[string]any {
	if len(request.Images) == 0 {
		return nil
	}
	images := make([]any, 0, len(request.Images))
	for _, image := range request.Images {
		images = append(images, map[string]any{
			"mediaType": image.MediaType,
			"base64":    encodeImageBase64(image.Bytes),
		})
	}
	return map[string]any{"images": images}
}

// inlineImageSecrets returns the encoded image payloads, which are request
// content and must never reach an error message.
func inlineImageSecrets(request lebro.DecisionRequest) []string {
	if len(request.Images) == 0 {
		return nil
	}
	secrets := make([]string, 0, len(request.Images))
	for _, image := range request.Images {
		secrets = append(secrets, encodeImageBase64(image.Bytes))
	}
	return secrets
}
