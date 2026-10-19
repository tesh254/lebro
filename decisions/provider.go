package decisions

import (
	"context"
	"errors"
	"net/http"
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
	// carries the image limits, and StructuredInstructions selects whether
	// instructions and criteria may be structured.
	Capabilities lebro.DecisionCapabilities
	// ImageStyle is the wire shape the provider uses for image inputs. A
	// shape this package does not implement leaves image inputs disabled.
	ImageStyle SystemOneImageStyle
	// RetryableStatuses are the HTTP statuses the provider documents as
	// retryable.
	RetryableStatuses []int
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
	client  *systemOneClient
	profile systemOneProfile
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
	baseURL := config.BaseURL
	if baseURL == "" {
		return nil, errors.New("lebro: a System One provider needs a base URL")
	}
	path := config.Path
	if path == "" {
		path = systemOnePath
	}
	authHeader := config.AuthHeader
	if authHeader == "" {
		authHeader = "Authorization"
	}
	client, err := newSystemOneClient(
		string(profile.provider),
		baseURL,
		path,
		authHeader,
		config.AuthPrefix,
		config.APIKey,
		config.Model,
		config.RetryableStatuses,
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
	return &SystemOneModel{client: client, profile: profile}, nil
}

// ProviderID identifies the adapter.
func (m *SystemOneModel) ProviderID() lebro.ProviderID {
	return lebro.ProviderID(m.profile.provider)
}

// Capabilities describes what the configured provider supports.
func (m *SystemOneModel) Capabilities() lebro.DecisionCapabilities {
	return m.profile.capabilities
}

// Decide evaluates the request with the configured provider and returns the
// typed answers.
func (m *SystemOneModel) Decide(ctx context.Context, request lebro.DecisionRequest) (lebro.DecisionResponse, error) {
	return decide(ctx, m.client, m.profile, request)
}

// profile turns the configuration into the internal provider contract.
func (c SystemOneConfig) profile() (systemOneProfile, error) {
	provider := string(c.ProviderID)
	if provider == "" {
		provider = defaultSystemOneProviderID
	}
	weighted := c.Score.Weighted
	if weighted == "" {
		weighted = defaultScoreWeightedField
	}
	if c.Score.Winner != "" && c.Score.Winner == weighted {
		return systemOneProfile{}, errors.New("lebro: the score weighted and winner fields must differ")
	}
	capabilities := c.Capabilities
	capabilities.RetryableStatuses = append([]int(nil), c.RetryableStatuses...)
	if c.ImageStyle != SystemOneImagesInline {
		// A shape this package does not implement leaves image inputs off,
		// so a request that carries one fails locally.
		capabilities.Images = lebro.DecisionImageCapabilities{}
	}
	buildQuestions := buildSystemOneQuestions
	if !capabilities.StructuredInstructions {
		buildQuestions = buildTextOnlyQuestions
	}
	profile := systemOneProfile{
		provider:           provider,
		capabilities:       capabilities,
		scoreWeightedField: weighted,
		scoreWinnerField:   c.Score.Winner,
		buildQuestions:     buildQuestions,
		extraFields: func(lebro.DecisionRequest) map[string]any {
			if !c.DisableStorage {
				return nil
			}
			return map[string]any{"store": false}
		},
		maxBodyBytes:     c.MaxBodyBytes,
		requestIDHeaders: append([]string(nil), c.RequestIDHeaders...),
	}
	if capabilities.Images.Enabled {
		profile.imageFields = inlineImageFields
		profile.requestSecrets = inlineImageSecrets
	}
	return profile, nil
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
