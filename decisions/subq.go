package decisions

import (
	"context"
	"net/http"
	"slices"
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

// SubQModel adapts SubQ Snap to [github.com/tesh254/lebro.DecisionModel]. Use
// [NewSubQ] to create instances. The adapter is safe for concurrent use.
type SubQModel struct {
	client  *systemOneClient
	profile systemOneProfile
}

var _ lebro.DecisionModel = (*SubQModel)(nil)

// NewSubQ builds a SubQ Snap decision adapter.
func NewSubQ(config SubQConfig) (*SubQModel, error) {
	baseURL := config.BaseURL
	if baseURL == "" {
		baseURL = subQBaseURL
	}
	model := config.Model
	if model == "" {
		model = subQModel
	}
	client, err := newSystemOneClient(
		subQProvider,
		baseURL,
		systemOnePath,
		"Authorization",
		"Bearer ",
		config.APIKey,
		model,
		subQRetryableStatuses,
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
	return &SubQModel{
		client: client,
		profile: systemOneProfile{
			provider:     subQProvider,
			capabilities: subQCapabilities(),
			scoreMapping: scoreWeightedOnly,
			buildQuestions: func(questions map[lebro.DecisionQuestionID]lebro.DecisionQuestion) (map[string]any, error) {
				return buildSystemOneQuestions(questions)
			},
			extraFields:    subQExtraFields,
			requestSecrets: subQRequestSecrets,
			maxBodyBytes:   subQMaxBodyBytes,
		},
	}, nil
}

// ProviderID identifies the adapter.
func (m *SubQModel) ProviderID() lebro.ProviderID {
	return lebro.ProviderID(subQProvider)
}

// Capabilities describes what SubQ supports, including the image inputs it
// accepts.
func (m *SubQModel) Capabilities() lebro.DecisionCapabilities {
	return subQCapabilities()
}

// Decide evaluates the request with Snap and returns the typed answers.
//
// Image bytes are encoded for the wire here and nowhere else. A request that
// carries an unsupported media type, too many images, or images beyond the
// documented size limits fails with a typed error before anything is sent.
func (m *SubQModel) Decide(ctx context.Context, request lebro.DecisionRequest) (lebro.DecisionResponse, error) {
	return decide(ctx, m.client, m.profile, request)
}

// subQExtraFields renders the provider-specific top-level request fields.
func subQExtraFields(request lebro.DecisionRequest) map[string]any {
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

// subQRequestSecrets returns the encoded image payloads. They are request
// content, so a provider error must not echo them back.
func subQRequestSecrets(request lebro.DecisionRequest) []string {
	if len(request.Images) == 0 {
		return nil
	}
	secrets := make([]string, 0, len(request.Images))
	for _, image := range request.Images {
		secrets = append(secrets, encodeImageBase64(image.Bytes))
	}
	return secrets
}

// subQCapabilities describes the SubQ decision surface.
//
// SubQ reports a probability-weighted score and no winning level, so a score
// answer keeps an expected level and no selected level. It stores decision
// inputs, including images, under the organization's retention policy.
func subQCapabilities() lebro.DecisionCapabilities {
	return lebro.DecisionCapabilities{
		Noul:                   true,
		Choice:                 true,
		Score:                  true,
		MultipleQuestions:      true,
		StructuredState:        true,
		StructuredInstructions: true,
		Images: lebro.DecisionImageCapabilities{
			Enabled:          true,
			MediaTypes:       []string{"image/png", "image/jpeg", "image/webp"},
			MaxImages:        subQMaxImages,
			MaxBytesPerImage: subQMaxBytesPerImage,
			MaxTotalBytes:    subQMaxTotalBytes,
		},
		MaxQuestions:     16,
		MaxChoiceOptions: 255,
		MaxScoreLevels:   10,
		Distributions:    true,
		Confidence:       true,
		Batching:         false,
		// SubQ documents a context window, not a byte limit for the state
		// alone. The request body limit is enforced separately.
		MaxStateBytes:     0,
		RetryableStatuses: slices.Clone(subQRetryableStatuses),
	}
}
