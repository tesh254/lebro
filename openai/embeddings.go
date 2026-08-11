package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/tesh254/lebro"
)

const (
	embeddingsPath           = "/embeddings"
	defaultEmbedderUserAgent = "lebro-openai-embeddings"
)

// EmbedderConfig describes an OpenAI-compatible embeddings endpoint. Model and
// Dimension are required; every other field defaults to a sensible value.
type EmbedderConfig struct {
	// BaseURL is the API root, for example "https://api.openai.com/v1". It
	// defaults to the public OpenAI endpoint.
	BaseURL string
	// APIKey is sent as the Bearer token. Required.
	APIKey string
	// Model is the embedding model id, for example "text-embedding-3-small".
	// Required: unlike chat completions, there is no meaningful default.
	Model string
	// Dimension is the vector length this model produces. It is required
	// because it determines the vector index dimension and must be known before
	// the first call; every response is checked against it, so a misconfigured
	// dimension surfaces as an error rather than as a corrupt index.
	Dimension int
	// RequestDimension sends the "dimensions" request parameter, asking the
	// provider to produce vectors of the configured Dimension. Set it for
	// models that support dimension reduction; leave it false for models and
	// gateways that reject the parameter.
	RequestDimension bool
	// HTTPClient issues requests. If nil a client with Timeout is used.
	HTTPClient *http.Client
	// Timeout caps each request when no earlier deadline is set on the context.
	// A zero value uses defaultTimeout when HTTPClient is also nil.
	Timeout time.Duration
	// UserAgent overrides the default User-Agent header.
	UserAgent string
	// Organization sets the optional OpenAI-Organization header.
	Organization string
}

// Embedder is a [lebro.EmbeddingModel] backed by an OpenAI-compatible
// embeddings endpoint. Use [NewEmbedder] to create instances.
//
// It is a text-embedding adapter only; it shares the chat adapter's error
// classification, so failures arrive as [*lebro.ModelError] with the same
// normalized kinds and callers can apply one retry policy across both.
type Embedder struct {
	baseURL          string
	apiKey           string
	model            string
	dimension        int
	requestDimension bool
	client           *http.Client
	timeout          time.Duration
	userAgent        string
	organization     string
}

var _ lebro.EmbeddingModel = (*Embedder)(nil)

// NewEmbedder builds an embeddings adapter for an OpenAI-compatible endpoint.
// The returned embedder is safe for concurrent use.
func NewEmbedder(config EmbedderConfig) (*Embedder, error) {
	if config.APIKey == "" {
		return nil, errors.New("lebro: API key is required")
	}
	if config.Model == "" {
		return nil, errors.New("lebro: embedding model is required")
	}
	if config.Dimension <= 0 {
		return nil, errors.New("lebro: embedding dimension must be positive")
	}

	baseURL := config.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("lebro: invalid base URL: %w", err)
	}
	if !parsed.IsAbs() {
		return nil, fmt.Errorf("lebro: base URL %q must be absolute", baseURL)
	}

	client := config.HTTPClient
	if client == nil {
		timeout := config.Timeout
		if timeout == 0 {
			timeout = defaultTimeout
		}
		client = &http.Client{Timeout: timeout}
	}

	userAgent := config.UserAgent
	if userAgent == "" {
		userAgent = defaultEmbedderUserAgent
	}

	return &Embedder{
		baseURL:          strings.TrimRight(baseURL, "/"),
		apiKey:           config.APIKey,
		model:            config.Model,
		dimension:        config.Dimension,
		requestDimension: config.RequestDimension,
		client:           client,
		timeout:          config.Timeout,
		userAgent:        userAgent,
		organization:     config.Organization,
	}, nil
}

// Dimension reports the configured vector length.
func (e *Embedder) Dimension() int {
	if e == nil {
		return 0
	}
	return e.dimension
}

// Embed returns one vector per input, in input order.
//
// The response is reordered by its declared index rather than trusted in wire
// order, because the API contract guarantees an index per item but not the
// order of the data array. The returned count and every vector's length are
// checked against the request, so a provider that silently drops or truncates
// an item fails loudly instead of writing a misaligned index.
//
// Failures are returned as [*lebro.ModelError] with a normalized kind; context
// cancellation is returned directly so errors.Is(err, context.Canceled) holds.
func (e *Embedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if e == nil {
		return nil, errors.New("lebro: embedder is nil")
	}
	if ctx == nil {
		return nil, errors.New("lebro: embedder context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(inputs) == 0 {
		return nil, e.invalidRequest("lebro: at least one embedding input is required", nil)
	}
	for i, input := range inputs {
		if input == "" {
			return nil, e.invalidRequest(fmt.Sprintf("lebro: embedding input %d is empty", i), nil)
		}
	}

	body := map[string]any{"model": e.model, "input": inputs}
	if e.requestDimension {
		body["dimensions"] = e.dimension
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, e.invalidRequest(fmt.Sprintf("lebro: encode request: %v", err), err)
	}

	reqCtx, cancel := e.requestContext(ctx)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, e.baseURL+embeddingsPath, bytes.NewReader(encoded))
	if err != nil {
		return nil, e.invalidRequest(err.Error(), err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+e.apiKey)
	httpReq.Header.Set("User-Agent", e.userAgent)
	if e.organization != "" {
		httpReq.Header.Set("OpenAI-Organization", e.organization)
	}

	resp, err := e.client.Do(httpReq)
	if err != nil {
		return nil, e.classifyTransportError(reqCtx, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, e.classifyResponseError(resp)
	}

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, e.classifyTransportError(reqCtx, err)
	}

	var parsed embeddingResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, e.malformedResponse(fmt.Sprintf("lebro: decode response: %v", err), err)
	}
	return e.mapEmbeddings(parsed, len(inputs))
}

// mapEmbeddings validates the response shape and returns vectors in input
// order.
func (e *Embedder) mapEmbeddings(parsed embeddingResponse, want int) ([][]float32, error) {
	if len(parsed.Data) != want {
		return nil, e.malformedResponse(fmt.Sprintf("lebro: response has %d embeddings for %d inputs", len(parsed.Data), want), nil)
	}

	data := make([]embeddingData, len(parsed.Data))
	copy(data, parsed.Data)
	sort.SliceStable(data, func(i, j int) bool { return data[i].Index < data[j].Index })

	vectors := make([][]float32, 0, len(data))
	for position, item := range data {
		// After sorting, position i must hold index i. Anything else means the
		// provider returned duplicate or out-of-range indices, which would
		// silently misalign vectors with their inputs.
		if item.Index != position {
			return nil, e.malformedResponse(fmt.Sprintf("lebro: response embedding index %d is out of order at position %d", item.Index, position), nil)
		}
		if len(item.Embedding) != e.dimension {
			return nil, e.malformedResponse(fmt.Sprintf("lebro: embedding %d has dimension %d, want %d", position, len(item.Embedding), e.dimension), nil)
		}
		vectors = append(vectors, item.Embedding)
	}
	return vectors, nil
}

func (e *Embedder) requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if e.timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, e.timeout)
}

// classifyResponseError mirrors the chat adapter's HTTP error mapping so one
// retry policy covers both adapters.
func (e *Embedder) classifyResponseError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	var parsed chatErrorBody
	_ = json.Unmarshal(body, &parsed)

	modelErr := &lebro.ModelError{
		Kind:       statusToKind(resp.StatusCode),
		Provider:   providerName,
		Code:       parsed.Error.Code,
		StatusCode: resp.StatusCode,
		Message:    parsed.Error.Message,
	}
	if modelErr.Message == "" {
		modelErr.Message = fmt.Sprintf("lebro: HTTP %d", resp.StatusCode)
	}
	if parsed.Error.Type != "" || parsed.Error.Param != "" {
		extension, _ := json.Marshal(map[string]string{"type": parsed.Error.Type, "param": parsed.Error.Param})
		modelErr.Extension = extension
	}
	if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After")); retryAfter > 0 {
		modelErr.RetryAfter = retryAfter
	}
	return modelErr
}

func (e *Embedder) classifyTransportError(ctx context.Context, err error) error {
	// The chat adapter's classifier is reused through a zero-value Model: it
	// reads no receiver state beyond the provider name, which both adapters
	// share, so the classification stays identical across the two.
	return (&Model{}).classifyTransportError(ctx, err)
}

func (e *Embedder) invalidRequest(message string, err error) error {
	return &lebro.ModelError{Kind: lebro.ModelErrorInvalidRequest, Provider: providerName, Message: message, Err: err}
}

func (e *Embedder) malformedResponse(message string, err error) error {
	return &lebro.ModelError{Kind: lebro.ModelErrorMalformedResponse, Provider: providerName, Message: message, Err: err}
}

type embeddingResponse struct {
	Model string          `json:"model"`
	Data  []embeddingData `json:"data"`
	Usage embeddingUsage  `json:"usage"`
}

type embeddingData struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

type embeddingUsage struct {
	PromptTokens int64 `json:"prompt_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}
