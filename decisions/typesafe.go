package decisions

import (
	"net/http"
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

// NewTypeSafe builds a TypeSafe Jev decision adapter.
//
// The adapter is a [SystemOneModel] configured with TypeSafe's documented
// contract. Point it at a gateway that fronts Jev by setting BaseURL and
// Model.
func NewTypeSafe(config TypeSafeConfig) (*SystemOneModel, error) {
	baseURL := config.BaseURL
	if baseURL == "" {
		baseURL = typeSafeBaseURL
	}
	model := config.Model
	if model == "" {
		model = typeSafeModel
	}
	return NewSystemOne(SystemOneConfig{
		APIKey:     config.APIKey,
		BaseURL:    baseURL,
		AuthHeader: "Authorization",
		AuthPrefix: "Bearer ",
		ProviderID: lebro.ProviderID(typeSafeProvider),
		Model:      model,
		Score:      SystemOneScoreFields{Weighted: "score"},
		Capabilities: lebro.DecisionCapabilities{
			Noul:                   true,
			Choice:                 true,
			Score:                  true,
			MultipleQuestions:      true,
			StructuredState:        true,
			StructuredInstructions: true,
			Distributions:          true,
			Confidence:             true,
			Batching:               false,
			// TypeSafe documents a request body limit, not a byte limit for
			// the state alone. The body limit is enforced separately.
			MaxStateBytes:     0,
			MaxQuestions:      0,
			MaxChoiceOptions:  255,
			RetryableStatuses: typeSafeRetryableStatuses,
			MaxScoreLevels:    10,
		},
		ImageStyle:       SystemOneImagesNone,
		MaxBodyBytes:     typeSafeMaxBodyBytes,
		RequestIDHeaders: []string{typeSafeRequestIDHeader, typeSafeGatewayRequestIDHeader},
		HTTPClient:       config.HTTPClient,
		Timeout:          config.Timeout,
		MaxRetries:       config.MaxRetries,
		RetryBackoff:     config.RetryBackoff,
	})
}
