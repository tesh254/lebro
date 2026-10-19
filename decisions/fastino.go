package decisions

import (
	"net/http"
	"time"

	"github.com/tesh254/lebro"
)

const (
	fastinoProvider = "fastino"
	fastinoBaseURL  = "https://api.fastino.ai"
	fastinoModel    = "fastino/GLiDE"
)

// fastinoRetryableStatuses are the HTTP status codes Fastino documents as
// retryable. The client and the advertised capability share this list so the
// two cannot drift apart.
var fastinoRetryableStatuses = []int{http.StatusTooEarly, http.StatusTooManyRequests, http.StatusServiceUnavailable}

// FastinoConfig configures the Fastino GLiDE decision adapter. APIKey is
// required; every other field has a documented default.
type FastinoConfig struct {
	// APIKey is the Fastino credential. It is sent in the X-API-Key header
	// and is never logged, returned, or included in an error.
	APIKey string
	// BaseURL is the API root. It defaults to the public Fastino endpoint.
	BaseURL string
	// Model is the model used when a request does not name one. It defaults
	// to "fastino/GLiDE".
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
	// DisableStorage keeps Fastino from storing the request and its answer.
	// It is false by default, which leaves the provider default in place.
	DisableStorage bool
}

// NewFastino builds a Fastino GLiDE decision adapter.
//
// The adapter is a [SystemOneModel] configured with Fastino's documented
// contract. GLiDE reports a winning score level and a separate
// probability-weighted value, and the adapter keeps the two apart.
func NewFastino(config FastinoConfig) (*SystemOneModel, error) {
	baseURL := config.BaseURL
	if baseURL == "" {
		baseURL = fastinoBaseURL
	}
	model := config.Model
	if model == "" {
		model = fastinoModel
	}
	return NewSystemOne(SystemOneConfig{
		APIKey:     config.APIKey,
		BaseURL:    baseURL,
		AuthHeader: "X-API-Key",
		ProviderID: lebro.ProviderID(fastinoProvider),
		Model:      model,
		Score:      SystemOneScoreFields{Weighted: "expected_level", Winner: "score"},
		Capabilities: lebro.DecisionCapabilities{
			Noul:                   true,
			Choice:                 true,
			Score:                  true,
			MultipleQuestions:      true,
			StructuredState:        true,
			StructuredInstructions: false,
			Distributions:          true,
			Confidence:             true,
			Batching:               false,
			MaxStateBytes:          0,
			MaxQuestions:           0,
			MaxChoiceOptions:       255,
			MaxScoreLevels:         255,
		},
		ImageStyle:        SystemOneImagesNone,
		RetryableStatuses: fastinoRetryableStatuses,
		DisableStorage:    config.DisableStorage,
		HTTPClient:        config.HTTPClient,
		Timeout:           config.Timeout,
		MaxRetries:        config.MaxRetries,
		RetryBackoff:      config.RetryBackoff,
	})
}
