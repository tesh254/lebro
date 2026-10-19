package decisions

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/tesh254/lebro"
)

const (
	typeSafeProvider = "typesafe"
	typeSafeBaseURL  = "https://api.typesafe.ai"
	typeSafeModel    = "jev-latest"
	// typeSafeRequestIDHeader carries the provider request identifier.
	typeSafeRequestIDHeader = "x-typesafe-request-id"
	// typeSafeGatewayRequestIDHeader carries the request identifier when the
	// adapter is pointed at a gateway that fronts TypeSafe, such as
	// OpenRouter.
	typeSafeGatewayRequestIDHeader = "x-generation-id"
	// typeSafeMaxBodyBytes is the largest request body TypeSafe accepts.
	typeSafeMaxBodyBytes = 32 << 20
)

// typeSafeRetryableStatuses are the HTTP status codes TypeSafe documents as
// retryable. The client and the advertised capability share this list so the
// two cannot drift apart.
var typeSafeRetryableStatuses = []int{http.StatusTooManyRequests, 529}

// TypeSafeConfig configures the TypeSafe Jev decision adapter. APIKey is
// required; every other field has a documented default.
type TypeSafeConfig struct {
	// APIKey is the TypeSafe credential. It is sent as a bearer token and is
	// never logged, returned, or included in an error.
	APIKey string
	// BaseURL is the API root. It defaults to the public TypeSafe endpoint.
	BaseURL string
	// Model is the model used when a request does not name one. It defaults
	// to the Jev alias "jev-latest".
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

// TypeSafeModel adapts TypeSafe Jev to [github.com/tesh254/lebro.DecisionModel].
// Use [NewTypeSafe] to create instances. The adapter is safe for concurrent
// use.
type TypeSafeModel struct {
	client  *systemOneClient
	profile systemOneProfile
}

var _ lebro.DecisionModel = (*TypeSafeModel)(nil)

// NewTypeSafe builds a TypeSafe Jev decision adapter.
func NewTypeSafe(config TypeSafeConfig) (*TypeSafeModel, error) {
	baseURL := config.BaseURL
	if baseURL == "" {
		baseURL = typeSafeBaseURL
	}
	model := config.Model
	if model == "" {
		model = typeSafeModel
	}
	client, err := newSystemOneClient(
		typeSafeProvider,
		baseURL,
		systemOnePath,
		"Authorization",
		"Bearer ",
		config.APIKey,
		model,
		typeSafeRetryableStatuses,
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
	return &TypeSafeModel{
		client: client,
		profile: systemOneProfile{
			provider:         typeSafeProvider,
			capabilities:     typeSafeCapabilities(),
			scoreMapping:     scoreWeightedOnly,
			buildQuestions:   buildSystemOneQuestions,
			requestIDHeaders: []string{typeSafeRequestIDHeader, typeSafeGatewayRequestIDHeader},
			maxBodyBytes:     typeSafeMaxBodyBytes,
		},
	}, nil
}

// ProviderID identifies the adapter.
func (m *TypeSafeModel) ProviderID() lebro.ProviderID {
	return lebro.ProviderID(typeSafeProvider)
}

// Capabilities describes what TypeSafe supports. Image inputs are not
// advertised: this adapter does not expose them.
func (m *TypeSafeModel) Capabilities() lebro.DecisionCapabilities {
	return typeSafeCapabilities()
}

// Decide evaluates the request with Jev and returns the typed answers.
func (m *TypeSafeModel) Decide(ctx context.Context, request lebro.DecisionRequest) (lebro.DecisionResponse, error) {
	return decide(ctx, m.client, m.profile, request)
}

// typeSafeCapabilities describes the TypeSafe decision surface.
//
// TypeSafe reports a probability-weighted score and no winning level, so a
// score answer keeps an expected level and no selected level. Confidence is
// reported for choice and score answers; a noul answer carries none.
func typeSafeCapabilities() lebro.DecisionCapabilities {
	return lebro.DecisionCapabilities{
		Noul:                   true,
		Choice:                 true,
		Score:                  true,
		MultipleQuestions:      true,
		StructuredState:        true,
		StructuredInstructions: true,
		Images: lebro.DecisionImageCapabilities{
			Enabled: false,
		},
		MaxQuestions:     0,
		MaxChoiceOptions: 255,
		MaxScoreLevels:   10,
		Distributions:    true,
		Confidence:       true,
		Batching:         false,
		// TypeSafe documents a request body limit, not a byte limit for the
		// state alone. The body limit is enforced separately.
		MaxStateBytes:     0,
		RetryableStatuses: slices.Clone(typeSafeRetryableStatuses),
	}
}
