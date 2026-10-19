package decisions

import (
	"context"
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

// FastinoModel adapts Fastino GLiDE to [github.com/tesh254/lebro.DecisionModel].
// Use [NewFastino] to create instances. The adapter is safe for concurrent
// use.
type FastinoModel struct {
	client  *systemOneClient
	profile systemOneProfile
}

var _ lebro.DecisionModel = (*FastinoModel)(nil)

// NewFastino builds a Fastino GLiDE decision adapter.
func NewFastino(config FastinoConfig) (*FastinoModel, error) {
	baseURL := config.BaseURL
	if baseURL == "" {
		baseURL = fastinoBaseURL
	}
	model := config.Model
	if model == "" {
		model = fastinoModel
	}
	client, err := newSystemOneClient(
		fastinoProvider,
		baseURL,
		systemOnePath,
		"X-API-Key",
		"",
		config.APIKey,
		model,
		fastinoRetryableStatuses,
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
	disableStorage := config.DisableStorage
	return &FastinoModel{
		client: client,
		profile: systemOneProfile{
			provider:     fastinoProvider,
			capabilities: fastinoCapabilities(),
			scoreMapping: scoreWinnerAndExpectation,
			buildQuestions: func(questions map[lebro.DecisionQuestionID]lebro.DecisionQuestion) (map[string]any, error) {
				return buildTextOnlyQuestions(questions)
			},
			extraFields: func(lebro.DecisionRequest) map[string]any {
				if !disableStorage {
					return nil
				}
				return map[string]any{"store": false}
			},
		},
	}, nil
}

// ProviderID identifies the adapter.
func (m *FastinoModel) ProviderID() lebro.ProviderID {
	return lebro.ProviderID(fastinoProvider)
}

// Capabilities describes what Fastino supports. Image inputs are not
// advertised: this adapter does not expose them.
func (m *FastinoModel) Capabilities() lebro.DecisionCapabilities {
	return fastinoCapabilities()
}

// Decide evaluates the request with GLiDE and returns the typed answers.
//
// GLiDE accepts text instructions and text criteria only. A request that
// carries structured instructions or criteria fails with an invalid-request
// error before anything is sent, because GLiDE does not describe a structured
// form for them.
func (m *FastinoModel) Decide(ctx context.Context, request lebro.DecisionRequest) (lebro.DecisionResponse, error) {
	return decide(ctx, m.client, m.profile, request)
}

// fastinoCapabilities describes the Fastino decision surface.
//
// GLiDE reports a winning score level and a separate probability-weighted
// value, and both are kept. It reports a confidence for every answer type.
func fastinoCapabilities() lebro.DecisionCapabilities {
	return lebro.DecisionCapabilities{
		Noul:                   true,
		Choice:                 true,
		Score:                  true,
		MultipleQuestions:      true,
		StructuredState:        true,
		StructuredInstructions: false,
		Images: lebro.DecisionImageCapabilities{
			Enabled: false,
		},
		MaxQuestions:      0,
		MaxChoiceOptions:  255,
		MaxScoreLevels:    255,
		Distributions:     true,
		Confidence:        true,
		Batching:          false,
		MaxStateBytes:     0,
		RetryableStatuses: fastinoRetryableStatuses,
	}
}
