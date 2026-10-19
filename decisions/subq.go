package decisions

import (
	"net/http"
	"time"

	"github.com/tesh254/lebro"
)

const (
	subQProvider = "subq"
	subQBaseURL  = "https://api.subq.ai"
	subQModel    = "subq-snap-preview"
	// subQMaxImages is the largest number of images one request may carry.
	subQMaxImages = 4
	// subQMaxBytesPerImage is the largest decoded size of one image.
	subQMaxBytesPerImage = 4 << 20
	// subQMaxTotalBytes is the largest decoded size of all images together.
	subQMaxTotalBytes = 8 << 20
	// subQMaxBodyBytes is the largest request body SubQ accepts.
	subQMaxBodyBytes = 13 << 20
)

// subQRetryableStatuses are the HTTP status codes SubQ documents as
// retryable. The client and the advertised capability share this list so the
// two cannot drift apart.
var subQRetryableStatuses = []int{http.StatusTooManyRequests, http.StatusServiceUnavailable}

// SubQConfig configures the SubQ Snap decision adapter. APIKey is required;
// every other field has a documented default.
type SubQConfig struct {
	// APIKey is the SubQ credential. It is sent as a bearer token and is
	// never logged, returned, or included in an error.
	APIKey string
	// BaseURL is the API root. It defaults to the public SubQ endpoint.
	BaseURL string
	// Model is the model used when a request does not name one. It defaults
	// to "subq-snap-preview".
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

// NewSubQ builds a SubQ Snap decision adapter.
//
// The adapter is a [SystemOneModel] configured with SubQ's documented
// contract, including its image inputs: raw PNG, JPEG, or WebP bytes are
// encoded as one line of base64 at the HTTP boundary only.
func NewSubQ(config SubQConfig) (*SystemOneModel, error) {
	baseURL := config.BaseURL
	if baseURL == "" {
		baseURL = subQBaseURL
	}
	model := config.Model
	if model == "" {
		model = subQModel
	}
	return NewSystemOne(SystemOneConfig{
		APIKey:     config.APIKey,
		BaseURL:    baseURL,
		AuthHeader: "Authorization",
		AuthPrefix: "Bearer ",
		ProviderID: lebro.ProviderID(subQProvider),
		Model:      model,
		Score:      SystemOneScoreFields{Weighted: "score"},
		Capabilities: lebro.DecisionCapabilities{
			Noul:                   true,
			Choice:                 true,
			Score:                  true,
			MultipleQuestions:      true,
			StructuredState:        true,
			StructuredInstructions: true,
			Batching:               false,
			// SubQ reports a probability for every option and a confidence on
			// every choice and score answer, and a noul answer carries
			// neither. SubQ documents that it exposes a confidence but not
			// what the number means, so the metric stays unknown: the value
			// is there and it cannot be compared with another provider's.
			NoulUncertainty: lebro.DecisionUncertainty{
				Distribution: lebro.DecisionAvailabilityUnavailable,
				Confidence:   lebro.DecisionAvailabilityUnavailable,
			},
			ChoiceUncertainty: lebro.DecisionUncertainty{
				Distribution:     lebro.DecisionAvailabilityRequired,
				Confidence:       lebro.DecisionAvailabilityRequired,
				ConfidenceMetric: lebro.DecisionConfidenceUnknown,
			},
			ScoreUncertainty: lebro.DecisionUncertainty{
				Distribution:     lebro.DecisionAvailabilityRequired,
				Confidence:       lebro.DecisionAvailabilityRequired,
				ConfidenceMetric: lebro.DecisionConfidenceUnknown,
			},
			Images: lebro.DecisionImageCapabilities{
				Enabled:          true,
				MediaTypes:       []string{"image/png", "image/jpeg", "image/webp"},
				MaxImages:        subQMaxImages,
				MaxBytesPerImage: subQMaxBytesPerImage,
				MaxTotalBytes:    subQMaxTotalBytes,
			},
			// SubQ documents a context window, not a byte limit for the
			// state alone. The request body limit is enforced separately.
			MaxStateBytes:     0,
			MaxQuestions:      16,
			MaxChoiceOptions:  255,
			RetryableStatuses: subQRetryableStatuses,
			MaxScoreLevels:    10,
		},
		ImageStyle:   SystemOneImagesInline,
		MaxBodyBytes: subQMaxBodyBytes,
		HTTPClient:   config.HTTPClient,
		Timeout:      config.Timeout,
		MaxRetries:   config.MaxRetries,
		RetryBackoff: config.RetryBackoff,
	})
}
